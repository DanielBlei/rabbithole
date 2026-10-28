// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"strings"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// recordFetches stores attempts for feed: n failures, then oks successes, all
// in the order given, so the newest attempt is last.
func recordFetches(t *testing.T, db *store.Store, feed config.Feed, oks, fails int) {
	t.Helper()
	var attempts []store.FeedFetch
	now := time.Now().Add(-time.Hour)
	for i := range oks {
		attempts = append(attempts, store.FeedFetch{
			FeedID: config.FeedID(feed.URL), Name: feed.Name, Items: 3, At: now.Add(time.Duration(i) * time.Second),
		})
	}
	for i := range fails {
		attempts = append(attempts, store.FeedFetch{
			FeedID: config.FeedID(feed.URL), Name: feed.Name, Error: "503 unavailable",
			At: now.Add(time.Duration(oks+i) * time.Second),
		})
	}
	if len(attempts) == 0 {
		return
	}
	if err := db.RecordFeedFetches(t.Context(), attempts); err != nil {
		t.Fatalf("RecordFeedFetches: %v", err)
	}
}

// With no sources the page is the first-run prompt, not the overview.
func TestOverviewEmptyStoreAsksForASource(t *testing.T) {
	w, _ := sourcesTestWeb(t, config.FeedsDoc{})
	out := get(t, w, "/sources")

	for _, want := range []string{
		"No sources yet",
		`<a class="clk-btn clk-btn--go" href="/sources/manage?add=1">Add a source</a>`,
		`<a class="clk-btn" href="/sources/manage">Import</a>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q; body=%s", want, out)
		}
	}
	if strings.Contains(out, `class="ov ov--page"`) {
		t.Errorf("first run renders the overview page; body=%s", out)
	}
}

func TestOverviewAllHealthy(t *testing.T) {
	alpha := config.Feed{Name: "Alpha", URL: "https://alpha.test/feed"}
	beta := config.Feed{Name: "Beta", URL: "https://beta.test/feed"}
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{alpha, beta}})
	recordFetches(t, db, alpha, 1, 0)
	recordFetches(t, db, beta, 1, 0)
	out := get(t, w, "/sources")

	for _, want := range []string{
		`<p class="ov-brief">2 sources, all healthy.</p>`,
		"Nothing failing.",
		"100%",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q; body=%s", want, out)
		}
	}
	if strings.Contains(out, "ov-att--") {
		t.Errorf("attention rows shown with nothing to attend to; body=%s", out)
	}
}

// Failing and flaky sources need attention; a source waiting for its first run
// is listed but not counted in the brief.
func TestOverviewNeedsAttention(t *testing.T) {
	broken := config.Feed{Name: "Broken", URL: "https://broken.test/feed"}
	wobbly := config.Feed{Name: "Wobbly", URL: "https://wobbly.test/feed"}
	waiting := config.Feed{Name: "Waiting", URL: "https://waiting.test/feed"}
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{broken, wobbly, waiting}})
	recordFetches(t, db, broken, 0, 3)
	recordFetches(t, db, wobbly, 0, 1)
	out := get(t, w, "/sources")

	for _, want := range []string{
		`<p class="ov-brief">3 sources, 2 need attention.</p>`,
		"1 source failing",
		"Broken</div>",
		"1 source flaky",
		"Wobbly: the latest fetch failed, earlier ones were fine",
		"1 source is waiting for the first run",
		"Waiting: ingestion fetches it next time it runs",
		`href="/sources/manage?state=failing"`,
		`href="/sources/manage?state=flaky"`,
		`aria-label="0 OK, 1 Flaky, 1 Failing, 1 Waiting for first run, 0 Off"`,
		`<span class="ov-rate__n">0%</span>`,
		"0 of 4 recent fetches",
		`class="ingb`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q; body=%s", want, out)
		}
	}
	if strings.Contains(out, "Nothing failing.") {
		t.Errorf("all-clear shown with failing sources; body=%s", out)
	}
	if strings.Contains(out, "?state=disabled") {
		t.Errorf("off link shown with no disabled source; body=%s", out)
	}
}

// Off sources are counted in the health bar and link to their Manage filter,
// but never need attention.
func TestOverviewCountsOffSources(t *testing.T) {
	good := config.Feed{Name: "Good", URL: "https://good.test/feed"}
	parked := config.Feed{Name: "Parked", URL: "https://parked.test/feed", Enabled: boolPtr(false)}
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{good, parked}})
	recordFetches(t, db, good, 1, 0)
	out := get(t, w, "/sources")

	for _, want := range []string{
		`<p class="ov-brief">2 sources, all healthy.</p>`,
		`href="/sources/manage?state=disabled"`,
		`<b>1</b> off`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q; body=%s", want, out)
		}
	}
}

// A feed with no fetch on record reports n/a and says why.
func TestOverviewRateWithNoFetches(t *testing.T) {
	w, _ := sourcesTestWeb(t, config.FeedsDoc{
		Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}},
	})
	out := get(t, w, "/sources")

	for _, want := range []string{`<span class="ov-rate__n">n/a</span>`, "No fetches yet"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q; body=%s", want, out)
		}
	}
}

// The rate counts recent successes over recent attempts, and the newest failure
// flags the source as flaky.
func TestOverviewRateCountsRecentAttempts(t *testing.T) {
	alpha := config.Feed{Name: "Alpha", URL: "https://alpha.test/feed"}
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{alpha}})
	recordFetches(t, db, alpha, 3, 1)
	out := get(t, w, "/sources")

	for _, want := range []string{`<span class="ov-rate__n">75%</span>`, "3 of 4 recent fetches"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q; body=%s", want, out)
		}
	}
}

// A removed source is not a source on the overview: it leaves the total, the
// brief and the attention list.
func TestOverviewLeavesOutRemovedSources(t *testing.T) {
	alpha := config.Feed{Name: "Alpha", URL: "https://alpha.test/feed"}
	gone := config.Feed{Name: "Gone", URL: "https://gone.test/feed"}
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{alpha, gone}})
	recordFetches(t, db, alpha, 1, 0)
	recordFetches(t, db, gone, 0, 3)
	if err := db.SoftDeleteFeed(t.Context(), feedID(t, db, "Gone")); err != nil {
		t.Fatalf("SoftDeleteFeed: %v", err)
	}
	out := get(t, w, "/sources")

	if !strings.Contains(out, `<p class="ov-brief">1 source, all healthy.</p>`) {
		t.Errorf("removed source counted on the overview; body=%s", out)
	}
	if strings.Contains(out, "ov-att--") {
		t.Errorf("removed source listed in attention; body=%s", out)
	}
}
