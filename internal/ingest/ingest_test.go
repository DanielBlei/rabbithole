// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// feedRSS renders a one-item RSS feed for a test server.
func feedRSS(title, link string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel><title>Feed</title>
  <item><title>%s</title><link>%s</link><guid>%s</guid>
  <description>summary</description></item>
</channel></rss>`, title, link, link)
}

// serveRSS starts a test server returning the given RSS body, registered for
// cleanup, and returns its URL.
func serveRSS(t *testing.T, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// testConfig builds a heuristic-provider config (no backend needed) over the
// given feeds, with a wide age window and a low threshold so test items pass.
func testConfig(t *testing.T, feeds ...config.Feed) *config.Config {
	t.Helper()
	return testConfigWith(t, config.FeedsDoc{Feeds: feeds})
}

// testConfigWith is testConfig over a full feeds document, so a test can
// exercise the defaults block and per-feed overrides. Feeds live in the store,
// so the document is seeded into the database the config points at; the
// openStore below reopens that same file.
func testConfigWith(t *testing.T, doc config.FeedsDoc) *config.Config {
	t.Helper()
	cfg := &config.Config{
		Inference: config.InferenceConfig{BatchSize: 2, MaxParallel: 2, Provider: "heuristic", Model: "test-model"},
		Ingest:    config.IngestConfig{Since: config.Duration(365 * 24 * time.Hour)},
		Store:     config.StoreConfig{DBPath: filepath.Join(t.TempDir(), "test.db")},
	}
	db, err := store.Open(cfg.Store.DBPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.SeedFeeds(t.Context(), doc); err != nil {
		t.Fatalf("seed feeds: %v", err)
	}
	return cfg
}

func openStore(t *testing.T, cfg *config.Config) *store.Store {
	t.Helper()
	db, err := store.Open(cfg.Store.DBPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func sourceCounts(t *testing.T, db *store.Store) map[string]int {
	t.Helper()
	srcs, err := db.Sources(context.Background())
	if err != nil {
		t.Fatalf("sources: %v", err)
	}
	got := make(map[string]int, len(srcs))
	for _, s := range srcs {
		got[s.Source] = s.Count
	}
	return got
}

func TestRunProcessesFeedsPerSourceAndDedups(t *testing.T) {
	ctx := context.Background()
	// "Alpha" scores 2 (matches vllm, inference); "Beta" scores 1 (latency).
	feedA := config.Feed{Name: "Alpha", URL: serveRSS(t, feedRSS("vLLM inference notes", "https://x.test/a"))}
	feedB := config.Feed{Name: "Beta", URL: serveRSS(t, feedRSS("Latency guide", "https://x.test/b"))}
	cfg := testConfig(t, feedA, feedB)
	db := openStore(t, cfg)
	profile := "vllm inference latency batching"

	out, err := Run(ctx, cfg, profile, db, time.Now(), Options{Record: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Fetched != 2 {
		t.Errorf("Fetched = %d, want 2", out.Fetched)
	}
	if len(out.Unseen) != 2 {
		t.Errorf("Unseen = %d, want 2", len(out.Unseen))
	}
	if len(out.Results) != 2 {
		t.Fatalf("Results = %d, want 2", len(out.Results))
	}
	// Results are sorted best-first across feeds: Alpha (2) before Beta (1).
	if out.Results[0].Item.Source != "Alpha" || out.Results[1].Item.Source != "Beta" {
		t.Errorf("results not sorted best-first across feeds: %q then %q",
			out.Results[0].Item.Source, out.Results[1].Item.Source)
	}
	// Both feeds were recorded, grouped by source.
	if got := sourceCounts(t, db); got["Alpha"] != 1 || got["Beta"] != 1 {
		t.Errorf("source counts = %v, want Alpha:1 Beta:1", got)
	}

	// Second run: every item is already in the store, so the pre-scoring dedup
	// drops them all and nothing new is processed.
	out2, err := Run(ctx, cfg, profile, db, time.Now(), Options{Record: true})
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if len(out2.Unseen) != 0 {
		t.Errorf("second run Unseen = %d, want 0 (all deduped)", len(out2.Unseen))
	}
}

// Fetched counts only items within the configured recency window, not every
// item the feed parser returned — a feed can carry an old backlog that was
// never in scope for this (or any) run.
func TestRunFetchedCountsOnlyItemsWithinAgeWindow(t *testing.T) {
	ctx := context.Background()
	recent := time.Now().Add(-1 * time.Hour).Format(time.RFC1123Z)
	old := time.Now().Add(-30 * 24 * time.Hour).Format(time.RFC1123Z)
	body := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel><title>Feed</title>
  <item><title>Recent item</title><link>https://x.test/recent</link><guid>https://x.test/recent</guid>
    <pubDate>%s</pubDate><description>summary</description></item>
  <item><title>Old item</title><link>https://x.test/old</link><guid>https://x.test/old</guid>
    <pubDate>%s</pubDate><description>summary</description></item>
</channel></rss>`, recent, old)
	feed := config.Feed{Name: "Mixed", URL: serveRSS(t, body)}
	cfg := testConfig(t, feed)
	// The feed sets no window of its own, so it inherits ingest.since — which
	// the run resolves against, not load.
	cfg.Ingest.Since = config.Duration(24 * time.Hour) // narrow window: only the recent item survives
	db := openStore(t, cfg)

	out, err := Run(ctx, cfg, "test", db, time.Now(), Options{Record: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Fetched != 1 {
		t.Errorf("Fetched = %d, want 1 (only the item within the configured window)", out.Fetched)
	}
	if len(out.Unseen) != 1 || out.Unseen[0].Title != "Recent item" {
		t.Errorf("Unseen = %+v, want just the recent item", out.Unseen)
	}
}

func TestRunSkipsFailedFeed(t *testing.T) {
	ctx := context.Background()
	good := config.Feed{Name: "Good", URL: serveRSS(t, feedRSS("vLLM inference notes", "https://x.test/g"))}
	bad := config.Feed{Name: "Bad", URL: serveRSS(t, "not xml at all")}
	cfg := testConfig(t, good, bad)
	db := openStore(t, cfg)

	out, err := Run(ctx, cfg, "vllm inference", db, time.Now(), Options{Record: true})
	if err != nil {
		t.Fatalf("Run should not fail because one feed failed: %v", err)
	}
	if len(out.Unseen) != 1 {
		t.Errorf("Unseen = %d, want 1 (only the good feed)", len(out.Unseen))
	}
	if got := sourceCounts(t, db); got["Good"] != 1 || got["Bad"] != 0 {
		t.Errorf("source counts = %v, want only Good:1", got)
	}
}

// multiItemRSS renders a feed of n items, newest first, one hour apart. slug
// namespaces the item links: dedup keys on link across feeds, so two feeds in
// one test must not share them.
func multiItemRSS(t *testing.T, slug string, n int) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		`<rss version="2.0"><channel><title>Feed</title>`)
	for i := range n {
		pub := time.Now().Add(-time.Duration(i) * time.Hour).Format(time.RFC1123Z)
		fmt.Fprintf(&b, `<item><title>Item %d</title><link>https://x.test/%s/%d</link>`+
			`<guid>https://x.test/%s/%d</guid><pubDate>%s</pubDate><description>summary</description></item>`,
			i, slug, i, slug, i, pub)
	}
	b.WriteString(`</channel></rss>`)
	return b.String()
}

