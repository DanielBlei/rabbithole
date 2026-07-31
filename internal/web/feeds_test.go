// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// globalSince is the ingest.since these tests resolve "inherited from global"
// against.
const globalSince = 9 * 24 * time.Hour

// feedsTestWeb builds a Web over a config whose feeds are resolved through the
// real cascade, so the viewer is tested against genuinely resolved values.
func feedsTestWeb(t *testing.T, doc config.FeedsDoc) (*Web, *store.Store) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	cfg := &config.Config{Ingest: config.IngestConfig{Since: config.Duration(globalSince)}}
	if err := cfg.SetFeeds(doc); err != nil {
		t.Fatalf("SetFeeds: %v", err)
	}
	cfg.Feeds.Path = "/tmp/feeds.yaml"
	return New(db, cfg, ":8080", "", testIngestManager(t, db)), db
}

func getFeeds(t *testing.T, w *Web) string {
	t.Helper()
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/feeds", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /feeds: status = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	return rec.Body.String()
}

// intPtr / boolPtr build the optional per-feed knobs.
func intPtr(n int) *int    { return &n }
func boolPtr(b bool) *bool { return &b }

// durPtr builds a *config.Duration for a per-feed override.
func durPtr(d time.Duration) *config.Duration {
	v := config.Duration(d)
	return &v
}

// The viewer's job is showing resolved values and where they came from.
func TestHandleFeedsRendersResolvedValues(t *testing.T) {
	w, _ := feedsTestWeb(t, config.FeedsDoc{
		Defaults: config.FeedDefaults{MaxItems: intPtr(5), Tags: []string{"all"}},
		Feeds: []config.Feed{
			{Name: "Alpha", URL: "https://alpha.test/feed", Since: durPtr(48 * time.Hour), Tags: []string{"ai"}},
			{Name: "Beta", URL: "https://beta.test/feed"},
			{Name: "Parked", URL: "https://parked.test/feed", Enabled: boolPtr(false)},
		},
	})
	out := getFeeds(t, w)

	cases := []struct {
		name string
		want string
		why  string
	}{
		{name: "feeds file path", want: "/tmp/feeds.yaml", why: "the viewer names the file it read"},
		{name: "total count", want: "3 feeds", why: "parked feeds still count"},
		{name: "enabled count", want: "2 enabled", why: "one of the three is parked"},
		{name: "own since rendered short", want: ">2d<", why: "Alpha's own 48h window"},
		{name: "inherited global since", want: "9d", why: "Beta falls through to ingest.since"},
		{name: "global origin marker", want: "inherited from global", why: "provenance tooltip"},
		{name: "defaults origin marker", want: "inherited from defaults", why: "Beta's cap comes from defaults"},
		{name: "defaults tag", want: ">all<", why: "defaults tags union onto every feed"},
		{name: "own tag", want: ">ai<", why: "the feed's own tag survives the union"},
		{name: "parked feed shown", want: "Parked", why: "disabled feeds are listed, not hidden"},
		{name: "parked dot", want: "fds__dot--off", why: "parked renders as its own state"},
		{name: "never fetched", want: "not fetched yet", why: "no history recorded yet"},
		{name: "host label", want: "alpha.test", why: "compact host under the feed name"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !strings.Contains(out, c.want) {
				t.Errorf("missing %q (%s); body=%s", c.want, c.why, out)
			}
		})
	}
}

// Health is joined onto the config by feed ID and drives the row's state.
func TestHandleFeedsShowsHealth(t *testing.T) {
	good := config.Feed{Name: "Good", URL: "https://good.test/feed"}
	broken := config.Feed{Name: "Broken", URL: "https://broken.test/feed"}
	w, db := feedsTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{good, broken}})

	now := time.Now()
	if err := db.RecordFeedFetches(t.Context(), []store.FeedFetch{
		{FeedID: config.FeedID(good.URL), Name: good.Name, Items: 7, At: now},
		{FeedID: config.FeedID(broken.URL), Name: broken.Name, Error: "dial tcp: no such host", At: now},
	}); err != nil {
		t.Fatalf("RecordFeedFetches: %v", err)
	}
	out := getFeeds(t, w)

	cases := []struct {
		name string
		want string
	}{
		{name: "item count", want: "7 items"},
		{name: "ok dot", want: "fds__dot--ok"},
		{name: "error dot", want: "fds__dot--error"},
		{name: "error surfaced", want: "dial tcp: no such host"},
		{name: "failing count in summary", want: "1 failing"},
		{name: "history strip rendered", want: "fds__tick"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !strings.Contains(out, c.want) {
				t.Errorf("missing %q; body=%s", c.want, out)
			}
		})
	}
}

