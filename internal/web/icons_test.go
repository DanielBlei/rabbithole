// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"bytes"
	"database/sql"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// pngBytes is a small valid PNG.
func pngBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 16, 16))
	for x := range 16 {
		img.Set(x, x, color.NRGBA{R: 200, A: 255})
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

// putIcon stores an ok icon for the feed at rawURL and returns its id.
func putIcon(t *testing.T, db *store.Store, rawURL, etag string, dark bool) string {
	t.Helper()
	id := config.FeedID(rawURL)
	err := db.UpsertFeedIcons(t.Context(), []store.FeedIcon{{
		FeedID: id, FeedURL: rawURL, SiteHost: "x.test", Status: store.IconStatusOK, Mime: "image/png",
		ETag: `"` + etag + `"`, Dark: dark, FetchedAt: time.Now(), Bytes: pngBytes(t),
	}})
	if err != nil {
		t.Fatalf("UpsertFeedIcons: %v", err)
	}
	return id
}

func iconReq(h http.Handler, method, path string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestIconRouteServesStoredBytes(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{})
	id := putIcon(t, db, "https://a.test/feed", "abc123", false)

	rec := iconReq(w.Routes(), http.MethodGet, "/icons/"+id+"?v=abc123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	for k, want := range map[string]string{
		"Content-Type":            "image/png",
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": "default-src 'none'; sandbox",
		"Cache-Control":           "private, max-age=604800, immutable",
		"ETag":                    `"abc123"`,
	} {
		if got := rec.Header().Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if !bytes.Equal(rec.Body.Bytes(), pngBytes(t)) {
		t.Error("body differs from the stored bytes")
	}
}

func TestIconRouteAnswers304ForMatchingTag(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{})
	id := putIcon(t, db, "https://a.test/feed", "abc123", false)

	rec := iconReq(w.Routes(), http.MethodGet, "/icons/"+id, "If-None-Match", `"abc123"`)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("304 carried a body of %d bytes", rec.Body.Len())
	}
	if got := rec.Header().Get("Cache-Control"); got != iconCacheControl {
		t.Errorf("304 Cache-Control = %q", got)
	}
	if rec := iconReq(w.Routes(), http.MethodGet, "/icons/"+id, "If-None-Match", `"other"`); rec.Code != http.StatusOK {
		t.Errorf("a different tag: status = %d, want 200", rec.Code)
	}
}

