// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// statRow is an ok row with the given stats applied.
func statRow(name string, st store.SourceStat) feedRowData {
	url := "https://" + strings.ToLower(strings.ReplaceAll(name, " ", "")) + ".test/feed"
	row := feedRowData{ID: config.FeedID(url), Name: name, URL: url, State: feedStateOK}
	row.applyStats(st, store.FeedHealth{}, time.Now())
	return row
}

// scoredStat is a source with scored items, of which high are 7 or more.
func scoredStat(scored, high int) store.SourceStat {
	return store.SourceStat{Scored: scored, High: high, Low: scored - high}
}

func overviewOf(rows ...feedRowData) overviewData {
	return overviewFrom(sourcesData{Rows: rows})
}

func callNames(calls []ovCall) []string {
	var out []string
	for _, c := range calls {
		out = append(out, c.Name)
	}
	return out
}

func TestOverviewBrief(t *testing.T) {
	busy := store.SourceStat{Weeks: [8]int{0, 0, 0, 0, 0, 0, 0, 16}}
	cases := []struct {
		name string
		rows []feedRowData
		want string
	}{
		{"both", []feedRowData{
			statRow("Busy", busy),
			statRow("Sharp", scoredStat(30, 24)),
		}, "2 sources, all healthy. Busy brings the most items and Sharp has the best signal."},
		{"same source", []feedRowData{
			statRow("Both", store.SourceStat{Weeks: busy.Weeks, Scored: 30, High: 24, Low: 6}),
			statRow("Idle", store.SourceStat{}),
		}, "2 sources, all healthy. Both brings the most items and has the best signal."},
		{"most items only", []feedRowData{
			statRow("Busy", busy),
			statRow("Idle", store.SourceStat{}),
		}, "2 sources, all healthy. Busy brings the most items."},
		{"best signal only", []feedRowData{
			statRow("Sharp", scoredStat(30, 24)),
			statRow("Idle", store.SourceStat{}),
		}, "2 sources, all healthy. Sharp has the best signal."},
		{"neither", []feedRowData{
			statRow("Idle", store.SourceStat{}),
			statRow("Thin", scoredStat(12, 12)),
		}, "2 sources, all healthy."},
		{"quiet needs attention", []feedRowData{
			statRow("Old", store.SourceStat{Last: time.Now().Add(-40 * 24 * time.Hour)}),
		}, "1 source, 1 needs attention."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := overviewOf(c.rows...).Brief; got != c.want {
				t.Errorf("brief = %q, want %q", got, c.want)
			}
		})
	}
}

func TestOverviewQuietIsAttention(t *testing.T) {
	old := store.SourceStat{Last: time.Now().Add(-45*24*time.Hour - time.Hour)}
	quiet := statRow("Old", old)
	fresh := statRow("Fresh", store.SourceStat{Last: time.Now().Add(-29 * 24 * time.Hour)})
	failing := statRow("Failing", old)
	failing.State = feedStateError
	failing.Stats.Quiet = false

	ov := overviewOf(quiet, fresh)
	if len(ov.Quiet) != 1 || ov.Quiet[0].Name != "Old" || ov.Quiet[0].Days != 45 {
		t.Errorf("quiet = %+v", ov.Quiet)
	}
	if ov.AllClear {
		t.Error("AllClear with a source gone quiet")
	}
	if !overviewOf(fresh).AllClear {
		t.Error("AllClear is false with nothing to attend to")
	}
	if got := overviewOf(quiet, failing).Brief; !strings.HasPrefix(got, "2 sources, 2 need attention.") {
		t.Errorf("brief = %q", got)
	}
}

func TestManageStateTextGoneQuiet(t *testing.T) {
	row := feedRowData{State: feedStateOK, LastFetch: "2h ago"}
	if got := row.StateText(); got != "Fetched 2h ago" {
		t.Fatalf("state text = %q", got)
	}
	row.Stats.Quiet = true
	if got, tone := row.StateText(), row.StateTone(); got != "Gone quiet" || tone != "warn" {
		t.Errorf("state text = %q tone %q, want Gone quiet warn", got, tone)
	}
}

