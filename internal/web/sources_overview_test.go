// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/library"
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
		"Add a feed you already read, or start from the Library: it has ready-made feeds grouped by topic.",
		`<a class="clk-btn clk-btn--go" href="/sources/manage?add=1">Add a source</a>`,
		`<a class="clk-btn" href="/sources/library">Browse the Library</a>`,
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
		`class="pane ov-shell"`,
		`aria-labelledby="ovSync"`,
		`data-ov-fold="attention" data-ov-default-open="false"`,
		`aria-expanded="false" aria-controls="ovAttnBody"`,
		`id="ovAttnBody" data-ov-content hidden`,
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
		`data-ov-fold="attention" data-ov-default-open="true"`,
		`aria-expanded="true" aria-controls="ovAttnBody"`,
		`id="ovAttnBody" data-ov-content`,
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

const overviewLibraryYAML = `schema_version: 1
type: rss
description: Test library.
groups:
  - name: alpha
    description: First group.
    sources:
      - name: Alpha One
        url: https://a.test/1
        description: One.
        labels: [Go, Open Source]
      - name: Alpha Two
        url: https://a.test/2
        description: Two.
        labels: [Go]
      - name: Alpha Three
        url: https://a.test/3
        description: Three.
        labels: [Rust]
      - name: Alpha Four
        url: https://a.test/4
        description: Four.
        labels: [Infra, Go, Open Source]
  - name: beta
    description: Second group.
    sources:
      - name: Beta One
        url: https://b.test/1
        description: One.
        labels: [Python]
      - name: Beta Two
        url: https://b.test/2
        description: Two.
        labels: [Go]
      - name: Beta Three
        url: https://b.test/3
        description: Three.
        labels: [Go]
      - name: Beta Four
        url: https://b.test/4
        description: Four.
        labels: [Go]
`

// overviewLibraryWeb is a Web over doc with a loaded library of
// overviewLibraryYAML.
func overviewLibraryWeb(t *testing.T, doc config.FeedsDoc) (*Web, *store.Store) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rss.yaml" {
			http.NotFound(rw, r)
			return
		}
		_, _ = rw.Write([]byte(overviewLibraryYAML))
	}))
	t.Cleanup(srv.Close)
	w, db := sourcesTestWeb(t, doc)
	w.SetLibrary(library.NewLoader(library.Options{Client: srv.Client(), BaseURL: srv.URL}))
	if snap := w.library.Get(t.Context()); !snap.Loaded {
		t.Fatalf("test library did not load: %+v", snap)
	}
	return w, db
}

// goFeeds are live sources whose tags match the Go labels of the test library.
func goFeeds() []config.Feed {
	return []config.Feed{
		{Name: "Mine", URL: "https://mine.test/f", Tags: []string{"go", "open source"}},
		{Name: "Mine too", URL: "https://mine.test/g", Tags: []string{"Go"}},
	}
}

// suggestionNames lists the names of the suggestion rows in page order.
func suggestionNames(body string) []string {
	var out []string
	for _, m := range regexp.MustCompile(`(?s)<li class="ov-grow"[^>]*>.*?<b class="ov-name" title="([^"]*)"`).FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return out
}

func TestOverviewSuggestionsRankByTagMatch(t *testing.T) {
	w, _ := overviewLibraryWeb(t, config.FeedsDoc{Feeds: goFeeds()})
	out := get(t, w, "/sources")

	want := []string{"Alpha One", "Alpha Four", "Alpha Two", "Beta Two"}
	if got := suggestionNames(out); !slices.Equal(got, want) {
		t.Errorf("suggestions = %v, want %v", got, want)
	}
	for _, s := range []string{
		"Library suggestions", "Based on the tags of your sources.",
		"Matches your tags: Go, Open Source", ">Browse the Library</a>",
		`name="pick" value="` + libKey("https://a.test/1") + `"`, `name="view" value="suggestion"`,
	} {
		if !strings.Contains(out, s) {
			t.Errorf("missing %q", s)
		}
	}
	if strings.Index(out, "Needs attention") > strings.Index(out, "Library suggestions") {
		t.Errorf("suggestions come before Needs attention")
	}
}

func TestOverviewSuggestionsSkipUnavailableSources(t *testing.T) {
	w, db := overviewLibraryWeb(t, config.FeedsDoc{Feeds: append(goFeeds(),
		config.Feed{Name: "Alpha One", URL: "https://a.test/1", Tags: []string{"go"}},
		config.Feed{Name: "Alpha Two", URL: "https://a.test/2"},
		config.Feed{Name: "Alpha Four", URL: "https://elsewhere.test/4"},
	)})
	if err := db.SoftDeleteFeed(t.Context(), feedID(t, db, "Alpha Two")); err != nil {
		t.Fatalf("SoftDeleteFeed: %v", err)
	}
	out := get(t, w, "/sources")

	want := []string{"Beta Two", "Beta Three", "Beta Four"}
	if got := suggestionNames(out); !slices.Equal(got, want) {
		t.Errorf("suggestions = %v, want %v (not subscribed, removed or name-taken)", got, want)
	}
}

func TestOverviewSuggestionsEmptyWithoutMatchingTags(t *testing.T) {
	w, _ := overviewLibraryWeb(t, config.FeedsDoc{Feeds: []config.Feed{
		{Name: "Mine", URL: "https://mine.test/f", Tags: []string{"cooking"}},
	}})
	out := get(t, w, "/sources")

	for _, s := range []string{
		"No suggestions right now. Tag your sources and the Library can match them.",
		">Browse the Library</a>", "Library coverage",
	} {
		if !strings.Contains(out, s) {
			t.Errorf("missing %q", s)
		}
	}
	if n := len(suggestionNames(out)); n != 0 {
		t.Errorf("%d suggestions with no matching tags", n)
	}
}

