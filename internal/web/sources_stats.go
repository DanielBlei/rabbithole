// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/DanielBlei/rabbithole/internal/store"
)

// Signal and volume thresholds.
const (
	minScored      = 10   // fewer scored items than this is "not enough data"
	noisyMinScored = 20   // a callout needs at least this many scored items
	quietDays      = 30   // an ok source whose newest item is this old has gone quiet
	noisyShare     = 0.15 // a high-signal share at or under this is noisy
)

// statsTTL bounds how stale the cached stats get. The aggregate costs about
// 200ms at 50k items, and mutation responses and live refreshes each ask for it.
const statsTTL = 60 * time.Second

// Spark geometry: an 8-bar chart, 64 by 20, with a 2px gap.
const (
	sparkW   = 64
	sparkH   = 20
	sparkGap = 2
)

// statsCache holds the last SourceStats result. It is replaced when it is older
// than statsTTL, when invalidate was called since it was computed, or when an
// ingest run started or finished since (the run key changed).
type statsCache struct {
	gen atomic.Uint64 // bumped by invalidate

	mu     sync.Mutex // held across a recompute, so concurrent callers share it
	ok     bool
	m      map[string]store.SourceStat
	at     time.Time
	genAt  uint64
	runKey string
}

func (c *statsCache) invalidate() { c.gen.Add(1) }

// clock is the time the Sources page measures from.
func (s *Web) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// runKey identifies the ingest state from the newest run (nil when none): it
// differs while a run is live and once a run has started or finished, which is when stored items and scores move.
func (s *Web) runKey(last *store.IngestRun) string {
	key := strconv.FormatBool(s.ing.Status().Running)
	if last == nil {
		return key
	}
	key += fmt.Sprintf("/%d", last.ID)
	if last.FinishedAt != nil {
		key += fmt.Sprintf("/%d", last.FinishedAt.UnixNano())
	}
	return key
}

// sourceStats returns the per-source stats keyed by source name. The map is
// shared: callers must not modify it.
func (s *Web) sourceStats(
	ctx context.Context,
	last *store.IngestRun,
	now time.Time,
) (map[string]store.SourceStat, error) {
	key := s.runKey(last)
	c := &s.srcStats
	c.mu.Lock()
	defer c.mu.Unlock()
	gen := c.gen.Load()
	if c.ok && c.genAt == gen && c.runKey == key && now.Sub(c.at) < statsTTL {
		return c.m, nil
	}
	m, err := s.db.SourceStats(ctx, now, store.SourceStatsOpts{
		High: highSignalScore, Mid: midSignalScore, ThumbUp: rateUp, ThumbDown: rateDown,
	})
	if err != nil {
		return nil, err
	}
	c.m, c.at, c.genAt, c.runKey, c.ok = m, now, gen, key, true
	return m, nil
}

// highPlus is how the high tier reads in a count: "7+".
func highPlus() string { return strconv.Itoa(highSignalScore) + "+" }

// tierWords words the score tiers' bounds for the templates: "high" is the
// legend's top tier, "mid" and "low" the ones below it, and "atleast" the bound
// a share is measured against.
func tierWords(key string) string {
	switch key {
	case "high":
		return fmt.Sprintf("%d and up", highSignalScore)
	case "mid":
		return fmt.Sprintf("%d to %d", midSignalScore, highSignalScore-1)
	case "low":
		return fmt.Sprintf("under %d", midSignalScore)
	case "atleast":
		return fmt.Sprintf("%d or higher", highSignalScore)
	}
	return ""
}

// sparkBar is one bar of the volume chart, in the svg's own coordinates.
type sparkBar struct {
	X, W  float64
	Y, H  int
	Class string
}

// tierSeg is one stretch of the tier bar; its width is proportional to N.
type tierSeg struct {
	Class string // high, mid, low or none
	N     int
	Title string
}

// tierBar is the stacked bar of score tiers.
type tierBar struct {
	Segs  []tierSeg // tiers with items, in order
	Empty bool
	Label string
}

// sigStats is a source's signal over the last 30 days.
type sigStats struct {
	Scored, High, Mid, Low, Unscored int
	// Enough is whether Scored is enough for Share to mean anything.
	Enough bool
	// Share is High over Scored, 0 without scored items, and Pct is it rounded.
	Share float64
	Pct   int
	Bar   tierBar
}

// engStats is what was done with the last 30 days' items. The store keeps no
// read timestamp, so these are the current state of the items stored in the
// window, not what happened inside it.
type engStats struct {
	Read, Hidden, Bookmarked, Up, Down int
}

// rowStats is a row's volume, signal, activity and fetch time.
type rowStats struct {
	PerWeek     float64
	PerWeekText string
	Spark       []sparkBar
	SparkLabel  string

	Last       time.Time // newest item ever stored, zero when none
	LastPhrase string

	// Quiet is an ok source with no new item for QuietDays days or more.
	Quiet     bool
	QuietDays int

	Sig sigStats
	Eng engStats

	// AvgMs is the mean fetch time of the successful recent attempts, LastMs the
	// latest fetch's; zero when unknown.
	AvgMs, LastMs     int
	AvgText, LastText string
}