func TestIconRouteIs404ForUnknownAndNonOKWithoutCaching(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{})
	none := config.FeedID("https://none.test/feed")
	err := db.UpsertFeedIcons(t.Context(), []store.FeedIcon{{
		FeedID: none, FeedURL: "https://none.test/feed", Status: store.IconStatusNone, FetchedAt: time.Now(),
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"unknown", none, "a%2Fb", "..%2F..%2Fetc"} {
		rec := iconReq(w.Routes(), http.MethodGet, "/icons/"+id)
		if rec.Code != http.StatusNotFound {
			t.Errorf("/icons/%s: status = %d, want 404", id, rec.Code)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("/icons/%s: Cache-Control = %q, want no-store", id, got)
		}
	}
}

func TestIconRouteSitsBehindTheLoginAndOverwritesNoStore(t *testing.T) {
	w := gatedWeb(t)
	id := putIcon(t, w.db, "https://a.test/feed", "abc123", false)
	b := newBrowser(t, w)

	rec := b.get("/icons/" + id)
	wantRedirect(t, rec, "/login?next=%2Ficons%2F"+id)
	if rec.Body.Len() != 0 && strings.Contains(rec.Body.String(), "PNG") {
		t.Error("a logged-out request got the icon")
	}

	wantRedirect(t, b.login("alice", "correct horse"), "/")
	rec = b.get("/icons/" + id)
	wantStatus(t, rec, http.StatusOK)
	if got := rec.Header().Get("Cache-Control"); got != iconCacheControl {
		t.Errorf("Cache-Control = %q, want %q (the gate's no-store must be replaced)", got, iconCacheControl)
	}
}

func TestIconRefSrcEscapesAHostileIDAndTag(t *testing.T) {
	ref := iconRef{ID: `a/../b?x=1&"><script>`, ETag: `t"g&z=1#`}
	src := ref.Src()
	if strings.ContainsAny(src, `"<>#`) || strings.Count(src, "?") != 1 || strings.Count(src, "/") != 2 {
		t.Errorf("Src = %q leaves its slots", src)
	}
	if !strings.HasPrefix(src, "/icons/") || !strings.Contains(src, "?v=") {
		t.Errorf("Src = %q", src)
	}
}

func TestValidIconTag(t *testing.T) {
	for tag, want := range map[string]bool{
		"abc123": true, "A-b_c.9": true, "": false, `a"b`: false, "a b": false, "a&b": false,
		"a/b": false, strings.Repeat("a", 129): false,
	} {
		if got := validIconTag(tag); got != want {
			t.Errorf("validIconTag(%q) = %v, want %v", tag, got, want)
		}
	}
}

func TestIconRefsKeepOnlyOKRowsWithAPlainTag(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{})
	okID := putIcon(t, db, "https://ok.test/feed", "abc123", true)
	err := db.UpsertFeedIcons(t.Context(), []store.FeedIcon{
		{FeedID: "n", Status: store.IconStatusNone, FetchedAt: time.Now()},
		{FeedID: "bad", Status: store.IconStatusOK, ETag: `"a b"`, Bytes: []byte{1}, FetchedAt: time.Now()},
	})
	if err != nil {
		t.Fatal(err)
	}
	refs := w.iconRefs(t.Context())
	if len(refs) != 1 {
		t.Fatalf("refs = %+v, want only the ok row", refs)
	}
	if got := refs[okID]; got != (iconRef{ID: okID, ETag: "abc123", Dark: true}) {
		t.Errorf("ref = %+v", got)
	}
}