func TestOverviewCoverageCountsPerGroup(t *testing.T) {
	w, _ := overviewLibraryWeb(t, config.FeedsDoc{Feeds: []config.Feed{
		{Name: "Alpha One", URL: "https://a.test/1"},
		{Name: "Beta Two", URL: "https://b.test/2"},
		{Name: "Beta Three", URL: "https://b.test/3"},
	}})
	out := get(t, w, "/sources")

	for _, s := range []string{
		"Library coverage", "How much of each Library group you have added.",
		`<p class="ov-big"><b>3</b> of 8 library sources added</p>`,
		`href="/sources/library/alpha"`, `<b>1</b> of 4`, `style="width:25%"`, `aria-label="1 of 4 added"`,
		`href="/sources/library/beta"`, `<b>2</b> of 4`, `style="width:50%"`,
	} {
		if !strings.Contains(out, s) {
			t.Errorf("missing %q", s)
		}
	}
}

func TestOverviewLibrarySectionsHidden(t *testing.T) {
	tests := []struct {
		name  string
		doc   config.FeedsDoc
		setup func(*Web)
	}{
		{"first run", config.FeedsDoc{}, nil},
		{
			"not loaded",
			config.FeedsDoc{Feeds: goFeeds()},
			func(w *Web) { w.SetLibrary(library.NewLoader(library.Options{})) },
		},
		{
			"off",
			config.FeedsDoc{Feeds: goFeeds()},
			func(w *Web) { w.SetLibrary(library.NewLoader(library.Options{BaseURL: "off"})) },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, _ := overviewLibraryWeb(t, tt.doc)
			if tt.setup != nil {
				tt.setup(w)
			}
			out := get(t, w, "/sources")
			for _, s := range []string{"Library suggestions", "Library coverage"} {
				if strings.Contains(out, s) {
					t.Errorf("%s shown", s)
				}
			}
		})
	}
}

func TestOverviewLiveCarriesLibrarySections(t *testing.T) {
	w, _ := overviewLibraryWeb(t, config.FeedsDoc{Feeds: goFeeds()})
	out := get(t, w, "/sources/live?page=overview")

	for _, s := range []string{`id="srcOverview"`, "Library suggestions", "Library coverage", "Alpha One"} {
		if !strings.Contains(out, s) {
			t.Errorf("live overview missing %q", s)
		}
	}
}

func TestOverviewAddSwapsTheSuggestionRow(t *testing.T) {
	w, db := overviewLibraryWeb(t, config.FeedsDoc{Feeds: goFeeds()})
	key := libKey("https://a.test/1")
	out := postLibrary(t, w, "/sources/library/apply", url.Values{"pick": {key}, "view": {"suggestion"}}, nil)

	for _, s := range []string{
		`<li class="ov-grow" id="ov-sug-` + config.FeedID("https://a.test/1") + `"`,
		`<span class="stp-chip stchip--subscribed">Subscribed</span>`, "Matches your tags: Go",
		`id="ovCoverage"`, `hx-swap-oob="true"`, `<b>1</b> of 8 library sources added`,
	} {
		if !strings.Contains(out, s) {
			t.Errorf("reply missing %q; body=%s", s, out)
		}
	}
	if strings.Contains(out, `type="submit"`) || strings.Contains(out, "libBody") {
		t.Errorf("reply still offers Add or repaints the Library; body=%s", out)
	}
	feeds, err := db.Feeds(t.Context())
	if err != nil {
		t.Fatalf("Feeds: %v", err)
	}
	if !slices.ContainsFunc(feeds, func(f config.Feed) bool { return f.URL == "https://a.test/1" }) {
		t.Errorf("Alpha One was not added; feeds=%+v", feeds)
	}

	again := postLibrary(t, w, "/sources/library/apply", url.Values{"pick": {key}, "view": {"suggestion"}}, nil)
	if !strings.Contains(again, "libact__why") || strings.Contains(again, "Subscribed</span>") {
		t.Errorf("a second add should say why it did nothing; body=%s", again)
	}
	if code := postFormCode(
		w,
		"/sources/library/apply",
		url.Values{"pick": {"nope"}, "view": {"suggestion"}},
	); code != http.StatusBadRequest {
		t.Errorf("unknown pick: status = %d, want 400", code)
	}
}

func TestManageTagPoolAppendsLibraryLabels(t *testing.T) {
	w, _ := overviewLibraryWeb(t, config.FeedsDoc{Feeds: []config.Feed{
		{Name: "Mine", URL: "https://mine.test/f", Tags: []string{"go"}},
	}})
	out := get(t, w, "/sources/manage")

	if want := `data-all-tags="go,Open Source,Rust,Infra,Python"`; !strings.Contains(out, want) {
		t.Errorf("missing %q", want)
	}
}

func TestWithLibraryLabelsWithoutFeeds(t *testing.T) {
	w, _ := overviewLibraryWeb(t, config.FeedsDoc{})
	got := withLibraryLabels(nil, w.library.Snapshot())
	if want := []string{"Go", "Open Source", "Rust", "Infra", "Python"}; !slices.Equal(got, want) {
		t.Errorf("labels = %v, want %v", got, want)
	}
	if got := withLibraryLabels(
		[]string{"x"},
		library.NewLoader(library.Options{}).Snapshot(),
	); !slices.Equal(
		got,
		[]string{"x"},
	) {
		t.Errorf("not loaded changed the pool: %v", got)
	}
}