// applyStats fills the row's stats from the store's aggregate and its health.
func (r *feedRowData) applyStats(st store.SourceStat, h store.FeedHealth, now time.Time) {
	var rs rowStats
	rs.PerWeek = st.PerWeek()
	rs.PerWeekText = perWeekText(rs.PerWeek)
	rs.Spark = sparkBars(st.Weeks[:])
	rs.SparkLabel = sparkLabel(st.Weeks[:])

	if !st.Last.IsZero() {
		rs.Last = st.Last
		rs.LastPhrase = agoPhrase(st.Last, now)
		if r.State == feedStateOK && now.Sub(st.Last) >= quietDays*24*time.Hour {
			rs.Quiet = true
			rs.QuietDays = int(now.Sub(st.Last) / (24 * time.Hour))
		}
	}

	rs.Sig = sigStats{
		Scored: st.Scored, High: st.High, Mid: st.Mid, Low: st.Low, Unscored: st.Unscored,
		Enough: st.Scored >= minScored,
	}
	if st.Scored > 0 {
		rs.Sig.Share = float64(st.High) / float64(st.Scored)
		rs.Sig.Pct = int(math.Round(100 * rs.Sig.Share))
	}
	rs.Sig.Bar = tierBarOf(st.High, st.Mid, st.Low, st.Unscored)
	rs.Eng = engStats{
		Read: st.Read, Hidden: st.Skipped, Bookmarked: st.Bookmarked,
		Up: st.ThumbsUp, Down: st.ThumbsDown,
	}

	rs.AvgMs = avgFetchMs(h.Recent)
	rs.LastMs = int(h.Elapsed.Round(time.Millisecond) / time.Millisecond)
	rs.AvgText, rs.LastText = fmtMs(rs.AvgMs), fmtMs(rs.LastMs)
	r.Stats = rs
}

// avgFetchMs is the mean fetch time in milliseconds over the successful
// attempts that recorded one; zero when there are none.
func avgFetchMs(recent []store.FeedAttempt) int {
	var sum time.Duration
	n := 0
	for _, a := range recent {
		if a.OK() && a.Elapsed > 0 {
			sum += a.Elapsed
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return int((sum / time.Duration(n)).Round(time.Millisecond) / time.Millisecond)
}

// fmtMs words a duration in milliseconds: "420 ms", "1.5 s", "12 s". Zero or
// less is unknown and gives "".
func fmtMs(ms int) string {
	switch {
	case ms <= 0:
		return ""
	case ms >= 10000:
		return strconv.Itoa((ms+500)/1000) + " s"
	case ms >= 1000:
		tenths := (ms + 50) / 100
		if tenths >= 100 {
			return "10 s"
		}
		return fmt.Sprintf("%d.%d s", tenths/10, tenths%10)
	default:
		return strconv.Itoa(ms) + " ms"
	}
}

// perWeekText words a weekly rate: whole from 10 up, else to one decimal with a
// trailing ".0" dropped.
func perWeekText(v float64) string {
	if v >= 10 {
		return strconv.Itoa(int(math.Round(v)))
	}
	return strconv.FormatFloat(math.Round(v*10)/10, 'f', -1, 64)
}

// sparkBars lays out weeks (oldest first) as bars in a sparkW by sparkH box. A
// zero week is a 1px stub, any other week is at least 2px, and the tallest
// fills the box.
func sparkBars(weeks []int) []sparkBar {
	n := len(weeks)
	if n == 0 {
		return nil
	}
	maxV := 1
	for _, v := range weeks {
		maxV = max(maxV, v)
	}
	bw := float64(sparkW-sparkGap*(n-1)) / float64(n)
	bars := make([]sparkBar, n)
	for i, v := range weeks {
		h := 1
		if v > 0 {
			h = max(2, int(math.Round(float64(v)/float64(maxV)*(sparkH-1))))
		}
		class := "spark__b"
		if v == 0 {
			class += " is-zero"
		}
		if i == n-1 {
			class += " is-last"
		}
		bars[i] = sparkBar{
			X: math.Round(float64(i)*(bw+sparkGap)*100) / 100, W: math.Round(bw*100) / 100,
			Y: sparkH - h, H: h, Class: class,
		}
	}
	return bars
}

// sparkLabel is the spark's accessible name.
func sparkLabel(weeks []int) string {
	parts := make([]string, len(weeks))
	for i, v := range weeks {
		parts[i] = strconv.Itoa(v)
	}
	return "Stored items per week, oldest to newest: " + strings.Join(parts, ", ")
}

// tierBarOf builds the tier bar from the four tier counts.
func tierBarOf(high, mid, low, unscored int) tierBar {
	b := tierBar{Label: fmt.Sprintf("%d high, %d mid, %d low, %d unscored", high, mid, low, unscored)}
	for _, t := range []struct {
		class, word string
		n           int
	}{{"high", "high", high}, {"mid", "mid", mid}, {"low", "low", low}, {"none", "unscored", unscored}} {
		if t.n > 0 {
			b.Segs = append(b.Segs, tierSeg{Class: t.class, N: t.n, Title: strconv.Itoa(t.n) + " " + t.word})
		}
	}
	b.Empty = len(b.Segs) == 0
	return b
}

// addStats fills every row's stats. On a read failure the rows keep zero stats
// and the page carries a note, the way a health read failure degrades.
func (s *Web) addStats(
	ctx context.Context, data *sourcesData, health map[string]store.FeedHealth, last *store.IngestRun, now time.Time,
) {
	stats, err := s.sourceStats(ctx, last, now)
	if err != nil {
		log.Error().Err(err).Msg("read source stats")
		if data.Error == "" {
			data.Error = "source stats unavailable: " + err.Error()
		}
	}
	for i := range data.Rows {
		row := &data.Rows[i]
		row.applyStats(stats[row.Name], health[row.ID], now)
	}
}