// renderFav executes the favicon partial for one source.
func renderFav(t *testing.T, icon iconRef, size int) string {
	t.Helper()
	tmpl := pageTmpl("feed.html")
	const src = `{{template "favicon" (fav .Icon "Alpha" "https://alpha.test/f" .Size)}}`
	if _, err := tmpl.New("t").Parse(src); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	data := struct {
		Icon iconRef
		Size int
	}{icon, size}
	if err := tmpl.ExecuteTemplate(&buf, "t", data); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestFaviconPartialDrawsTheIconOrTheMonogram(t *testing.T) {
	withIcon := renderFav(t, iconRef{ID: "f1", ETag: "abc"}, 20)
	if strings.Count(withIcon, "<img") != 1 || strings.Contains(withIcon, "fav__m") ||
		strings.Contains(withIcon, ">A<") {
		t.Errorf("icon tile = %s", withIcon)
	}
	img := tagWith(t, withIcon, "class", "fav__i")
	wantAttrs(t, "img", img, map[string]string{
		"src": "/icons/f1?v=abc", "alt": "", "width": "16", "height": "16", "decoding": "async",
	})
	if strings.Contains(withIcon, "fav--dark") || strings.Contains(withIcon, "fav--mono") {
		t.Errorf("plain icon tile has a plate or monogram class: %s", withIcon)
	}

	mono := renderFav(t, iconRef{}, 20)
	if strings.Contains(mono, "<img") || !strings.Contains(mono, "fav--mono") || !strings.Contains(mono, ">A<") {
		t.Errorf("monogram tile = %s", mono)
	}

	dark := renderFav(t, iconRef{ID: "f1", ETag: "abc", Dark: true}, 20)
	if !strings.Contains(dark, `class="fav fav--dark"`) {
		t.Errorf("dark icon tile lacks the plate: %s", dark)
	}

	// Both draw in the same --fs box.
	for _, out := range []string{withIcon, mono, dark} {
		if !strings.Contains(out, `style="--fs:20px"`) {
			t.Errorf("tile lacks the shared --fs box: %s", out)
		}
	}
	if big := renderFav(t, iconRef{ID: "f1", ETag: "abc"}, 28); !strings.Contains(big, `width="24"`) ||
		!strings.Contains(big, "--fs:28px") {
		t.Errorf("28px tile = %s", big)
	}
}

func TestFaviconPartialEscapesAHostileIcon(t *testing.T) {
	out := renderFav(t, iconRef{ID: `"><script>alert(1)</script>`, ETag: `x" onerror="y`}, 20)
	if strings.Contains(out, "<script") || strings.Contains(out, `onerror="y`) {
		t.Errorf("hostile icon broke out of the attribute: %s", out)
	}
}

// hasIconImg is whether the fragment holds the stored icon of id.
func hasIconImg(body, id string) bool {
	return strings.Contains(body, `class="fav__i" src="/icons/`+id+`?v=`)
}

func TestManageRowAndNeedsAttentionCardDrawTheIcon(t *testing.T) {
	good := config.Feed{Name: "Good", URL: "https://good.test/feed"}
	bad := config.Feed{Name: "Bad", URL: "https://bad.test/feed"}
	plain := config.Feed{Name: "Plain", URL: "https://plain.test/feed"}
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{good, bad, plain}})
	recordFailures(t, db, bad, 3, "503 unavailable")
	goodID := putIcon(t, db, good.URL, "g1", false)
	badID := putIcon(t, db, bad.URL, "b1", true)

	out := get(t, w, "/sources/manage")
	if !hasIconImg(out, goodID) {
		t.Errorf("Manage row lacks the icon of Good; body=%s", out)
	}
	card := out[strings.Index(out, `<article class="mg-card">`):]
	card, _, _ = strings.Cut(card, "</article>")
	if !hasIconImg(card, badID) || !strings.Contains(card, "fav--dark") {
		t.Errorf("needs-attention card lacks the dark icon of Bad: %s", card)
	}

	plainRow := out[strings.Index(out, `id="src-`+config.FeedID(plain.URL)+`"`):]
	plainRow, _, _ = strings.Cut(plainRow, `class="mg-item"`)
	if strings.Contains(plainRow, "<img") || !strings.Contains(plainRow, "fav--mono") {
		t.Errorf("a feed without an icon should keep its monogram: %s", plainRow)
	}

	unit := get(t, w, "/sources/feed/"+goodID)
	if !hasIconImg(unit, goodID) {
		t.Errorf("one-source reply lacks the icon; body=%s", unit)
	}
}

func TestOverviewSectionsDrawTheIcon(t *testing.T) {
	w, db := overviewSectionsWeb(t)
	alphaID := putIcon(t, db, "https://alpha.test/feed", "a1", false)
	betaID := putIcon(t, db, "https://beta.test/feed", "b1", false)

	out := get(t, w, "/sources")
	// Alpha is in volume, signal rows, the best callout and slowest; Beta in the
	// noisy callout, so every list carries an icon tile.
	if got := strings.Count(out, `src="/icons/`+alphaID+`?v=a1"`); got < 4 {
		t.Errorf("Alpha's icon drawn %d times on the Overview, want volume, best, signal and slowest", got)
	}
	if got := strings.Count(out, `src="/icons/`+betaID+`?v=b1"`); got < 3 {
		t.Errorf("Beta's icon drawn %d times on the Overview, want volume, noisy and signal", got)
	}
	slow := out[strings.Index(out, "Slowest feeds"):]
	if !strings.Contains(slow, `src="/icons/`+betaID+`?v=b1" alt="" width="14" height="14"`) {
		t.Errorf("slowest rows lack the 18px icon; section=%.600s", slow)
	}
	live := get(t, w, "/sources/live?page=overview")
	if !hasIconImg(live, alphaID) {
		t.Error("the live Overview lacks the icon")
	}
}

