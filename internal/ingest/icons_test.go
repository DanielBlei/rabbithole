// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/png"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/httpclient"
	"github.com/DanielBlei/rabbithole/internal/profile"
	"github.com/DanielBlei/rabbithole/internal/store"
)

var iconNow = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

// iconPNG is a small solid PNG, the icon the fake sites serve.
func iconPNG() []byte {
	img := image.NewNRGBA(image.Rect(0, 0, 32, 32))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = 200, 30, 30, 255
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		panic(err)
	}
	return b.Bytes()
}

// iconHosts is one server standing in for many sites: requests are told apart
// by Host, and a client from client() sends every connection to it. Each site
// serves an empty head page and an icon at /favicon.ico unless configured.
type iconHosts struct {
	*httptest.Server
	release chan struct{}

	mu      sync.Mutex
	hits    map[string]int // "host/path"
	cur     int            // page requests in flight
	peak    int
	hold    map[string]bool // hosts whose pages never answer
	noIcon  map[string]bool // hosts whose /favicon.ico is a 404
	started chan string     // a host's page request arrived and is being held

	gate     chan struct{} // when set, every page waits for it to close
	gateOnce sync.Once
	relOnce  sync.Once
}

func newIconHosts(t *testing.T) *iconHosts {
	t.Helper()
	h := &iconHosts{
		hits: map[string]int{}, hold: map[string]bool{}, noIcon: map[string]bool{},
		release: make(chan struct{}), started: make(chan string, 256),
	}
	h.Server = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.Close)
	t.Cleanup(h.open)
	t.Cleanup(h.openGate)
	return h
}

// open releases the held pages, which then answer with an empty head.
func (h *iconHosts) open() { h.relOnce.Do(func() { close(h.release) }) }

// openGate lets the gated pages through.
func (h *iconHosts) openGate() {
	h.gateOnce.Do(func() {
		if h.gate != nil {
			close(h.gate)
		}
	})
}

func (h *iconHosts) serve(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if hn, _, err := net.SplitHostPort(host); err == nil {
		host = hn
	}
	page := r.URL.Path == "/"
	h.mu.Lock()
	h.hits[host+r.URL.Path]++
	if page {
		h.cur++
		h.peak = max(h.peak, h.cur)
	}
	hold, noIcon := h.hold[host], h.noIcon[host]
	h.mu.Unlock()
	if page {
		defer func() {
			h.mu.Lock()
			h.cur--
			h.mu.Unlock()
		}()
	}

	if hold && page {
		select {
		case h.started <- host:
		default:
		}
		select {
		case <-r.Context().Done():
		case <-h.release:
		}
		return
	}
	if page && h.gate != nil {
		select {
		case h.started <- host:
		default:
		}
		select {
		case <-h.gate:
		case <-r.Context().Done():
			return
		}
	}
	switch {
	case page:
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><head></head><body></body></html>"))
	case r.URL.Path == "/favicon.ico" && !noIcon:
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(iconPNG())
	default:
		http.NotFound(w, r)
	}
}

func (h *iconHosts) client() *http.Client {
	addr := h.Listener.Addr().String()
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}}
}

func (h *iconHosts) count(host string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for k, v := range h.hits {
		if strings.HasPrefix(k, host+"/") {
			n += v
		}
	}
	return n
}

func (h *iconHosts) total() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, v := range h.hits {
		n += v
	}
	return n
}

func (h *iconHosts) peakPages() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.peak
}

// withIconBounds overrides the pass bounds for one test.
func withIconBounds(t *testing.T, sites, conc int, pass time.Duration) {
	t.Helper()
	s, c, p := iconMaxSites, iconConcurrency, iconPassTimeout
	iconMaxSites, iconConcurrency, iconPassTimeout = sites, conc, pass
	t.Cleanup(func() { iconMaxSites, iconConcurrency, iconPassTimeout = s, c, p })
}