// A disabled feed is never fetched — the run must not even reach its URL.
func TestRunSkipsDisabledFeeds(t *testing.T) {
	ctx := context.Background()
	var offHits int32
	off := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&offHits, 1)
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(feedRSS("Parked item", "https://x.test/off")))
	}))
	defer off.Close()

	disabled := false
	cfg := testConfigWith(t, config.FeedsDoc{Feeds: []config.Feed{
		{Name: "On", URL: serveRSS(t, feedRSS("vLLM inference notes", "https://x.test/on"))},
		{Name: "Off", URL: off.URL, Enabled: &disabled},
	}})
	db := openStore(t, cfg)

	out, err := Run(ctx, cfg, "vllm inference", db, time.Now(), Options{Record: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := atomic.LoadInt32(&offHits); n != 0 {
		t.Errorf("disabled feed was fetched %d times, want 0", n)
	}
	if len(out.Unseen) != 1 || out.Unseen[0].Source != "On" {
		t.Errorf("Unseen = %+v, want only the enabled feed's item", out.Unseen)
	}
	if got := sourceCounts(t, db); got["Off"] != 0 {
		t.Errorf("source counts = %v, want nothing recorded for Off", got)
	}
}

// A deleted feed is invisible to a run. Deletion is soft, so the row is still
// there — the run has to be reading the live set, not every row in the table.
func TestRunSkipsDeletedFeeds(t *testing.T) {
	ctx := context.Background()
	var goneHits int32
	gone := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&goneHits, 1)
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(feedRSS("Removed item", "https://x.test/gone")))
	}))
	defer gone.Close()

	cfg := testConfigWith(t, config.FeedsDoc{Feeds: []config.Feed{
		{Name: "Kept", URL: serveRSS(t, feedRSS("vLLM inference notes", "https://x.test/kept"))},
		{Name: "Removed", URL: gone.URL},
	}})
	db := openStore(t, cfg)

	feeds, err := db.Feeds(ctx)
	if err != nil {
		t.Fatalf("Feeds: %v", err)
	}
	for _, f := range feeds {
		if f.Name == "Removed" {
			if err := db.SoftDeleteFeed(ctx, f.ID); err != nil {
				t.Fatalf("SoftDeleteFeed: %v", err)
			}
		}
	}

	out, err := Run(ctx, cfg, "vllm inference", db, time.Now(), Options{Record: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := atomic.LoadInt32(&goneHits); n != 0 {
		t.Errorf("deleted feed was fetched %d times, want 0", n)
	}
	if len(out.Unseen) != 1 || out.Unseen[0].Source != "Kept" {
		t.Errorf("Unseen = %+v, want only the kept feed's item", out.Unseen)
	}
}

// Each feed filters against its own window and cap, not one global setting.
func TestRunAppliesPerFeedFilters(t *testing.T) {
	tight := config.Duration(2*time.Hour + 30*time.Minute) // keeps items 0,1,2
	cap2 := 2

	cases := []struct {
		name string
		doc  config.FeedsDoc
		// want maps feed name -> how many of its items should survive.
		want map[string]int
	}{
		{
			name: "per-feed since overrides the defaults block",
			doc: config.FeedsDoc{
				Defaults: config.FeedDefaults{Since: &tight},
				Feeds: []config.Feed{
					{Name: "Tight", URL: serveRSS(t, multiItemRSS(t, "tight", 6))},
					{Name: "Wide", URL: serveRSS(t, multiItemRSS(t, "wide", 6)), Since: durPtr(48 * time.Hour)},
				},
			},
			want: map[string]int{"Tight": 3, "Wide": 6},
		},
		{
			name: "per-feed cap overrides the defaults block",
			doc: config.FeedsDoc{
				Defaults: config.FeedDefaults{MaxItems: &cap2},
				Feeds: []config.Feed{
					{Name: "Capped", URL: serveRSS(t, multiItemRSS(t, "capped", 5))},
					{Name: "Uncapped", URL: serveRSS(t, multiItemRSS(t, "unc", 5)), MaxItems: intPtr(0)},
				},
			},
			want: map[string]int{"Capped": 2, "Uncapped": 5},
		},
		{
			// Both filters apply, age first then the cap.
			name: "since and cap compose",
			doc: config.FeedsDoc{
				Feeds: []config.Feed{
					{
						Name:     "Both",
						URL:      serveRSS(t, multiItemRSS(t, "both", 10)),
						Since:    durPtr(4*time.Hour + 30*time.Minute), // items 0..4 survive
						MaxItems: intPtr(2),                            // then trimmed to 2
					},
				},
			},
			want: map[string]int{"Both": 2},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := testConfigWith(t, c.doc)
			db := openStore(t, cfg)

			out, err := Run(context.Background(), cfg, "test", db, time.Now(), Options{Record: true})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			got := map[string]int{}
			for _, it := range out.Unseen {
				got[it.Source]++
			}
			for feed, want := range c.want {
				if got[feed] != want {
					t.Errorf("%s kept %d items, want %d", feed, got[feed], want)
				}
			}
		})
	}
}