func TestOverviewVolumeOrderAndSkipped(t *testing.T) {
	slow := statRow("Slow", store.SourceStat{Weeks: [8]int{0, 0, 0, 0, 0, 0, 0, 8}})
	fast := statRow("Fast", store.SourceStat{Weeks: [8]int{0, 0, 0, 0, 0, 0, 40, 40}})
	mid := statRow("Mid", store.SourceStat{Weeks: [8]int{0, 0, 0, 0, 0, 0, 0, 24}})
	off := statRow("Off", store.SourceStat{})
	off.State = feedStateOff
	never := statRow("Never", store.SourceStat{})
	never.State = feedStateNever
	gone := statRow("Gone", store.SourceStat{Weeks: [8]int{99, 99, 99, 99, 99, 99, 99, 99}})
	gone.Deleted = true

	ov := overviewOf(slow, off, fast, never, mid, gone)
	var names []string
	for _, v := range ov.Volume {
		names = append(names, v.Name)
	}
	if want := []string{"Fast", "Mid", "Slow"}; !slices.Equal(names, want) {
		t.Errorf("volume order = %v, want %v", names, want)
	}
	if ov.VolumeSkipped != 2 {
		t.Errorf("skipped = %d, want 2", ov.VolumeSkipped)
	}
	if got, want := ov.SkippedNote(), "2 sources not shown: off or waiting for the first run."; got != want {
		t.Errorf("note = %q, want %q", got, want)
	}
	ov.VolumeSkipped = 1
	if got, want := ov.SkippedNote(), "1 source not shown: off or waiting for the first run."; got != want {
		t.Errorf("note = %q, want %q", got, want)
	}
	if top := ov.Volume[0]; top.PerWeekText != "10" || len(top.Spark) != 8 || top.Hint != "" {
		t.Errorf("top row = %+v", top)
	}
}

func TestOverviewVolumeHintForQuietSource(t *testing.T) {
	ov := overviewOf(statRow("Old", store.SourceStat{Last: time.Now().Add(-33*24*time.Hour - time.Hour)}))
	if len(ov.Volume) != 1 || ov.Volume[0].Hint != "No new items in 33 days" {
		t.Errorf("volume = %+v", ov.Volume)
	}
}

func TestOverviewSignalCallouts(t *testing.T) {
	t.Run("best is the top share with enough scored items", func(t *testing.T) {
		ov := overviewOf(
			statRow("Small", scoredStat(19, 19)),
			statRow("Good", scoredStat(20, 12)),
			statRow("Better", scoredStat(40, 36)),
		)
		best := ov.Signal.Best
		if best == nil || best.Name != "Better" || best.Hint != "36 of 40 scored are 7+ (90%)" {
			t.Errorf("best = %+v", best)
		}
	})

	t.Run("no best without a high share", func(t *testing.T) {
		ov := overviewOf(statRow("Dull", scoredStat(30, 0)), statRow("Duller", scoredStat(25, 0)))
		if ov.Signal.Best != nil {
			t.Errorf("best = %+v, want none", ov.Signal.Best)
		}
		if got := callNames(ov.Signal.Noisy); !slices.Equal(got, []string{"Dull", "Duller"}) {
			t.Errorf("noisy = %v", got)
		}
	})

	t.Run("noisy thresholds", func(t *testing.T) {
		ov := overviewOf(
			statRow("Best", scoredStat(20, 20)),
			statRow("Edge", scoredStat(20, 3)),        // 15%: noisy
			statRow("Over", scoredStat(20, 4)),        // 20%: not noisy
			statRow("Short", scoredStat(19, 0)),       // too few scored
			statRow("Quiet noise", scoredStat(50, 5)), // 10%: noisy
		)
		if got := callNames(ov.Signal.Noisy); !slices.Equal(got, []string{"Quiet noise", "Edge"}) {
			t.Errorf("noisy = %v, want [Quiet noise Edge] by share ascending", got)
		}
	})

	t.Run("noisy is capped at three, worst first, best left out", func(t *testing.T) {
		ov := overviewOf(
			statRow("A", scoredStat(30, 4)),
			statRow("B", scoredStat(30, 3)),
			statRow("C", scoredStat(30, 2)),
			statRow("D", scoredStat(30, 1)),
			statRow("E", scoredStat(30, 0)),
		)
		if got := callNames(ov.Signal.Noisy); !slices.Equal(got, []string{"E", "D", "C"}) {
			t.Errorf("noisy = %v, want [E D C]", got)
		}
		if ov.Signal.Best == nil || ov.Signal.Best.Name != "A" {
			t.Errorf("best = %+v, want A", ov.Signal.Best)
		}
		for _, c := range ov.Signal.Noisy {
			if c.Name == "A" {
				t.Error("best is also listed as noisy")
			}
		}
	})
}