// Health survives a rename because it's keyed on the URL-derived ID.
func TestHandleFeedsHealthSurvivesRename(t *testing.T) {
	const url = "https://good.test/feed"
	w, db := feedsTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{
		{Name: "The New Name", URL: url},
	}})
	// History was recorded under the feed's *old* display name.
	if err := db.RecordFeedFetches(t.Context(), []store.FeedFetch{
		{FeedID: config.FeedID(url), Name: "The Old Name", Items: 4, At: time.Now()},
	}); err != nil {
		t.Fatalf("RecordFeedFetches: %v", err)
	}

	out := getFeeds(t, w)
	if !strings.Contains(out, "4 items") {
		t.Errorf("renamed feed lost its history; body=%s", out)
	}
	if strings.Contains(out, "not fetched yet") {
		t.Errorf("renamed feed reported as never fetched; body=%s", out)
	}
}

// A disabled feed reports as parked even with history on file — old errors
// from before it was switched off must not read as a live problem.
func TestHandleFeedsDisabledOverridesStaleHealth(t *testing.T) {
	parked := config.Feed{Name: "Parked", URL: "https://parked.test/feed", Enabled: boolPtr(false)}
	w, db := feedsTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{parked}})
	if err := db.RecordFeedFetches(t.Context(), []store.FeedFetch{
		{FeedID: config.FeedID(parked.URL), Name: parked.Name, Error: "410 gone", At: time.Now()},
	}); err != nil {
		t.Fatalf("RecordFeedFetches: %v", err)
	}

	out := getFeeds(t, w)
	// The error may still appear in the history strip's tooltip — that's the
	// point of keeping history. What must not happen is it reading as the
	// feed's *current* state.
	if strings.Contains(out, `class="fds__err"`) {
		t.Errorf("stale error rendered as live status for a disabled feed; body=%s", out)
	}
	if !strings.Contains(out, "fds__dot--off") {
		t.Errorf("expected the parked dot; body=%s", out)
	}
	if !strings.Contains(out, ">disabled<") {
		t.Errorf("expected the disabled label; body=%s", out)
	}
	if strings.Contains(out, "1 failing") {
		t.Errorf("a disabled feed must not count as failing; body=%s", out)
	}
	// Its history is still there to look back at.
	if !strings.Contains(out, "fds__tick") {
		t.Errorf("parked feed lost its history strip; body=%s", out)
	}
}

// A run of failures shows the streak and when it started.
func TestHandleFeedsShowsFailureRun(t *testing.T) {
	feed := config.Feed{Name: "Broken", URL: "https://broken.test/feed"}
	w, db := feedsTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{feed}})

	id := config.FeedID(feed.URL)
	base := time.Now().Add(-10 * time.Hour)
	fetches := []store.FeedFetch{
		{FeedID: id, Name: feed.Name, Items: 3, At: base},
		{FeedID: id, Name: feed.Name, Error: "boom", At: base.Add(2 * time.Hour)},
		{FeedID: id, Name: feed.Name, Error: "boom", At: base.Add(4 * time.Hour)},
		{FeedID: id, Name: feed.Name, Error: "boom", At: base.Add(6 * time.Hour)},
	}
	for _, f := range fetches {
		if err := db.RecordFeedFetches(t.Context(), []store.FeedFetch{f}); err != nil {
			t.Fatalf("RecordFeedFetches: %v", err)
		}
	}

	out := getFeeds(t, w)
	if !strings.Contains(out, "&times;3") {
		t.Errorf("expected the ×3 failure streak; body=%s", out)
	}
	if !strings.Contains(out, "failing since") {
		t.Errorf("expected a failing-since phrase; body=%s", out)
	}
}

