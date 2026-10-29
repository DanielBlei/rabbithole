// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"net/url"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/store"
)

func TestFmtMs(t *testing.T) {
	cases := []struct {
		ms   int
		want string
	}{
		{-5, ""},
		{0, ""},
		{1, "1 ms"},
		{999, "999 ms"},
		{1000, "1.0 s"},
		{1049, "1.0 s"},
		{1050, "1.1 s"},
		{1500, "1.5 s"},
		{9900, "9.9 s"},
		{9949, "9.9 s"},
		{9950, "10 s"},
		{9999, "10 s"},
		{10000, "10 s"},
		{10499, "10 s"},
		{10500, "11 s"},
		{125000, "125 s"},
	}
	for _, c := range cases {
		if got := fmtMs(c.ms); got != c.want {
			t.Errorf("fmtMs(%d) = %q, want %q", c.ms, got, c.want)
		}
	}
}

func TestPerWeekText(t *testing.T) {
	cases := []struct {
		v    float64
		want string
	}{
		{0, "0"},
		{0.04, "0"},
		{0.125, "0.1"},
		{1, "1"},
		{2.5, "2.5"},
		{9.94, "9.9"},
		{9.95, "10"},
		{9.99, "10"},
		{10, "10"},
		{10.4, "10"},
		{12.5, "13"},
		{250, "250"},
	}
	for _, c := range cases {
		if got := perWeekText(c.v); got != c.want {
			t.Errorf("perWeekText(%v) = %q, want %q", c.v, got, c.want)
		}
	}
}

func TestSparkBars(t *testing.T) {
	xs := []float64{0, 8.25, 16.5, 24.75, 33, 41.25, 49.5, 57.75}
	t.Run("all zero", func(t *testing.T) {
		got := sparkBars(make([]int, 8))
		if len(got) != 8 {
			t.Fatalf("got %d bars, want 8", len(got))
		}
		for i, b := range got {
			want := sparkBar{X: xs[i], W: 6.25, Y: 19, H: 1, Class: "spark__b is-zero"}
			if i == 7 {
				want.Class = "spark__b is-zero is-last"
			}
			if b != want {
				t.Errorf("bar %d = %+v, want %+v", i, b, want)
			}
		}
	})

	t.Run("single non-zero fills the box", func(t *testing.T) {
		got := sparkBars([]int{0, 0, 0, 5, 0, 0, 0, 0})
		for i, b := range got {
			if i == 3 {
				if b.H != 19 || b.Y != 1 || b.Class != "spark__b" {
					t.Errorf("tall bar = %+v, want H 19 Y 1 plain", b)
				}
				continue
			}
			if b.H != 1 || b.Y != 19 {
				t.Errorf("bar %d = %+v, want a 1px stub", i, b)
			}
		}
	})

	t.Run("last bar is marked even when non-zero", func(t *testing.T) {
		got := sparkBars([]int{1, 0, 0, 0, 0, 0, 0, 4})
		if got[7].Class != "spark__b is-last" || got[7].H != 19 {
			t.Errorf("last = %+v", got[7])
		}
		if got[0].Class != "spark__b" {
			t.Errorf("first = %+v", got[0])
		}
	})

	t.Run("heights scale and never drop under 2px", func(t *testing.T) {
		got := sparkBars([]int{1, 50, 100, 0, 0, 0, 0, 0})
		if got[0].H != 2 || got[0].Y != 18 {
			t.Errorf("small bar = %+v, want 2px", got[0])
		}
		if got[1].H != 10 {
			t.Errorf("half bar H = %d, want 10", got[1].H)
		}
		if got[2].H != 19 {
			t.Errorf("max bar H = %d, want 19", got[2].H)
		}
	})

	t.Run("geometry of the nonzero shape", func(t *testing.T) {
		got := sparkBars([]int{3, 3, 3, 3, 3, 3, 3, 3})
		for i, b := range got {
			if b.X != xs[i] || b.W != 6.25 || b.H != 19 {
				t.Errorf("bar %d = %+v", i, b)
			}
		}
	})

	if sparkBars(nil) != nil {
		t.Error("no weeks should give no bars")
	}
}