func TestOverviewSignalRows(t *testing.T) {
	enough := statRow(
		"Enough",
		store.SourceStat{
			Scored:     20,
			High:       5,
			Mid:        5,
			Low:        10,
			Unscored:   3,
			Read:       4,
			Skipped:    2,
			Bookmarked: 1,
			ThumbsUp:   3,
			ThumbsDown: 1,
		},
	)
	more := statRow("More", scoredStat(30, 27))
	thin := statRow("Thin", store.SourceStat{Scored: 8, High: 8})
	thinner := statRow("Thinner", store.SourceStat{Scored: 2, High: 1, Low: 1})
	ov := overviewOf(thinner, enough, thin, more)

	var names []string
	for _, r := range ov.Signal.Rows {
		names = append(names, r.Name)
	}
	if want := []string{"More", "Enough", "Thin", "Thinner"}; !slices.Equal(names, want) {
		t.Fatalf("row order = %v, want %v", names, want)
	}
	rows := ov.Signal.Rows
	if rows[1].Lead != "25%" || rows[1].Hint != "5 of 20 scored are 7+, 3 unscored" || rows[1].Thin {
		t.Errorf("enough row = %+v", rows[1])
	}
	if want := "read 4 · hidden 2 · bookmarked 1 · thumbs 3 up, 1 down"; rows[1].Eng != want {
		t.Errorf("engagement = %q, want %q", rows[1].Eng, want)
	}
	if rows[0].Hint != "27 of 30 scored are 7+" {
		t.Errorf("no unscored suffix expected: %q", rows[0].Hint)
	}
	if !rows[2].Thin || rows[2].Lead != "Not enough data yet" || rows[2].Hint != "8 scored, 10 needed" {
		t.Errorf("thin row = %+v", rows[2])
	}
}

func TestOverviewSlowest(t *testing.T) {
	timed := func(name string, ms int) feedRowData {
		r := statRow(name, store.SourceStat{})
		r.Stats.AvgMs, r.Stats.AvgText = ms, fmtMs(ms)
		return r
	}
	off := timed("Off", 9000)
	off.State = feedStateOff
	ov := overviewOf(timed("A", 400), timed("B", 2000), off, timed("C", 1000), timed("D", 10), timed("None", 0))

	var got []string
	for _, s := range ov.Slowest {
		got = append(got, fmt.Sprintf("%s %s %d", s.Name, s.AvgText, s.Width))
	}
	if want := []string{"B 2.0 s 100", "C 1.0 s 50", "A 400 ms 20"}; !slices.Equal(got, want) {
		t.Errorf("slowest = %v, want %v", got, want)
	}

	narrow := overviewOf(timed("Huge", 100000), timed("Tiny", 10))
	if w := narrow.Slowest[1].Width; w != minBarPct {
		t.Errorf("narrow bar = %d, want %d", w, minBarPct)
	}
	if len(overviewOf(timed("None", 0)).Slowest) != 0 {
		t.Error("a source without a fetch time is listed")
	}
}

// seedItems stores items for source: high (score 8), mid (5), low (2) and
// unscored ones.
func seedItems(t *testing.T, db *store.Store, source string, high, mid, low, unscored int) {
	t.Helper()
	var all []feeds.Item
	var scored []store.DigestEntry
	add := func(n, score int, isScored bool) {
		for range n {
			id := fmt.Sprintf("%s-%d", source, len(all))
			it := feeds.Item{ID: id, Source: source, Title: id, Link: "https://items.test/" + id}
			all = append(all, it)
			if isScored {
				scored = append(scored, store.DigestEntry{Item: it, Score: score, Model: "m", Digested: true})
			}
		}
	}
	add(high, 8, true)
	add(mid, 5, true)
	add(low, 2, true)
	add(unscored, 0, false)
	if err := db.Record(t.Context(), all, scored, time.Now()); err != nil {
		t.Fatalf("Record: %v", err)
	}
}