func TestToFeedStripOrdersOldestFirst(t *testing.T) {
	now := time.Now()
	// store returns Recent newest-first.
	attempts := []store.FeedAttempt{
		{Status: store.FeedStatusError, Error: "newest", At: now.Add(-1 * time.Hour)},
		{Status: store.FeedStatusOK, Items: 2, At: now.Add(-2 * time.Hour)},
		{Status: store.FeedStatusOK, Items: 1, At: now.Add(-3 * time.Hour)},
	}
	strip := toFeedStrip(attempts, now)
	if len(strip) != 3 {
		t.Fatalf("strip = %d ticks, want 3", len(strip))
	}
	// Display is oldest-first, so the newest attempt is last.
	if !strip[0].OK || !strip[1].OK || strip[2].OK {
		t.Errorf("strip OK flags = %v/%v/%v, want ok,ok,failed",
			strip[0].OK, strip[1].OK, strip[2].OK)
	}
	if !strings.Contains(strip[2].Title, "newest") {
		t.Errorf("last tick title = %q, want the newest attempt's error", strip[2].Title)
	}
	if toFeedStrip(nil, now) != nil {
		t.Error("no attempts should produce no strip")
	}
}

func TestShortDur(t *testing.T) {
	cases := []struct {
		name string
		in   time.Duration
		want string
	}{
		{name: "whole days", in: 7 * 24 * time.Hour, want: "7d"},
		{name: "hours rounding to days", in: 48 * time.Hour, want: "2d"},
		{name: "bare hours", in: 36 * time.Hour, want: "36h"},
		{name: "minutes", in: 90 * time.Minute, want: "90m"},
		{name: "sub-minute falls back to Go form", in: 90 * time.Second, want: "1m30s"},
		{name: "zero", in: 0, want: "—"},
		{name: "negative", in: -time.Hour, want: "—"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shortDur(c.in); got != c.want {
				t.Errorf("shortDur(%v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestCapLabel(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want string
	}{
		{name: "uncapped", in: 0, want: "—"},
		{name: "negative treated as uncapped", in: -3, want: "—"},
		{name: "capped", in: 25, want: "25"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := capLabel(c.in); got != c.want {
				t.Errorf("capLabel(%d) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestHostOf(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "https", in: "https://medium.com/feed/tag/ai", want: "medium.com"},
		{name: "http", in: "http://example.test/rss", want: "example.test"},
		{name: "with port", in: "http://localhost:8080/rss", want: "localhost:8080"},
		{name: "no scheme falls back to raw", in: "example.test/rss", want: "example.test/rss"},
		{name: "empty", in: "", want: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hostOf(c.in); got != c.want {
				t.Errorf("hostOf(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestItemsPhrase(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{in: 0, want: "0 items"},
		{in: 1, want: "1 item"},
		{in: 2, want: "2 items"},
	}
	for _, c := range cases {
		if got := itemsPhrase(c.in); got != c.want {
			t.Errorf("itemsPhrase(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDefaultsSummary(t *testing.T) {
	cases := []struct {
		name     string
		defaults config.FeedDefaults
		want     string
	}{
		{
			name: "no defaults names the global fallback",
			want: "since 9d (from ingest.since) · uncapped",
		},
		{
			name:     "defaults set",
			defaults: config.FeedDefaults{Since: durPtr(72 * time.Hour), MaxItems: intPtr(10)},
			want:     "since 3d · max_items 10",
		},
		{
			name:     "disabled by default is called out",
			defaults: config.FeedDefaults{Enabled: boolPtr(false)},
			want:     "since 9d (from ingest.since) · uncapped · disabled by default",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := &config.Config{Ingest: config.IngestConfig{Since: config.Duration(globalSince)}}
			if err := cfg.SetFeeds(config.FeedsDoc{
				Defaults: c.defaults,
				Feeds:    []config.Feed{{Name: "A", URL: "http://a.test/feed"}},
			}); err != nil {
				t.Fatalf("SetFeeds: %v", err)
			}
			if got := defaultsSummary(cfg); got != c.want {
				t.Errorf("defaultsSummary = %q, want %q", got, c.want)
			}
		})
	}
}