func TestOverviewSuggestionDrawsTheIcon(t *testing.T) {
	w, db := overviewLibraryWeb(t, config.FeedsDoc{Feeds: goFeeds()})
	id := putIcon(t, db, "https://a.test/1", "s1", false)

	out := get(t, w, "/sources")
	row := out[strings.Index(out, `id="ov-sug-`+id+`"`):]
	row, _, _ = strings.Cut(row, "</li>")
	if !hasIconImg(row, id) {
		t.Errorf("suggestion row lacks the icon: %s", row)
	}
}

func TestLibraryRowsDrawTheIconOfASubscribedSourceOnly(t *testing.T) {
	w, db := libraryTestWeb(t, config.FeedsDoc{})
	all := libraryEntries(w.library.Get(t.Context()))
	if len(all) < 2 {
		t.Fatalf("library has %d entries", len(all))
	}
	with, without := all[0].Source.URL, all[1].Source.URL
	id := putIcon(t, db, with, "l1", false)

	out := get(t, w, "/sources/library")
	if row := libRow(t, out, with); !hasIconImg(row, id) || !strings.Contains(row, `width="24"`) {
		t.Errorf("row with an icon: %s", row)
	}
	if row := libRow(t, out, without); strings.Contains(row, "<img") || !strings.Contains(row, "fav--mono") {
		t.Errorf("row without an icon should be a monogram: %s", row)
	}
}

// A failed icon read must cost nothing but the icons.
func TestPagesRenderWhenTheIconReadFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "icons.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	feed := config.Feed{Name: "Alpha", URL: "https://alpha.test/feed"}
	if _, err := db.SeedFeeds(t.Context(), config.FeedsDoc{Feeds: []config.Feed{feed}}); err != nil {
		t.Fatal(err)
	}
	w := New(db, config.Static{}, testIngestManager(t, db))
	putIcon(t, db, feed.URL, "a1", false)

	other, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	if _, err := other.ExecContext(t.Context(), `DROP TABLE feed_icons`); err != nil {
		t.Fatalf("drop feed_icons: %v", err)
	}
	if _, err := db.FeedIconMeta(t.Context()); err == nil {
		t.Fatal("FeedIconMeta still reads; the failure was not injected")
	}

	for _, path := range []string{"/sources", "/sources/manage"} {
		out := get(t, w, path)
		if strings.Contains(out, "<img") && strings.Contains(out, "fav__i") {
			t.Errorf("GET %s drew an icon after the read failed", path)
		}
	}
	if out := get(t, w, "/sources/manage"); !strings.Contains(out, "fav--mono") {
		t.Error("Manage lost its monograms")
	}
}

// A feed change asks the ingest manager to look up icons: the feed's site
// answers with a page naming an icon, and the row lands in the store.
func TestAfterFeedChangeKicksIconLookup(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{})
	icon := pngBytes(t)
	site := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			rw.Header().Set("Content-Type", "text/html")
			_, _ = rw.Write([]byte(`<html><head><link rel="icon" href="/i.png"></head><body></body></html>`))
		case "/i.png":
			rw.Header().Set("Content-Type", "image/png")
			_, _ = rw.Write(icon)
		default:
			http.NotFound(rw, r)
		}
	}))
	t.Cleanup(site.Close)
	w.ing.SetClient(site.Client())

	feed := config.Feed{Name: "Kicked", URL: site.URL + "/feed.xml"}
	if _, err := db.SeedFeeds(t.Context(), config.FeedsDoc{Feeds: []config.Feed{feed}}); err != nil {
		t.Fatal(err)
	}
	w.afterFeedChange(t.Context())

	id := config.FeedID(feed.URL)
	deadline := time.Now().Add(10 * time.Second)
	for {
		meta, err := db.FeedIconMeta(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if m, ok := meta[id]; ok {
			if m.Status != store.IconStatusOK {
				t.Fatalf("icon row = %+v, want status ok", m)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no icon row after the feed change")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// With no ingest manager (some tests build a Web without one) the kick is a no-op.
func TestKickIconsToleratesNoManager(t *testing.T) {
	w := &Web{}
	w.kickIcons()
}