// The cap keeps the newest items, so a firehose can't eat the scoring budget.
func TestRunCapKeepsNewestItems(t *testing.T) {
	cfg := testConfigWith(t, config.FeedsDoc{Feeds: []config.Feed{
		{Name: "Firehose", URL: serveRSS(t, multiItemRSS(t, "fire", 5)), MaxItems: intPtr(2)},
	}})
	db := openStore(t, cfg)

	out, err := Run(context.Background(), cfg, "test", db, time.Now(), Options{Record: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(out.Unseen) != 2 {
		t.Fatalf("Unseen = %d, want 2 (the cap)", len(out.Unseen))
	}
	// Item 0 is newest, item 1 next — the cap must not take an arbitrary pair.
	got := []string{out.Unseen[0].Title, out.Unseen[1].Title}
	sort.Strings(got)
	if got[0] != "Item 0" || got[1] != "Item 1" {
		t.Errorf("kept %v, want the two newest (Item 0, Item 1)", got)
	}
	// Fetched counts what survived both filters, not what the feed returned.
	if out.Fetched != 2 {
		t.Errorf("Fetched = %d, want 2 (post-cap)", out.Fetched)
	}
}

// Every fetch outcome is appended to the feed history — including the failures
// Run otherwise only logs and skips, which is the whole point of recording it.
func TestRunRecordsFeedFetchHistory(t *testing.T) {
	cases := []struct {
		name   string
		record bool // the run's Options.Record
	}{
		{name: "recording run", record: true},
		// History must land on a --dry-run too: a dead feed should surface
		// whether or not the run persisted any items.
		{name: "non-recording run", record: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			good := config.Feed{Name: "Good", URL: serveRSS(t, feedRSS("vLLM inference notes", "https://x.test/g"))}
			bad := config.Feed{Name: "Bad", URL: serveRSS(t, "not xml at all")}
			cfg := testConfig(t, good, bad)
			db := openStore(t, cfg)

			if _, err := Run(ctx, cfg, "vllm inference", db, time.Now(), Options{Record: c.record}); err != nil {
				t.Fatalf("Run: %v", err)
			}

			health, err := db.FeedHealthByID(ctx, 10)
			if err != nil {
				t.Fatalf("FeedHealthByID: %v", err)
			}
			// History is keyed on the feed's stable ID, not its display name.
			goodH, ok := health[config.FeedID(good.URL)]
			if !ok || !goodH.OK() {
				t.Fatalf("Good health = %+v, want a recorded ok entry", goodH)
			}
			if goodH.Items != 1 {
				t.Errorf("Good items = %d, want 1", goodH.Items)
			}
			badH, ok := health[config.FeedID(bad.URL)]
			if !ok || badH.OK() {
				t.Fatalf("Bad health = %+v, want a recorded error entry", badH)
			}
			if badH.Error == "" {
				t.Error("Bad health should carry the fetch error")
			}
			if badH.FailStreak != 1 {
				t.Errorf("Bad fail streak = %d, want 1", badH.FailStreak)
			}
		})
	}
}

// Successive runs append rather than overwrite, so the history accumulates.
func TestRunAppendsToFeedHistory(t *testing.T) {
	ctx := context.Background()
	feed := config.Feed{Name: "Solo", URL: serveRSS(t, feedRSS("A", "https://x.test/a"))}
	cfg := testConfig(t, feed)
	db := openStore(t, cfg)

	for range 3 {
		if _, err := Run(ctx, cfg, "test", db, time.Now(), Options{Record: true}); err != nil {
			t.Fatalf("Run: %v", err)
		}
	}

	health, err := db.FeedHealthByID(ctx, 100)
	if err != nil {
		t.Fatalf("FeedHealthByID: %v", err)
	}
	if got := health[config.FeedID(feed.URL)].Recent; len(got) != 3 {
		t.Errorf("history = %d attempts, want one per run (3)", len(got))
	}
}

func TestCapNewest(t *testing.T) {
	now := time.Now()
	mk := func(title string, age time.Duration) feeds.Item {
		return feeds.Item{Title: title, Published: now.Add(-age)}
	}
	in := []feeds.Item{mk("old", 3*time.Hour), mk("new", time.Hour), {Title: "undated"}, mk("mid", 2*time.Hour)}

	if got := capNewest(in, 0); len(got) != 4 {
		t.Errorf("cap 0 returned %d items, want all 4 (uncapped)", len(got))
	}
	if got := capNewest(in, 10); len(got) != 4 {
		t.Errorf("cap above length returned %d items, want 4", len(got))
	}
	got := capNewest(in, 2)
	if len(got) != 2 || got[0].Title != "new" || got[1].Title != "mid" {
		t.Fatalf("cap 2 = %v, want [new mid]", titles(got))
	}
	// An unknown publish date must not outrank a known-recent item.
	if got := capNewest(in, 3); got[2].Title != "old" {
		t.Errorf("cap 3 = %v, want undated sorted last", titles(got))
	}
	// The input must not be reordered under the caller.
	if in[0].Title != "old" {
		t.Errorf("capNewest mutated its input: %v", titles(in))
	}
}

func titles(items []feeds.Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Title
	}
	return out
}

// intPtr builds a *int for a per-feed override.
func intPtr(n int) *int { return &n }

// durPtr builds a *config.Duration for a per-feed override.
func durPtr(d time.Duration) *config.Duration {
	v := config.Duration(d)
	return &v
}