func iconDB(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "icons.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// feedAt is a site entry for a feed at feedURL whose site is siteURL.
func feedAt(feedURL, siteURL string) IconSite {
	return IconSite{FeedID: config.FeedID(feedURL), FeedURL: feedURL, SiteURL: siteURL}
}

// fallbackAt is feedAt for a site taken from the feed URL's own origin.
func fallbackAt(feedURL, siteURL string) IconSite {
	s := feedAt(feedURL, siteURL)
	s.Fallback = true
	return s
}

func iconRow(t *testing.T, db *store.Store, id string) (store.FeedIcon, bool) {
	t.Helper()
	meta, err := db.FeedIconMeta(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	row, ok := meta[id]
	return row, ok
}

func TestRefreshIconsRecordsAnIcon(t *testing.T) {
	h := newIconHosts(t)
	db := iconDB(t)
	s := feedAt("http://feeds.one.test/rss", "http://one.test/")

	stats := RefreshIcons(context.Background(), db, h.client(), []IconSite{s}, iconNow)
	if stats.Stale != 1 || stats.OK != 1 || stats.Deferred != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	got, err := db.FeedIconBytes(context.Background(), s.FeedID)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(got.Bytes)
	wantETag := `"` + hex.EncodeToString(sum[:])[:16] + `"`
	if got.Status != store.IconStatusOK || got.Mime != "image/png" || got.ETag != wantETag {
		t.Errorf("row = %+v, want ok image/png etag %s", got, wantETag)
	}
	if !regexp.MustCompile(`^"[0-9a-f]{16}"$`).MatchString(got.ETag) {
		t.Errorf("etag %q is not a quoted hex prefix", got.ETag)
	}
	if got.FeedURL != s.FeedURL || got.SiteHost != "one.test" || got.Dark || !got.FetchedAt.Equal(iconNow) {
		t.Errorf("row = %+v", got)
	}
	if _, err := png.Decode(bytes.NewReader(got.Bytes)); err != nil {
		t.Errorf("stored bytes are not a PNG: %v", err)
	}
}

func TestRefreshIconsOneFetchPerSiteKey(t *testing.T) {
	h := newIconHosts(t)
	db := iconDB(t)
	sites := []IconSite{
		feedAt("http://same.test/a", "http://same.test/"),
		feedAt("http://same.test/b", "http://www.same.test/blog"),
		feedAt("http://same.test/c", "https://SAME.test/other"),
	}
	stats := RefreshIcons(context.Background(), db, h.client(), sites, iconNow)
	if stats.Stale != 1 || stats.OK != 1 {
		t.Fatalf("stats = %+v, want one site", stats)
	}
	if n := h.count("same.test") + h.count("www.same.test"); n != 2 {
		t.Errorf("%d requests, want the page and the icon once", n)
	}
	for _, s := range sites {
		row, ok := iconRow(t, db, s.FeedID)
		if !ok || row.Status != store.IconStatusOK || row.SiteHost != "same.test" || row.FeedURL != s.FeedURL {
			t.Errorf("row for %s = %+v (present %v)", s.FeedURL, row, ok)
		}
	}
}

func TestRefreshIconsCapsSitesPerPass(t *testing.T) {
	h := newIconHosts(t)
	db := iconDB(t)
	var sites []IconSite
	for i := range 25 {
		sites = append(sites, feedAt(fmt.Sprintf("http://h%02d.test/rss", i), fmt.Sprintf("http://h%02d.test/", i)))
	}

	stats := RefreshIcons(context.Background(), db, h.client(), sites, iconNow)
	if stats.Stale != 25 || stats.OK != 20 || stats.Deferred != 5 {
		t.Fatalf("first pass stats = %+v", stats)
	}
	meta, _ := db.FeedIconMeta(context.Background())
	if len(meta) != 20 {
		t.Fatalf("%d rows after the first pass, want 20", len(meta))
	}

	stats = RefreshIcons(context.Background(), db, h.client(), sites, iconNow)
	if stats.Stale != 5 || stats.OK != 5 || stats.Deferred != 0 {
		t.Fatalf("second pass stats = %+v", stats)
	}
	if meta, _ = db.FeedIconMeta(context.Background()); len(meta) != 25 {
		t.Errorf("%d rows after the second pass, want 25", len(meta))
	}
}

func TestRefreshIconsRunsFourSitesAtATime(t *testing.T) {
	h := newIconHosts(t)
	h.gate = make(chan struct{})
	db := iconDB(t)
	var sites []IconSite
	for i := range 12 {
		sites = append(sites, feedAt(fmt.Sprintf("http://c%02d.test/rss", i), fmt.Sprintf("http://c%02d.test/", i)))
	}
	done := make(chan IconStats, 1)
	go func() { done <- RefreshIcons(context.Background(), db, h.client(), sites, iconNow) }()

	for range 4 {
		select {
		case <-h.started:
		case <-time.After(5 * time.Second):
			t.Fatal("fewer than four sites started")
		}
	}
	select {
	case host := <-h.started:
		t.Fatalf("a fifth site (%s) started while four were in flight", host)
	case <-time.After(200 * time.Millisecond):
	}

	h.openGate()
	select {
	case stats := <-done:
		if stats.OK != 12 {
			t.Fatalf("stats = %+v", stats)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the pass did not finish")
	}
	if got := h.peakPages(); got != 4 {
		t.Errorf("peak concurrency = %d, want 4", got)
	}
}

func TestRefreshIconsPassDeadlineLeavesTheRestUntouched(t *testing.T) {
	withIconBounds(t, 20, 4, 400*time.Millisecond)
	h := newIconHosts(t)
	db := iconDB(t)
	sites := []IconSite{feedAt("http://fast.test/rss", "http://fast.test/")}
	for i := range 5 {
		host := fmt.Sprintf("slow%d.test", i)
		h.hold[host] = true
		sites = append(sites, feedAt("http://"+host+"/rss", "http://"+host+"/"))
	}

	start := time.Now()
	stats := RefreshIcons(context.Background(), db, h.client(), sites, iconNow)
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("pass took %v with a 400ms deadline", took)
	}
	if stats.OK != 1 || stats.Errors != 0 || stats.Deferred != 5 {
		t.Errorf("stats = %+v, want 1 ok and 5 deferred", stats)
	}
	meta, _ := db.FeedIconMeta(context.Background())
	if len(meta) != 1 {
		t.Errorf("%d rows written, want only the fast site's: %v", len(meta), meta)
	}
}

func TestRefreshIconsRequestTimeoutRecordsError(t *testing.T) {
	h := newIconHosts(t)
	h.hold["stuck.test"] = true
	db := iconDB(t)
	client := h.client()
	client.Timeout = 150 * time.Millisecond
	s := feedAt("http://stuck.test/rss", "http://stuck.test/")

	stats := RefreshIcons(context.Background(), db, client, []IconSite{s}, iconNow)
	if stats.Errors != 1 || stats.OK != 0 || stats.None != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	row, ok := iconRow(t, db, s.FeedID)
	if !ok || row.Status != store.IconStatusError || row.Error == "" || len(row.Error) > 200 {
		t.Errorf("row = %+v", row)
	}
}

func TestRefreshIconsStaleness(t *testing.T) {
	day := 24 * time.Hour
	type tc struct {
		name     string
		status   string // "" for no row
		age      time.Duration
		oldFeed  bool // the row was computed for another feed URL
		fallback bool // the site is the feed URL's own origin, not a real link
		want     bool // refetched
	}
	cases := []tc{
		{"no row", "", 0, false, false, true},
		{"fresh ok", store.IconStatusOK, day, false, false, false},
		{"ok at 29d", store.IconStatusOK, 29 * day, false, false, false},
		{"ok at 31d", store.IconStatusOK, 31 * day, false, false, true},
		{"none at 29d", store.IconStatusNone, 29 * day, false, false, false},
		{"none at 31d", store.IconStatusNone, 31 * day, false, false, true},
		{"error at 23h", store.IconStatusError, 23 * time.Hour, false, false, false},
		{"error at 25h", store.IconStatusError, 25 * time.Hour, false, false, true},
		{"feed url changed", store.IconStatusOK, time.Hour, true, false, true},

		{"fallback, no row", "", 0, false, true, true},
		{"fallback, fresh ok", store.IconStatusOK, day, false, true, false},
		{"fallback, ok at 31d", store.IconStatusOK, 31 * day, false, true, false},
		{"fallback, none at 31d", store.IconStatusNone, 31 * day, false, true, false},
		{"fallback, error at 25h", store.IconStatusError, 25 * time.Hour, false, true, false},
		{"fallback, feed url changed", store.IconStatusOK, time.Hour, true, true, true},
	}
	h := newIconHosts(t)
	db := iconDB(t)
	var sites []IconSite
	var rows []store.FeedIcon
	for i, c := range cases {
		host := fmt.Sprintf("s%d.test", i)
		s := feedAt("http://"+host+"/rss", "http://"+host+"/")
		s.Fallback = c.fallback
		sites = append(sites, s)
		if c.status == "" {
			continue
		}
		row := store.FeedIcon{
			FeedID: s.FeedID, FeedURL: s.FeedURL, SiteHost: host, Status: c.status,
			FetchedAt: iconNow.Add(-c.age),
		}
		if c.status == store.IconStatusOK {
			row.Bytes, row.Mime, row.ETag = []byte("old"), "image/png", `"old"`
		}
		if c.oldFeed {
			row.FeedURL = "http://" + host + "/old-rss"
		}
		rows = append(rows, row)
	}
	if err := db.UpsertFeedIcons(context.Background(), rows); err != nil {
		t.Fatal(err)
	}

	RefreshIcons(context.Background(), db, h.client(), sites, iconNow)
	for i, c := range cases {
		host := fmt.Sprintf("s%d.test", i)
		if got := h.count(host) > 0; got != c.want {
			t.Errorf("%s: refetched = %v, want %v", c.name, got, c.want)
		}
	}
}

// A kick covers every live feed with its URL's origin as a fallback site, so it
// fills what is missing and leaves an aged lookup to a run.
func TestRefreshIconsKickModeNeverRefreshesAgedRows(t *testing.T) {
	day := 24 * time.Hour
	h := newIconHosts(t)
	cfg := testConfigWith(t, config.FeedsDoc{Feeds: []config.Feed{
		{Name: "OK", URL: "http://aged-ok.test/rss"},
		{Name: "None", URL: "http://aged-none.test/rss"},
		{Name: "Err", URL: "http://aged-err.test/rss"},
		{Name: "New", URL: "http://new.test/rss"},
	}})
	db := openStore(t, cfg)
	aged := func(host, status string, age time.Duration) store.FeedIcon {
		u := "http://" + host + "/rss"
		return store.FeedIcon{
			FeedID: config.FeedID(u), FeedURL: u, SiteHost: host, Status: status,
			FetchedAt: iconNow.Add(-age),
		}
	}
	err := db.UpsertFeedIcons(context.Background(), []store.FeedIcon{
		aged("aged-ok.test", store.IconStatusOK, 60*day),
		aged("aged-none.test", store.IconStatusNone, 60*day),
		aged("aged-err.test", store.IconStatusError, 3*day),
	})
	if err != nil {
		t.Fatal(err)
	}

	stats := RefreshIcons(context.Background(), db, h.client(), nil, iconNow)
	if stats.Stale != 1 || stats.OK != 1 {
		t.Fatalf("stats = %+v, want only the new feed", stats)
	}
	for _, host := range []string{"aged-ok.test", "aged-none.test", "aged-err.test"} {
		if n := h.count(host); n != 0 {
			t.Errorf("%s was requested %d times by a kick", host, n)
		}
	}
	if h.count("new.test") == 0 {
		t.Error("the new feed was not fetched")
	}
}

func TestRefreshIconsFollowsASiteLinkThatMoved(t *testing.T) {
	h := newIconHosts(t)
	db := iconDB(t)
	feedURL := "http://feeds.moved.test/rss"
	s := fallbackAt(feedURL, "http://feeds.moved.test")

	// Lookup recorded from the feed's own host (the add-time kick).
	RefreshIcons(context.Background(), db, h.client(), []IconSite{s}, iconNow)
	row, _ := iconRow(t, db, s.FeedID)
	if row.SiteHost != "feeds.moved.test" {
		t.Fatalf("site host = %q", row.SiteHost)
	}
	before := h.total()

	// A run then reports the feed's real site link.
	linked := feedAt(feedURL, "http://www.moved.test/blog")
	RefreshIcons(context.Background(), db, h.client(), []IconSite{linked}, iconNow)
	if row, _ = iconRow(t, db, s.FeedID); row.SiteHost != "moved.test" {
		t.Fatalf("site host after the link = %q, want moved.test", row.SiteHost)
	}
	after := h.total()
	if after == before {
		t.Fatal("the moved link was not fetched")
	}

	// Stable: the same link again, and a run that fell back to the feed origin.
	RefreshIcons(context.Background(), db, h.client(), []IconSite{linked}, iconNow)
	RefreshIcons(context.Background(), db, h.client(), []IconSite{s}, iconNow)
	if h.total() != after {
		t.Error("a repeated or fallback site triggered another fetch")
	}
	if row, _ = iconRow(t, db, s.FeedID); row.SiteHost != "moved.test" {
		t.Errorf("site host flipped to %q", row.SiteHost)
	}
}

// A feed whose host differs from its site (a hosted feed) keeps the icon of its
// real link: neither a kick nor a run whose fetch failed brings the feed host's
// icon back, however old the row is.
func TestRefreshIconsHostedFeedKeepsItsLinkedIcon(t *testing.T) {
	const feedURL = "http://feeds.hosted.test/blog"
	h := newIconHosts(t)
	cfg := testConfigWith(t, config.FeedsDoc{Feeds: []config.Feed{{Name: "Hosted", URL: feedURL}}})
	db := openStore(t, cfg)
	id := config.FeedID(feedURL)

	// A run found the real link.
	RefreshIcons(context.Background(), db, h.client(), []IconSite{feedAt(feedURL, "http://blog.test/")}, iconNow)
	if row, _ := iconRow(t, db, id); row.SiteHost != "blog.test" || row.Status != store.IconStatusOK {
		t.Fatalf("row = %+v", row)
	}
	later := iconNow.Add(90 * 24 * time.Hour)
	before := h.count("blog.test")

	// A kick and a run whose fetch failed both fall back to the feed host.
	RefreshIcons(context.Background(), db, h.client(), nil, later)
	fallback := []IconSite{fallbackAt(feedURL, "http://feeds.hosted.test")}
	RefreshIcons(context.Background(), db, h.client(), fallback, later)
	if n := h.count("feeds.hosted.test"); n != 0 {
		t.Errorf("the feed host was requested %d times", n)
	}
	if row, _ := iconRow(t, db, id); row.SiteHost != "blog.test" || !row.FetchedAt.Equal(iconNow) {
		t.Errorf("row = %+v, want it untouched", row)
	}

	// A run that finds the link again is what ages it out.
	RefreshIcons(context.Background(), db, h.client(), []IconSite{feedAt(feedURL, "http://blog.test/")}, later)
	if h.count("blog.test") <= before {
		t.Error("the aged row was not refreshed by a run with a real link")
	}
	if row, _ := iconRow(t, db, id); row.SiteHost != "blog.test" || !row.FetchedAt.Equal(later) {
		t.Errorf("row = %+v", row)
	}
}

func TestRefreshIconsOrdersNeverThenMovedThenOldest(t *testing.T) {
	withIconBounds(t, 2, 1, time.Minute)
	h := newIconHosts(t)
	db := iconDB(t)
	aged := feedAt("http://aged.test/rss", "http://aged.test/")
	moved := feedAt("http://moved.test/rss", "http://moved.test/")
	never := feedAt("http://never.test/rss", "http://never.test/")
	err := db.UpsertFeedIcons(context.Background(), []store.FeedIcon{
		{FeedID: aged.FeedID, FeedURL: aged.FeedURL, SiteHost: "aged.test", Status: store.IconStatusNone,
			FetchedAt: iconNow.Add(-90 * 24 * time.Hour)},
		{FeedID: moved.FeedID, FeedURL: "http://moved.test/older", SiteHost: "moved.test",
			Status: store.IconStatusNone, FetchedAt: iconNow.Add(-time.Hour)},
	})
	if err != nil {
		t.Fatal(err)
	}

	stats := RefreshIcons(context.Background(), db, h.client(), []IconSite{aged, moved, never}, iconNow)
	if stats.Stale != 3 || stats.Deferred != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	if h.count("never.test") == 0 || h.count("moved.test") == 0 || h.count("aged.test") != 0 {
		t.Errorf("fetched never=%d moved=%d aged=%d, want never and moved only",
			h.count("never.test"), h.count("moved.test"), h.count("aged.test"))
	}
}

func TestRefreshIconsCrossrefIsNoneWithoutARequest(t *testing.T) {
	h := newIconHosts(t)
	db := iconDB(t)
	s := feedAt("http://api.crossref.org/works?q=x", "http://api.crossref.org")

	stats := RefreshIcons(context.Background(), db, h.client(), []IconSite{s}, iconNow)
	if stats.None != 1 || h.total() != 0 {
		t.Errorf("stats = %+v, %d requests", stats, h.total())
	}
	row, ok := iconRow(t, db, s.FeedID)
	if !ok || row.Status != store.IconStatusNone || row.SiteHost != "api.crossref.org" {
		t.Errorf("row = %+v (present %v)", row, ok)
	}
}

func TestRefreshIconsAcademicUsesTheSourceOrigin(t *testing.T) {
	h := newIconHosts(t)
	h.noIcon["export.arxiv.org"] = true
	db := iconDB(t)
	results := []feeds.Result{{}}
	active := []config.ResolvedFeed{{
		ID: config.FeedID("http://export.arxiv.org/api/query?q=ai"), URL: "http://export.arxiv.org/api/query?q=ai",
		Type: config.FeedTypeAcademic,
	}}
	sites := iconSites(active, results)
	if len(sites) != 1 || sites[0].SiteURL != "http://export.arxiv.org" {
		t.Fatalf("sites = %+v", sites)
	}

	stats := RefreshIcons(context.Background(), db, h.client(), sites, iconNow)
	row, ok := iconRow(t, db, active[0].ID)
	if stats.OK != 1 || !ok || row.Status != store.IconStatusOK || row.SiteHost != "export.arxiv.org" {
		t.Errorf("stats = %+v, row = %+v", stats, row)
	}
	if h.count("arxiv.org") == 0 {
		t.Error("the parent domain was not tried")
	}
}

func TestRefreshIconsNoIconIsNone(t *testing.T) {
	h := newIconHosts(t)
	h.noIcon["bare.test"] = true
	db := iconDB(t)
	s := feedAt("http://bare.test/rss", "http://bare.test/")
	stats := RefreshIcons(context.Background(), db, h.client(), []IconSite{s}, iconNow)
	if row, _ := iconRow(t, db, s.FeedID); stats.None != 1 || row.Status != store.IconStatusNone || row.Bytes != nil {
		t.Errorf("stats = %+v, row = %+v", stats, row)
	}
}

func TestRefreshIconsBlockedAddressIsNone(t *testing.T) {
	t.Setenv(httpclient.AllowPrivateEnv, "")
	h := newIconHosts(t)
	db := iconDB(t)
	s := feedAt(h.URL+"/rss", h.URL)

	stats := RefreshIcons(context.Background(), db, feeds.NewClient(""), []IconSite{s}, iconNow)
	row, ok := iconRow(t, db, s.FeedID)
	if stats.None != 1 || stats.Errors != 0 || !ok || row.Status != store.IconStatusNone {
		t.Errorf("stats = %+v, row = %+v", stats, row)
	}
	if h.total() != 0 {
		t.Errorf("%d requests reached the private server", h.total())
	}
}

func TestRefreshIconsCancelledWritesNothing(t *testing.T) {
	t.Run("before the pass", func(t *testing.T) {
		h := newIconHosts(t)
		db := iconDB(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		RefreshIcons(ctx, db, h.client(), []IconSite{
			feedAt("http://a.test/rss", "http://a.test/"),
			feedAt("http://api.crossref.org/w", "http://api.crossref.org"),
		}, iconNow)
		if meta, _ := db.FeedIconMeta(context.Background()); len(meta) != 0 || h.total() != 0 {
			t.Errorf("rows = %v, requests = %d", meta, h.total())
		}
	})

	t.Run("during the pass", func(t *testing.T) {
		h := newIconHosts(t)
		h.hold["slow.test"] = true
		db := iconDB(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			<-h.started
			cancel()
		}()
		RefreshIcons(ctx, db, h.client(), []IconSite{
			feedAt("http://fast.test/rss", "http://fast.test/"),
			feedAt("http://slow.test/rss", "http://slow.test/"),
			feedAt("http://api.crossref.org/w", "http://api.crossref.org"),
		}, iconNow)
		if meta, _ := db.FeedIconMeta(context.Background()); len(meta) != 0 {
			t.Errorf("rows = %v, want none after cancellation", meta)
		}
	})
}

func TestRefreshIconsWithoutSitesCoversEveryLiveFeed(t *testing.T) {
	h := newIconHosts(t)
	off := false
	cfg := testConfigWith(t, config.FeedsDoc{Feeds: []config.Feed{
		{Name: "On", URL: "http://on.test/rss"},
		{Name: "Off", URL: "http://off.test/rss", Enabled: &off},
	}})
	db := openStore(t, cfg)

	stats := RefreshIcons(context.Background(), db, h.client(), nil, iconNow)
	if stats.OK != 2 {
		t.Fatalf("stats = %+v, want both feeds covered", stats)
	}
	if h.count("on.test") == 0 || h.count("off.test") == 0 {
		t.Error("a live feed was skipped")
	}
}

func TestIconSitesPrefersTheFeedLink(t *testing.T) {
	active := []config.ResolvedFeed{
		{ID: "a", URL: "http://feeds.a.test/rss"},
		{ID: "b", URL: "http://b.test/rss"},
		{ID: "c", URL: "ftp://c.test/rss"},
	}
	results := []feeds.Result{{Site: "http://a.test/blog"}, {Err: fmt.Errorf("down")}, {}}
	got := iconSites(active, results)
	want := []IconSite{
		{FeedID: "a", FeedURL: active[0].URL, SiteURL: "http://a.test/blog"},
		{FeedID: "b", FeedURL: active[1].URL, SiteURL: "http://b.test", Fallback: true},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("sites = %+v, want %+v", got, want)
	}
}

func TestTrimErrorKeepsWholeRunes(t *testing.T) {
	for name, unit := range map[string]string{"two byte": "é", "three byte": "€", "four byte": "𝄞"} {
		for lead := range 4 {
			msg := strings.Repeat("a", lead) + strings.Repeat(unit, 150)
			got := trimError(errors.New(msg))
			if !utf8.ValidString(got) || len(got) > iconErrorMaxChars || !strings.HasPrefix(msg, got) {
				t.Errorf("%s with %d lead bytes: clipped to %d bytes, valid utf-8 = %v",
					name, lead, len(got), utf8.ValidString(got))
			}
			if len(got) < iconErrorMaxChars-utf8.UTFMax {
				t.Errorf("%s with %d lead bytes: clipped to %d bytes, too short", name, lead, len(got))
			}
		}
	}
	if got := trimError(errors.New("short")); got != "short" {
		t.Errorf("short message = %q", got)
	}
}

func TestRunFillsOutcomeSites(t *testing.T) {
	linked := `<?xml version="1.0"?><rss version="2.0"><channel><title>T</title>` +
		`<link>http://blog.test/home</link><item><title>vLLM notes</title>` +
		`<link>https://x.test/1</link><guid>https://x.test/1</guid></item></channel></rss>`
	failing := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(failing.Close)
	linkedURL := serveRSS(t, linked)
	cfg := testConfig(t,
		config.Feed{Name: "Linked", URL: linkedURL},
		config.Feed{Name: "Failing", URL: failing.URL + "/feed"},
	)
	db := openStore(t, cfg)

	out, err := Run(context.Background(), cfg.Config, testProfile("vllm"), db, time.Now(),
		Options{Record: true, Client: loopbackClient})
	if err != nil {
		t.Fatal(err)
	}
	bySite := map[string]IconSite{}
	for _, s := range out.Sites {
		bySite[s.FeedID] = s
	}
	if s := bySite[config.FeedID(linkedURL)]; s.SiteURL != "http://blog.test/home" || s.FeedURL != linkedURL ||
		s.Fallback {
		t.Errorf("linked site = %+v", s)
	}
	if s := bySite[config.FeedID(failing.URL+"/feed")]; s.SiteURL != failing.URL || !s.Fallback {
		t.Errorf("failed feed site = %+v, want its origin %s", s, failing.URL)
	}
	if len(out.Sites) != 2 {
		t.Errorf("%d sites, want 2", len(out.Sites))
	}
}

// iconManager is a manager over a store holding one feed at feedURL, whose
// fetches go to h.
func iconManager(t *testing.T, h *iconHosts, feedURLs ...string) *Manager {
	t.Helper()
	m, _ := newManagerForTest(t, Outcome{}, nil)
	var doc config.FeedsDoc
	for i, u := range feedURLs {
		doc.Feeds = append(doc.Feeds, config.Feed{Name: fmt.Sprintf("F%d", i), URL: u})
	}
	if _, err := m.db.SeedFeeds(context.Background(), doc); err != nil {
		t.Fatal(err)
	}
	m.SetClient(h.client())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		m.Shutdown(ctx)
	})
	return m
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestKickIconsFillsMissingIcons(t *testing.T) {
	h := newIconHosts(t)
	m := iconManager(t, h, "http://kick.test/rss")
	m.KickIcons()
	waitFor(t, "the icon row", func() bool {
		_, ok := iconRow(t, m.db, config.FeedID("http://kick.test/rss"))
		return ok
	})
}

func TestKickIconsSkipsWhileAPassRuns(t *testing.T) {
	h := newIconHosts(t)
	m := iconManager(t, h, "http://held.test/rss")
	id := config.FeedID("http://held.test/rss")

	m.iconMu.Lock()
	m.KickIcons()
	if h.total() != 0 {
		t.Error("a kick ran while a pass held the lock")
	}
	m.mu.Lock()
	m.pending = false
	m.mu.Unlock()
	m.iconMu.Unlock()

	m.KickIcons()
	waitFor(t, "the icon row", func() bool { _, ok := iconRow(t, m.db, id); return ok })
}

func TestShutdownCancelsAndWaitsForAKickedPass(t *testing.T) {
	h := newIconHosts(t)
	h.hold["slow.test"] = true
	m := iconManager(t, h, "http://slow.test/rss")
	m.KickIcons()
	select {
	case <-h.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the pass never reached the site")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	m.Shutdown(ctx)
	if ctx.Err() != nil || time.Since(start) > 2*time.Second {
		t.Fatal("Shutdown did not finish promptly")
	}
	if m.iconMu.TryLock() {
		m.iconMu.Unlock()
	} else {
		t.Error("the pass still holds the lock after Shutdown")
	}
	if _, ok := iconRow(t, m.db, config.FeedID("http://slow.test/rss")); ok {
		t.Error("a cancelled pass wrote a row")
	}
	m.KickIcons()
	if m.iconMu.TryLock() {
		m.iconMu.Unlock()
	} else {
		t.Error("a kick started after Shutdown")
	}
	if h.count("slow.test") != 1 {
		t.Error("a kick fetched after Shutdown")
	}
}

// addLateFeed adds a feed the running pass cannot have read.
func addLateFeed(t *testing.T, m *Manager) {
	t.Helper()
	late := config.Feed{Name: "Late", URL: "http://late.test/rss"}
	if _, err := m.db.AddFeed(context.Background(), late); err != nil {
		t.Fatal(err)
	}
}

func TestKickIconsDuringAPassRunsOneMore(t *testing.T) {
	h := newIconHosts(t)
	h.hold["first.test"] = true
	m := iconManager(t, h, "http://first.test/rss")
	m.KickIcons()
	select {
	case <-h.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the pass never reached the site")
	}

	// The running pass already read its feed list, so only a second pass can see
	// this one. Two kicks in the meantime still make a single extra pass.
	addLateFeed(t, m)
	m.KickIcons()
	m.KickIcons()
	if h.count("late.test") != 0 {
		t.Fatal("the late feed was fetched before the first pass ended")
	}

	h.open()
	waitFor(t, "the late feed's row", func() bool {
		_, ok := iconRow(t, m.db, config.FeedID("http://late.test/rss"))
		return ok
	})
	waitFor(t, "the passes to end", func() bool {
		if !m.iconMu.TryLock() {
			return false
		}
		m.iconMu.Unlock()
		return true
	})
	if n := h.count("late.test"); n != 2 {
		t.Errorf("late.test saw %d requests, want its page and icon once", n)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pending {
		t.Error("pending is still set after the passes ended")
	}
}

func TestShutdownDropsAPendingKick(t *testing.T) {
	h := newIconHosts(t)
	h.hold["slow.test"] = true
	m := iconManager(t, h, "http://slow.test/rss")
	m.KickIcons()
	select {
	case <-h.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the pass never reached the site")
	}
	addLateFeed(t, m)
	m.KickIcons()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	m.Shutdown(ctx)
	if h.count("late.test") != 0 {
		t.Error("a pending kick ran after Shutdown")
	}
}

// runStub makes the manager's run return outcome, then err.
func runStub(m *Manager, outcome Outcome, err error) {
	m.run = func(
		context.Context, config.Config, profile.Snapshot, *store.Store, time.Time, Options,
	) (Outcome, error) {
		return outcome, err
	}
}

func TestRunEndPassFillsIconsForFetchedSites(t *testing.T) {
	h := newIconHosts(t)
	m := iconManager(t, h)
	s := feedAt("http://feeds.run.test/rss", "http://run.test/blog")
	runStub(m, Outcome{Sites: []IconSite{s}}, nil)

	if err := m.Start(context.Background(), store.IngestTriggerManual); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, m)
	row, ok := iconRow(t, m.db, s.FeedID)
	if !ok || row.Status != store.IconStatusOK || row.SiteHost != "run.test" {
		t.Errorf("row = %+v (present %v)", row, ok)
	}
}

func TestRunEndPassFreesTheRunSlotFirst(t *testing.T) {
	h := newIconHosts(t)
	h.hold["slow.test"] = true
	m := iconManager(t, h)
	s := feedAt("http://slow.test/rss", "http://slow.test/")
	runStub(m, Outcome{Sites: []IconSite{s}}, nil)

	if err := m.Start(context.Background(), store.IngestTriggerManual); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the pass never reached the site")
	}
	if m.Status().Running {
		t.Error("the run still holds the slot during the icon pass")
	}
	last, err := m.db.LastIngestRun(context.Background())
	if err != nil || last.Status != store.IngestStatusOK {
		t.Errorf("last run = %+v, %v", last, err)
	}

	// Shutdown ends the pass and the run goroutine with it.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	m.Shutdown(ctx)
	if ctx.Err() != nil {
		t.Fatal("Shutdown timed out")
	}
	waitIdle(t, m)
	if _, ok := iconRow(t, m.db, s.FeedID); ok {
		t.Error("a cancelled pass wrote a row")
	}
}

func TestRunEndPassSkippedForCancelledAndRescoreRuns(t *testing.T) {
	h := newIconHosts(t)
	m := iconManager(t, h)
	s := feedAt("http://skip.test/rss", "http://skip.test/")

	m.run = func(
		ctx context.Context, _ config.Config, _ profile.Snapshot, _ *store.Store, _ time.Time, _ Options,
	) (Outcome, error) {
		<-ctx.Done()
		return Outcome{Sites: []IconSite{s}}, ctx.Err()
	}
	if err := m.Start(context.Background(), store.IngestTriggerManual); err != nil {
		t.Fatal(err)
	}
	m.Cancel()
	waitIdle(t, m)

	m.rescore = func(
		context.Context, config.Config, profile.Snapshot, *store.Store, time.Time, time.Duration,
	) (RescoreOutcome, error) {
		return RescoreOutcome{}, nil
	}
	if err := m.StartRescore(context.Background(), time.Hour); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, m)

	if h.total() != 0 {
		t.Errorf("%d icon requests after a cancelled and a rescore run", h.total())
	}
}

func TestRunEndPassSkippedForFailedRunsAndRunsWithoutSites(t *testing.T) {
	h := newIconHosts(t)
	m := iconManager(t, h)
	s := feedAt("http://skip.test/rss", "http://skip.test/")

	runStub(m, Outcome{Sites: []IconSite{s}}, errors.New("scoring failed"))
	if err := m.Start(context.Background(), store.IngestTriggerManual); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, m)

	runStub(m, Outcome{}, nil)
	if err := m.Start(context.Background(), store.IngestTriggerManual); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, m)

	m.src = funcSource(func(context.Context) (config.Config, int, error) {
		return config.Config{}, 0, errors.New("config unreadable")
	})
	if err := m.Start(context.Background(), store.IngestTriggerManual); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, m)

	// Shutdown waits for any pass a run started.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	m.Shutdown(ctx)
	if h.total() != 0 {
		t.Errorf("%d icon requests after a failed run and a run without sites", h.total())
	}
}