func TestSparkLabel(t *testing.T) {
	got := sparkLabel([]int{0, 1, 2, 3, 4, 5, 6, 70})
	want := "Stored items per week, oldest to newest: 0, 1, 2, 3, 4, 5, 6, 70"
	if got != want {
		t.Errorf("sparkLabel = %q, want %q", got, want)
	}
}

func TestTierBarOf(t *testing.T) {
	b := tierBarOf(3, 0, 2, 1)
	wantSegs := []tierSeg{
		{Class: "high", N: 3, Title: "3 high"},
		{Class: "low", N: 2, Title: "2 low"},
		{Class: "none", N: 1, Title: "1 unscored"},
	}
	if !reflect.DeepEqual(b.Segs, wantSegs) {
		t.Errorf("segs = %+v, want %+v", b.Segs, wantSegs)
	}
	if b.Label != "3 high, 0 mid, 2 low, 1 unscored" || b.Empty {
		t.Errorf("bar = %+v", b)
	}

	empty := tierBarOf(0, 0, 0, 0)
	if !empty.Empty || len(empty.Segs) != 0 || empty.Label != "0 high, 0 mid, 0 low, 0 unscored" {
		t.Errorf("empty bar = %+v", empty)
	}
}

func TestAvgFetchMs(t *testing.T) {
	ok := func(ms int) store.FeedAttempt {
		return store.FeedAttempt{Status: store.FeedStatusOK, Elapsed: time.Duration(ms) * time.Millisecond}
	}
	failed := store.FeedAttempt{Status: store.FeedStatusError, Elapsed: 30 * time.Second}
	cases := []struct {
		name   string
		recent []store.FeedAttempt
		want   int
	}{
		{"none", nil, 0},
		{"only failures", []store.FeedAttempt{failed, failed}, 0},
		{"ignores errors", []store.FeedAttempt{ok(100), failed, ok(300)}, 200},
		{"ignores zero elapsed", []store.FeedAttempt{ok(0), ok(400), ok(0)}, 400},
		{"only zero elapsed", []store.FeedAttempt{ok(0), ok(0)}, 0},
		{"rounds to ms", []store.FeedAttempt{ok(1), ok(2)}, 2},
	}
	for _, c := range cases {
		if got := avgFetchMs(c.recent); got != c.want {
			t.Errorf("%s: avgFetchMs = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestApplyStats(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour

	t.Run("quiet at exactly 30 days", func(t *testing.T) {
		row := feedRowData{State: feedStateOK}
		row.applyStats(store.SourceStat{Last: now.Add(-30 * day)}, store.FeedHealth{}, now)
		if !row.Stats.Quiet || row.Stats.QuietDays != 30 {
			t.Errorf("30 days: quiet=%v days=%d", row.Stats.Quiet, row.Stats.QuietDays)
		}
		row = feedRowData{State: feedStateOK}
		row.applyStats(store.SourceStat{Last: now.Add(-30*day + time.Second)}, store.FeedHealth{}, now)
		if row.Stats.Quiet {
			t.Error("just under 30 days should not be quiet")
		}
	})

	t.Run("quiet needs an ok source with items", func(t *testing.T) {
		old := store.SourceStat{Last: now.Add(-90 * day)}
		for _, state := range []string{feedStateError, feedStateFlaky, feedStateNever, feedStateOff} {
			row := feedRowData{State: state}
			row.applyStats(old, store.FeedHealth{}, now)
			if row.Stats.Quiet {
				t.Errorf("state %s should not be quiet", state)
			}
		}
		row := feedRowData{State: feedStateOK}
		row.applyStats(store.SourceStat{}, store.FeedHealth{}, now)
		if row.Stats.Quiet || !row.Stats.Last.IsZero() || row.Stats.LastPhrase != "" {
			t.Errorf("no items: %+v", row.Stats)
		}
		row = feedRowData{State: feedStateOK}
		row.applyStats(old, store.FeedHealth{}, now)
		if !row.Stats.Quiet || row.Stats.QuietDays != 90 || row.Stats.LastPhrase == "" {
			t.Errorf("90 days: %+v", row.Stats)
		}
	})

	t.Run("enough at 10 scored", func(t *testing.T) {
		row := feedRowData{}
		row.applyStats(store.SourceStat{Scored: 9, High: 3, Mid: 3, Low: 3}, store.FeedHealth{}, now)
		if row.Stats.Sig.Enough {
			t.Error("9 scored should not be enough")
		}
		row = feedRowData{}
		row.applyStats(store.SourceStat{Scored: 10, High: 3, Mid: 3, Low: 4, Unscored: 2}, store.FeedHealth{}, now)
		sig := row.Stats.Sig
		if !sig.Enough || sig.Share != 0.3 || sig.Pct != 30 {
			t.Errorf("10 scored: %+v", sig)
		}
		if sig.Bar.Label != "3 high, 3 mid, 4 low, 2 unscored" {
			t.Errorf("bar label = %q", sig.Bar.Label)
		}
	})

	t.Run("share without scored items", func(t *testing.T) {
		row := feedRowData{}
		row.applyStats(store.SourceStat{Unscored: 4}, store.FeedHealth{}, now)
		if sig := row.Stats.Sig; sig.Share != 0 || sig.Pct != 0 || sig.Enough || sig.Unscored != 4 {
			t.Errorf("sig = %+v", sig)
		}
	})

	t.Run("share rounds", func(t *testing.T) {
		row := feedRowData{}
		row.applyStats(store.SourceStat{Scored: 30, High: 10, Mid: 10, Low: 10}, store.FeedHealth{}, now)
		if row.Stats.Sig.Pct != 33 {
			t.Errorf("pct = %d, want 33", row.Stats.Sig.Pct)
		}
	})

	t.Run("volume engagement and fetch time", func(t *testing.T) {
		st := store.SourceStat{
			Weeks: [8]int{0, 0, 0, 0, 0, 0, 4, 12},
			Read:  5, Skipped: 6, Bookmarked: 2, ThumbsUp: 1, ThumbsDown: 3,
			Last: now.Add(-3 * time.Hour),
		}
		h := store.FeedHealth{
			Elapsed: 1500 * time.Millisecond,
			Recent: []store.FeedAttempt{
				{Status: store.FeedStatusOK, Elapsed: 200 * time.Millisecond},
				{Status: store.FeedStatusError, Elapsed: 9 * time.Second},
				{Status: store.FeedStatusOK, Elapsed: 400 * time.Millisecond},
			},
		}
		row := feedRowData{State: feedStateOK}
		row.applyStats(st, h, now)
		got := row.Stats
		if got.PerWeek != 2 || got.PerWeekText != "2" || len(got.Spark) != 8 {
			t.Errorf("volume = %v %q %d bars", got.PerWeek, got.PerWeekText, len(got.Spark))
		}
		if got.LastPhrase != "3h ago" {
			t.Errorf("last phrase = %q", got.LastPhrase)
		}
		if want := (engStats{Read: 5, Hidden: 6, Bookmarked: 2, Up: 1, Down: 3}); got.Eng != want {
			t.Errorf("eng = %+v, want %+v", got.Eng, want)
		}
		if got.AvgMs != 300 || got.AvgText != "300 ms" || got.LastMs != 1500 || got.LastText != "1.5 s" {
			t.Errorf("fetch time = %+v", got)
		}
	})

	t.Run("no fetch time", func(t *testing.T) {
		row := feedRowData{}
		row.applyStats(store.SourceStat{}, store.FeedHealth{}, now)
		if s := row.Stats; s.AvgMs != 0 || s.LastMs != 0 || s.AvgText != "" || s.LastText != "" {
			t.Errorf("stats = %+v", s)
		}
	})
}

// statsTestWeb is a Web over two sources with a clock the test moves.
func statsTestWeb(t *testing.T) (*Web, *time.Time) {
	t.Helper()
	w, _ := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{
		{Name: "Alpha", URL: "https://alpha.test/feed"},
		{Name: "Beta", URL: "https://beta.test/feed"},
	}})
	clock := time.Now()
	w.now = func() time.Time { return clock }
	return w, &clock
}

// itemAdder returns a func recording one more item under a source, numbering
// the test's items so their IDs stay unique.
func itemAdder(t *testing.T, db *store.Store) func(source string) {
	t.Helper()
	n := 0
	return func(source string) {
		t.Helper()
		n++
		id := source + "-" + strconv.Itoa(n)
		if err := db.Record(t.Context(),
			[]feeds.Item{{ID: id, Source: source, Title: id, Link: "https://x.test/" + id}},
			nil, time.Now()); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
}

// itemsOf is the 8-week total the stats report for a source.
func itemsOf(t *testing.T, w *Web, source string) int {
	t.Helper()
	last, err := w.db.LastIngestRun(t.Context())
	if err != nil {
		t.Fatalf("LastIngestRun: %v", err)
	}
	m, err := w.sourceStats(t.Context(), last, w.clock())
	if err != nil {
		t.Fatalf("sourceStats: %v", err)
	}
	n := 0
	for _, v := range m[source].Weeks {
		n += v
	}
	return n
}

func TestSourceStatsCacheTTL(t *testing.T) {
	w, clock := statsTestWeb(t)
	add := itemAdder(t, w.db)
	add("Alpha")
	if got := itemsOf(t, w, "Alpha"); got != 1 {
		t.Fatalf("first read = %d, want 1", got)
	}

	add("Alpha")
	*clock = clock.Add(statsTTL - time.Second)
	if got := itemsOf(t, w, "Alpha"); got != 1 {
		t.Errorf("within the TTL = %d, want the cached 1", got)
	}

	*clock = clock.Add(time.Second)
	if got := itemsOf(t, w, "Alpha"); got != 2 {
		t.Errorf("at the TTL = %d, want a fresh 2", got)
	}

	add("Alpha")
	*clock = clock.Add(time.Second)
	if got := itemsOf(t, w, "Alpha"); got != 2 {
		t.Errorf("after the refresh = %d, want the cached 2", got)
	}
}

func TestSourceStatsCacheInvalidatedByRename(t *testing.T) {
	w, _ := statsTestWeb(t)
	add := itemAdder(t, w.db)
	add("Alpha")
	if got := itemsOf(t, w, "Alpha"); got != 1 {
		t.Fatalf("first read = %d, want 1", got)
	}
	add("Alpha")
	if got := itemsOf(t, w, "Alpha"); got != 1 {
		t.Fatalf("cached read = %d, want 1", got)
	}

	id := feedID(t, w.db, "Alpha")
	postForm(t, w, "/sources/feed/"+id, url.Values{"name": {"Alpha 2"}, "url": {"https://alpha.test/feed"}})
	if got := itemsOf(t, w, "Alpha 2"); got != 2 {
		t.Errorf("after the rename = %d under the new name, want a fresh 2", got)
	}
	if got := itemsOf(t, w, "Alpha"); got != 0 {
		t.Errorf("old name still has %d", got)
	}
}

// Edits that cannot move the per-source aggregate keep the cache.
func TestSourceStatsCacheKeptByToggle(t *testing.T) {
	w, _ := statsTestWeb(t)
	add := itemAdder(t, w.db)
	add("Beta")
	if got := itemsOf(t, w, "Beta"); got != 1 {
		t.Fatalf("first read = %d, want 1", got)
	}
	add("Beta")

	id := feedID(t, w.db, "Beta")
	postForm(t, w, "/sources/feed/"+id+"/enabled", url.Values{"enabled": {"off"}})
	if got := itemsOf(t, w, "Beta"); got != 1 {
		t.Errorf("after a toggle = %d, want the cached 1", got)
	}
}

// A new source has no items, so the cached map stays valid.
func TestSourceStatsCacheKeptByAdd(t *testing.T) {
	w, _ := statsTestWeb(t)
	add := itemAdder(t, w.db)
	add("Alpha")
	itemsOf(t, w, "Alpha")
	add("Alpha")
	postForm(t, w, "/sources", url.Values{"name": {"Gamma"}, "url": {"https://gamma.test/feed"}})
	if got := itemsOf(t, w, "Alpha"); got != 1 {
		t.Errorf("after an add = %d, want the cached 1", got)
	}
	if got := itemsOf(t, w, "Gamma"); got != 0 {
		t.Errorf("new source = %d items, want 0", got)
	}
}

// An ingest run starting or finishing drops the cache without any call from
// the web layer.
func TestSourceStatsCacheInvalidatedByIngestRun(t *testing.T) {
	w := newIngestWeb(t)
	add := itemAdder(t, w.db)
	clock := time.Now()
	w.now = func() time.Time { return clock }
	add("Alpha")
	if got := itemsOf(t, w, "Alpha"); got != 1 {
		t.Fatalf("first read = %d, want 1", got)
	}
	add("Alpha")
	if got := itemsOf(t, w, "Alpha"); got != 1 {
		t.Fatalf("cached read = %d, want 1", got)
	}

	post(t, w, "/ingest/run")
	deadline := time.Now().Add(5 * time.Second)
	for {
		last, err := w.db.LastIngestRun(t.Context())
		if err != nil {
			t.Fatalf("LastIngestRun: %v", err)
		}
		if last != nil && last.Status != store.IngestStatusRunning && !w.ing.Status().Running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("run did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := itemsOf(t, w, "Alpha"); got != 2 {
		t.Errorf("after a run = %d, want a fresh 2", got)
	}
	add("Alpha")
	if got := itemsOf(t, w, "Alpha"); got != 2 {
		t.Errorf("run key stable = %d, want the cached 2", got)
	}
}

func TestSourcesDataWithStats(t *testing.T) {
	w, _ := statsTestWeb(t)
	add := itemAdder(t, w.db)
	add("Alpha")

	rowOf := func(d sourcesData, name string) feedRowData {
		t.Helper()
		for _, r := range d.Rows {
			if r.Name == name {
				return r
			}
		}
		t.Fatalf("no row %q", name)
		return feedRowData{}
	}

	d, err := w.sourcesData(t.Context(), "", true)
	if err != nil {
		t.Fatalf("sourcesData: %v", err)
	}
	if a := rowOf(d, "Alpha"); a.Stats.PerWeek != 0.125 || len(a.Stats.Spark) != 8 || a.Stats.PerWeekText != "0.1" {
		t.Errorf("Alpha stats = %+v", a.Stats)
	}
	if b := rowOf(d, "Beta"); len(b.Stats.Spark) != 8 || b.Stats.PerWeek != 0 || !b.Stats.Last.IsZero() {
		t.Errorf("Beta stats = %+v", b.Stats)
	}
}

// The Library page has no use for stats, so it neither reads nor fills them.
func TestSourcesPageDataLibrarySkipsStats(t *testing.T) {
	w, _ := statsTestWeb(t)
	add := itemAdder(t, w.db)
	add("Alpha")

	d, err := w.sourcesPageData(t.Context(), "", "library")
	if err != nil {
		t.Fatalf("sourcesPageData: %v", err)
	}
	if w.srcStats.ok {
		t.Error("the Library page computed stats")
	}
	for _, r := range d.Rows {
		if r.Stats.Spark != nil || r.Stats.PerWeekText != "" {
			t.Errorf("row %s has stats: %+v", r.Name, r.Stats)
		}
	}

	if _, err := w.sourcesPageData(t.Context(), "", "manage"); err != nil {
		t.Fatalf("sourcesPageData: %v", err)
	}
	if !w.srcStats.ok {
		t.Error("the Manage page did not compute stats")
	}
}