// recordTimed stores one successful fetch of feed that took elapsed.
func recordTimed(t *testing.T, db *store.Store, feed config.Feed, elapsed time.Duration) {
	t.Helper()
	err := db.RecordFeedFetches(t.Context(), []store.FeedFetch{{
		FeedID: config.FeedID(feed.URL), Name: feed.Name, Items: 3, Elapsed: elapsed, At: time.Now().Add(-time.Hour),
	}})
	if err != nil {
		t.Fatalf("RecordFeedFetches: %v", err)
	}
}

func overviewSectionsWeb(t *testing.T) (*Web, *store.Store) {
	t.Helper()
	alpha := config.Feed{Name: "Alpha", URL: "https://alpha.test/feed"}
	beta := config.Feed{Name: "Beta", URL: "https://beta.test/feed"}
	delta := config.Feed{Name: "Delta", URL: "https://delta.test/feed"}
	waiting := config.Feed{Name: "Waiting", URL: "https://waiting.test/feed"}
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{alpha, beta, delta, waiting}})
	recordTimed(t, db, alpha, 400*time.Millisecond)
	recordTimed(t, db, beta, 2*time.Second)
	recordTimed(t, db, delta, 100*time.Millisecond)
	seedItems(t, db, "Alpha", 20, 0, 5, 2)
	seedItems(t, db, "Beta", 2, 0, 20, 0)
	seedItems(t, db, "Delta", 1, 0, 2, 0)
	return w, db
}

func TestOverviewRendersSections(t *testing.T) {
	w, db := overviewSectionsWeb(t)
	out := get(t, w, "/sources")

	for _, want := range []string{
		// Brief and order of the sections.
		`<p class="ov-brief">4 sources, all healthy. Alpha brings the most items and has the best signal.</p>`,
		// Volume.
		`Stored items per week, last 8 weeks. Retention and the since window cut older history.`,
		`<span class="ov-cw-spark">Last 8 weeks</span><span class="ov-cw-num">Stored items</span>`,
		`aria-label="Stored items per week, oldest to newest: 0, 0, 0, 0, 0, 0, 0, 27"`,
		`<rect class="spark__b is-last"`,
		`<b>3.4</b><span class="sh-hint"> /wk</span>`,
		`1 source not shown: off or waiting for the first run.`,
		`href="/sources/manage#src-` + feedID(t, db, "Alpha") + `"`,
		// Signal.
		`How many scored items are 7 or higher, per source.`,
		`Best signal`, `20 of 25 scored are 7&#43; (80%)`,
		`Noisy, low signal`, `2 of 22 scored are 7&#43; (9%)`,
		`aria-label="Bar colours"`, `7 and up`, `4 to 6`, `under 4`, `unscored`,
		`read 0 · hidden 0 · bookmarked 0 · thumbs 0 up, 0 down`,
		`aria-label="20 high, 0 mid, 5 low, 2 unscored"`,
		`class="tier__s tier__s--high"`,
		`<b>80%</b><span class="sh-hint">20 of 25 scored are 7&#43;, 2 unscored</span>`,
		`<li class="ov-srow is-thin">`,
		`<b>Not enough data yet</b><span class="sh-hint">3 scored, 10 needed</span>`,
		`Last 30 days. Signal is the score from your profile, not read or thumbs counts.`,
		// Slowest.
		`Average fetch time. Slow feeds stretch every run.`,
		`aria-label="Average 2.0 s"`, `style="width:100%"`,
		`aria-label="Average 400 ms"`, `style="width:20%"`,
		`aria-label="Average 100 ms"`, `style="width:5%"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, name := range []string{"Alpha", "Beta"} {
		href := `href="/sources/manage?select=` + feedID(t, db, name) + `">Review in Manage</a>`
		if !strings.Contains(out, href) {
			t.Errorf("no Review in Manage link for %s; want %s", name, href)
		}
	}
	if n := strings.Count(out, ">Review in Manage</a>"); n != 2 {
		t.Errorf("%d Review in Manage links, want one per callout", n)
	}

	order := []string{
		"Sync and health",
		"Needs attention",
		">Volume<",
		">Signal<",
		"Library suggestions",
		"Library coverage",
		"Slowest feeds",
	}
	last := -1
	for _, h := range order {
		if h == "Library suggestions" || h == "Library coverage" {
			continue // only with a loaded library
		}
		i := strings.Index(out, h)
		if i < 0 || i < last {
			t.Errorf("section %q out of order (at %d, previous %d)", h, i, last)
		}
		last = i
	}
}

func TestOverviewSectionsOrderWithLibrary(t *testing.T) {
	w, _ := overviewLibraryWeb(t, config.FeedsDoc{Feeds: goFeeds()})
	out := get(t, w, "/sources")
	for _, fold := range []string{"attention", "volume", "signal", "suggestions", "coverage", "slowest"} {
		if !strings.Contains(out, `data-ov-fold="`+fold+`"`) {
			t.Errorf("Overview is missing the %q fold control", fold)
		}
	}
	last := -1
	for _, h := range []string{"Sync and health", "Needs attention", ">Volume<", ">Signal<", "Library suggestions", "Library coverage", "Slowest feeds"} {
		i := strings.Index(out, h)
		if i < 0 || i < last {
			t.Errorf("section %q out of order (at %d, previous %d)", h, i, last)
		}
		last = i
	}
}

func TestOverviewSectionsEmptyStates(t *testing.T) {
	w, _ := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}}})
	out := get(t, w, "/sources")

	for _, want := range []string{
		"Nothing fetched yet. Run ingestion to see volume.",
		"Nothing scored yet. Signal appears after the first run.",
		"No fetch times yet.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, not := range []string{"ov-vrow", "ov-srow", "ov-call", "not shown"} {
		if strings.Contains(out, not) {
			t.Errorf("empty overview contains %q", not)
		}
	}
}

// Rows without a single scored item read as nothing scored, not as an empty list.
func TestOverviewSignalNothingScored(t *testing.T) {
	ov := overviewOf(statRow("Idle", store.SourceStat{}), statRow("Fresh", store.SourceStat{Unscored: 4}))
	if len(ov.Signal.Rows) != 2 || ov.Signal.Scored {
		t.Fatalf("signal = %+v, want two rows and Scored false", ov.Signal)
	}
	var b strings.Builder
	if err := sourcesTmpl.ExecuteTemplate(&b, "sourcesOverview", sourcesData{Overview: ov}); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := b.String()
	if !strings.Contains(out, "Nothing scored yet. Signal appears after the first run.") {
		t.Error("missing the nothing-scored note")
	}
	if strings.Contains(out, "ov-srow") {
		t.Error("signal rows rendered with nothing scored")
	}

	if !overviewOf(statRow("Sharp", scoredStat(12, 6))).Signal.Scored {
		t.Error("Scored is false with a scored item")
	}
}

func TestOverviewRendersGoneQuiet(t *testing.T) {
	alpha := config.Feed{Name: "Alpha", URL: "https://alpha.test/feed"}
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{alpha}})
	recordFetches(t, db, alpha, 1, 0)
	seedItems(t, db, "Alpha", 0, 0, 0, 1)
	clock := time.Now().Add(60*24*time.Hour + time.Hour)
	w.now = func() time.Time { return clock }

	out := get(t, w, "/sources")
	for _, want := range []string{
		`<li class="ov-att ov-att--quiet">`,
		"Gone quiet: Alpha",
		"No new items in 60 days",
		`href="/sources/manage#src-` + feedID(t, db, "Alpha") + `">Open</a>`,
		`<p class="ov-brief">1 source, 1 needs attention.</p>`,
		"No new items in 60 days</span>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q; body=%s", want, out)
		}
	}
	if strings.Contains(out, "Nothing failing.") {
		t.Error("all-clear shown with a source gone quiet")
	}
	if manage := get(t, w, "/sources/manage"); !strings.Contains(manage, "Gone quiet") {
		t.Errorf("Manage row lacks the Gone quiet state; body=%s", manage)
	}
}

func TestOverviewLiveCarriesStatsSections(t *testing.T) {
	w, _ := overviewSectionsWeb(t)
	out := get(t, w, "/sources/live?page=overview")

	for _, s := range []string{`id="srcOverview"`, ">Volume<", ">Signal<", "Slowest feeds", "Best signal", `class="spark"`, `class="tier"`} {
		if !strings.Contains(out, s) {
			t.Errorf("live overview missing %q", s)
		}
	}
}
