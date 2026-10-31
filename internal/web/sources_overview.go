// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/rs/zerolog/log"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/library"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// maxSuggestions is how many Library sources the Overview suggests.
const maxSuggestions = 4

// Callout and slowest-list limits, and the narrowest a slowest bar is drawn.
const (
	maxNoisy   = 3
	maxSlowest = 3
	minBarPct  = 3
)

// overviewData is the Sources Overview page: the health of the live sources,
// what needs attention, and the last ingestion run. Removed sources are left
// out throughout.
type overviewData struct {
	Total int
	// FirstRun is set when there are no sources, which replaces the page with
	// the add-or-import prompt.
	FirstRun bool
	// Brief is the one-line summary under the heading.
	Brief string

	Health     []healthSeg
	HealthAria string
	// RateText is the fetch success rate, or "n/a" with nothing to measure.
	RateText  string
	RateOK    int
	RateTotal int

	// Failing, Flaky and Never list the enabled sources in each state, by name.
	Failing []overviewName
	Flaky   []overviewName
	Never   []overviewName
	// Quiet lists the ok sources with no new item for quietDays days or more.
	Quiet []ovQuiet
	// AllClear is set when nothing is failing, flaky, gone quiet or waiting
	// for a first run.
	AllClear bool
	// LastRun is how long ago the newest finished run ended, empty when there
	// is none or a run is still going.
	LastRun string

	Ingest ingestBarData

	// Volume is the active sources by stored items per week, most first. Active
	// means on and fetched at least once; Signal and Slowest cover the same.
	Volume []ovVolRow
	// VolumeSkipped counts the live sources left out of Volume: off, or
	// waiting for the first run.
	VolumeSkipped int
	Signal        ovSignal
	// Slowest is the three active sources with the longest average fetch.
	Slowest []ovSlow

	// Library holds the suggestions and the coverage. It is nil when they are
	// hidden: first run, or no library loaded.
	Library *overviewLibrary
	// LibFill is the URL that fills the Library sections in once the library
	// has loaded, set while it has not.
	LibFill string
}

// overviewLibrary is the two Library sections of the Overview.
type overviewLibrary struct {
	Suggestions []ovSuggestion
	Coverage    ovCoverage
}

// ovSuggestion is one suggested Library source. Done and Err are the states of
// its row after the Add button was used.
type ovSuggestion struct {
	Key  string
	ID   string
	Name string
	URL  string
	Why  string
	Done bool
	Err  string
	Icon iconRef
}

// ovCoverage is how much of the Library is added, in total and per group.
type ovCoverage struct {
	// OOB sends the section out of band, for a reply that is not the whole page.
	OOB    bool
	Added  int
	Total  int
	Groups []ovCovGroup
}

type ovCovGroup struct {
	Name  string
	Title string
	Added int
	Total int
	Pct   int
}

// ovQuiet is a source named in the attention list as gone quiet.
type ovQuiet struct {
	ID   string
	Name string
	Days int
}

// ovVolRow is one source in the volume list. Hint is set for a source gone quiet.
type ovVolRow struct {
	ID, Name, URL string
	Icon          iconRef
	Hint          string
	PerWeek       float64
	PerWeekText   string
	Spark         []sparkBar
	SparkLabel    string
}

// ovCall is a source named in a signal callout.
type ovCall struct {
	ID, Name, URL string
	Icon          iconRef
	Hint          string
}

// ovSignal is the Signal section: the two callouts and a row per active source.
type ovSignal struct {
	// Scored is whether any listed source has a scored item.
	Scored bool
	Best   *ovCall
	Noisy  []ovCall
	Rows   []ovSigRow
}

// NoisySelect is the noisy callout's sources as the ids of Manage's ?select=.
func (s ovSignal) NoisySelect() string {
	ids := make([]string, len(s.Noisy))
	for i, c := range s.Noisy {
		ids[i] = c.ID
	}
	return strings.Join(ids, ",")
}

// ovSigRow is one source in the signal list. Thin rows lack the scored items
// for the share to mean anything.
type ovSigRow struct {
	ID, Name, URL string
	Icon          iconRef
	Eng           string
	Bar           tierBar
	Thin          bool
	// Lead is the bold line of the share column and Hint the line under it.
	Lead, Hint string
}

// ovSlow is one source in the slowest list. Width is the bar's percentage.
type ovSlow struct {
	ID, Name, URL string
	Icon          iconRef
	AvgText       string
	Width         int
}

// healthSeg is one state's share of the health bar and legend. Link is the
// Manage filter for the state, set only when some source is in it.
type healthSeg struct {
	Key   string
	Label string
	Word  string // Label in lower case, as the legend reads it
	Count int
	Link  string
}

// overviewName is a source named in the attention list.
type overviewName struct {
	ID   string
	Name string
	URL  string
}

// overviewFrom builds the Overview model from the rows the Manage page lists
// and the ingest bar already read for it, shown here as the plain bar.
func overviewFrom(data sourcesData) overviewData {
	ingest := data.Attention.Bar
	ingest.Rerun, ingest.Compact, ingest.Note = false, false, false

	var ov overviewData
	var ok, off int
	for _, r := range data.Rows {
		if r.Deleted {
			continue
		}
		ov.Total++
		name := overviewName{ID: r.ID, Name: r.Name, URL: r.URL}
		switch r.State {
		case feedStateOK:
			ok++
		case feedStateFlaky:
			ov.Flaky = append(ov.Flaky, name)
		case feedStateError:
			ov.Failing = append(ov.Failing, name)
		case feedStateNever:
			ov.Never = append(ov.Never, name)
		case feedStateOff:
			off++
		}
	}
	ov.FirstRun = ov.Total == 0

	ov.Health = []healthSeg{
		{Key: "ok", Label: "OK", Word: "ok", Count: ok},
		{Key: "flaky", Label: "Flaky", Word: "flaky", Count: len(ov.Flaky),
			Link: linkIfAny(len(ov.Flaky), "/sources/manage?state=flaky")},
		{Key: "failing", Label: "Failing", Word: "failing", Count: len(ov.Failing),
			Link: linkIfAny(len(ov.Failing), "/sources/manage?state=failing")},
		{Key: "never", Label: "Waiting for first run", Word: "waiting for first run", Count: len(ov.Never)},
		{Key: "off", Label: "Off", Word: "off", Count: off,
			Link: linkIfAny(off, "/sources/manage?state=disabled")},
	}
	aria := make([]string, 0, len(ov.Health))
	for _, h := range ov.Health {
		aria = append(aria, fmt.Sprintf("%d %s", h.Count, h.Label))
	}
	ov.HealthAria = strings.Join(aria, ", ")

	okTicks, total := fetchSuccess(liveRows(data.Rows))
	ov.RateText = ratePercent(okTicks, total)
	ov.RateOK, ov.RateTotal = okTicks, total

	ov.addStats(data.Rows)

	attention := len(ov.Failing) + len(ov.Flaky) + len(ov.Quiet)
	ov.Brief = fmt.Sprintf("%s, ", plural(ov.Total, "source", "sources"))
	if attention == 0 {
		ov.Brief += "all healthy."
	} else {
		verb := "need"
		if attention == 1 {
			verb = "needs"
		}
		ov.Brief += fmt.Sprintf("%d %s attention.", attention, verb)
	}
	ov.Brief += ov.briefTail()

	ov.AllClear = len(ov.Failing) == 0 && len(ov.Flaky) == 0 && len(ov.Quiet) == 0 && len(ov.Never) == 0
	if !ingest.Running && ingest.Status != "none" {
		ov.LastRun = ingest.When
	}
	ov.Ingest = ingest
	return ov
}

// liveRows drops removed sources.
func liveRows(rows []feedRowData) []feedRowData {
	out := make([]feedRowData, 0, len(rows))
	for _, r := range rows {
		if !r.Deleted {
			out = append(out, r)
		}
	}
	return out
}

// linkIfAny returns href when n is non-zero, and no link otherwise.
func linkIfAny(n int, href string) string {
	if n > 0 {
		return href
	}
	return ""
}

// plural words a count: "1 source", "2 sources".
func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// overview is the Overview model: the live sources and, from the cached
// library, the suggestions and coverage. It never waits for the network.
func (s *Web) overview(ctx context.Context, data sourcesData) overviewData {
	ov := overviewFrom(data)
	if !ov.FirstRun {
		snap := s.library.Snapshot()
		ov.Library = s.overviewLibrary(ctx, snap, data.Rows, data.icons)
		if ov.Library == nil && libraryPending(snap) {
			ov.LibFill = libraryFillURL("overview", "", "", 1)
		}
	}
	return ov
}

// overviewLibrary builds the Library sections, or nil when the library has
// nothing to show.
func (s *Web) overviewLibrary(
	ctx context.Context, snap library.Snapshot, rows []feedRowData, icons map[string]iconRef,
) *overviewLibrary {
	if snap.Off || !snap.Loaded || len(libraryEntries(snap)) == 0 {
		return nil
	}
	states, err := s.libraryStates(ctx, snap)
	if err != nil {
		log.Error().Err(err).Msg("plan the library for the overview")
		return nil
	}
	sugg := librarySuggestions(snap, states, rows)
	for i := range sugg {
		sugg[i].Icon = icons[sugg[i].ID]
	}
	return &overviewLibrary{
		Suggestions: sugg,
		Coverage:    libraryCoverage(snap, states),
	}
}

// librarySuggestions picks the addable Library sources that share a label with
// a live source's tags, case-insensitively. A source scores 100 per matching
// label plus how many live sources carry each of them; ties keep library order.
func librarySuggestions(snap library.Snapshot, states map[string]store.PlanRow, rows []feedRowData) []ovSuggestion {
	freq := liveTagFrequency(rows)
	type cand struct {
		sug   ovSuggestion
		score int
	}
	var cands []cand
	for _, e := range libraryEntries(snap) {
		key := config.FeedURLKey(e.Source.URL)
		if state, _ := libraryState(states[key]); state != libStateAdd {
			continue
		}
		why, score := suggestionMatch(e.Source.Labels, freq)
		if score == 0 {
			continue
		}
		cands = append(cands, cand{score: score, sug: ovSuggestion{
			Key: key, ID: config.FeedID(e.Source.URL), Name: e.Source.Name, URL: e.Source.URL, Why: why,
		}})
	}
	slices.SortStableFunc(cands, func(a, b cand) int { return b.score - a.score })
	out := make([]ovSuggestion, 0, maxSuggestions)
	for _, c := range cands[:min(len(cands), maxSuggestions)] {
		out = append(out, c.sug)
	}
	return out
}

// liveTagFrequency counts, case-insensitively, how many live sources carry each tag.
func liveTagFrequency(rows []feedRowData) map[string]int {
	freq := make(map[string]int)
	for _, r := range rows {
		if r.Deleted {
			continue
		}
		for _, t := range r.Tags {
			freq[strings.ToLower(t)]++
		}
	}
	return freq
}

// suggestionMatch is the "Matches your tags" hint for a source's labels and its
// score; the score is 0 when no label matches a live tag.
func suggestionMatch(labels []string, freq map[string]int) (why string, score int) {
	var matched []string
	for _, l := range labels {
		if n := freq[strings.ToLower(l)]; n > 0 {
			matched = append(matched, l)
			score += 100 + n
		}
	}
	if len(matched) == 0 {
		return "", 0
	}
	return "Matches your tags: " + strings.Join(matched, ", "), score
}

// suggestionWhy recomputes the hint of the first picked entry from the sources
// stored right now, before an add makes the entry match itself.
func (s *Web) suggestionWhy(ctx context.Context, snap library.Snapshot, pick map[string]bool) string {
	doc, err := s.db.FeedsDoc(ctx)
	if err != nil {
		return ""
	}
	rows := make([]feedRowData, len(doc.Feeds))
	for i, f := range doc.Feeds {
		rows[i].Tags = f.Tags
	}
	for _, e := range libraryEntries(snap) {
		if pick[config.FeedURLKey(e.Source.URL)] {
			why, _ := suggestionMatch(e.Source.Labels, liveTagFrequency(rows))
			return why
		}
	}
	return ""
}

// libraryCoverage counts the added sources of each group.
func libraryCoverage(snap library.Snapshot, states map[string]store.PlanRow) ovCoverage {
	var cov ovCoverage
	for _, f := range snap.Library.Files {
		for _, g := range f.Groups {
			cg := ovCovGroup{Name: g.Name, Title: g.Title, Total: len(g.Sources)}
			for _, src := range g.Sources {
				if state, _ := libraryState(states[config.FeedURLKey(src.URL)]); state == libStateSubscribed {
					cg.Added++
				}
			}
			if cg.Total > 0 {
				cg.Pct = cg.Added * 100 / cg.Total
			}
			cov.Added += cg.Added
			cov.Total += cg.Total
			cov.Groups = append(cov.Groups, cg)
		}
	}
	return cov
}

// SkippedNote words how many live sources the volume list leaves out.
func (ov overviewData) SkippedNote() string {
	return plural(ov.VolumeSkipped, "source", "sources") + " not shown: off or waiting for the first run."
}

// briefTail is the closing line of the brief: the busiest source and the one
// with the best signal, whichever exist.
func (ov overviewData) briefTail() string {
	busy := len(ov.Volume) > 0 && ov.Volume[0].PerWeek > 0
	best := ov.Signal.Best != nil
	switch {
	case busy && best && ov.Volume[0].ID == ov.Signal.Best.ID:
		return " " + ov.Volume[0].Name + " brings the most items and has the best signal."
	case busy && best:
		return " " + ov.Volume[0].Name + " brings the most items and " + ov.Signal.Best.Name + " has the best signal."
	case busy:
		return " " + ov.Volume[0].Name + " brings the most items."
	case best:
		return " " + ov.Signal.Best.Name + " has the best signal."
	}
	return ""
}

// addStats fills the quiet list and the Volume, Signal and Slowest sections
// from the rows' stats.
func (ov *overviewData) addStats(rows []feedRowData) {
	var active []feedRowData
	for _, r := range rows {
		if r.Deleted {
			continue
		}
		if r.Stats.Quiet {
			ov.Quiet = append(ov.Quiet, ovQuiet{ID: r.ID, Name: r.Name, Days: r.Stats.QuietDays})
		}
		if r.State == feedStateOff || r.State == feedStateNever {
			ov.VolumeSkipped++
			continue
		}
		active = append(active, r)
	}
	ov.Volume = volumeRows(active)
	ov.Signal = signalOf(active)
	ov.Slowest = slowestOf(active)
}

// volumeRows lists the sources by stored items per week, most first.
func volumeRows(active []feedRowData) []ovVolRow {
	sorted := slices.Clone(active)
	slices.SortStableFunc(sorted, func(a, b feedRowData) int {
		return cmp.Compare(b.Stats.PerWeek, a.Stats.PerWeek)
	})
	out := make([]ovVolRow, len(sorted))
	for i, r := range sorted {
		out[i] = ovVolRow{
			ID:          r.ID,
			Name:        r.Name,
			URL:         r.URL,
			Icon:        r.Icon,
			PerWeek:     r.Stats.PerWeek,
			PerWeekText: r.Stats.PerWeekText,
			Spark:       r.Stats.Spark,
			SparkLabel:  r.Stats.SparkLabel,
		}
		if r.Stats.Quiet {
			out[i].Hint = fmt.Sprintf("No new items in %d days", r.Stats.QuietDays)
		}
	}
	return out
}

// signalOf builds the Signal section: sources with enough scored items first,
// by share, then the rest by how many they have. The callouts pick from the
// sources with at least noisyMinScored scored items.
func signalOf(active []feedRowData) ovSignal {
	sorted := slices.Clone(active)
	slices.SortStableFunc(sorted, func(a, b feedRowData) int {
		if a.Stats.Sig.Enough != b.Stats.Sig.Enough {
			if a.Stats.Sig.Enough {
				return -1
			}
			return 1
		}
		if a.Stats.Sig.Enough {
			return cmp.Compare(b.Stats.Sig.Share, a.Stats.Sig.Share)
		}
		return cmp.Compare(b.Stats.Sig.Scored, a.Stats.Sig.Scored)
	})
	var sig ovSignal
	sig.Rows = make([]ovSigRow, len(sorted))
	for i, r := range sorted {
		sig.Rows[i] = sigRow(r)
		sig.Scored = sig.Scored || r.Stats.Sig.Scored > 0
	}

	var solid []feedRowData
	for _, r := range active {
		if r.Stats.Sig.Scored >= noisyMinScored {
			solid = append(solid, r)
		}
	}
	byShare := slices.Clone(solid)
	slices.SortStableFunc(byShare, func(a, b feedRowData) int {
		return cmp.Compare(b.Stats.Sig.Share, a.Stats.Sig.Share)
	})
	var bestID string
	if len(byShare) > 0 && byShare[0].Stats.Sig.Share > 0 {
		c := callOf(byShare[0])
		sig.Best, bestID = &c, byShare[0].ID
	}
	var noisy []feedRowData
	for _, r := range solid {
		if r.Stats.Sig.Share <= noisyShare && r.ID != bestID {
			noisy = append(noisy, r)
		}
	}
	slices.SortStableFunc(noisy, func(a, b feedRowData) int {
		return cmp.Compare(a.Stats.Sig.Share, b.Stats.Sig.Share)
	})
	for _, r := range noisy[:min(len(noisy), maxNoisy)] {
		sig.Noisy = append(sig.Noisy, callOf(r))
	}
	return sig
}

// callOf is a callout entry: the source and its share.
func callOf(r feedRowData) ovCall {
	sg := r.Stats.Sig
	return ovCall{
		ID: r.ID, Name: r.Name, URL: r.URL, Icon: r.Icon,
		Hint: fmt.Sprintf("%d of %d scored are %s (%d%%)", sg.High, sg.Scored, highPlus(), sg.Pct),
	}
}

// sigRow is a source's row in the signal list.
func sigRow(r feedRowData) ovSigRow {
	sg, e := r.Stats.Sig, r.Stats.Eng
	row := ovSigRow{
		ID:   r.ID,
		Name: r.Name,
		URL:  r.URL,
		Icon: r.Icon,
		Bar:  sg.Bar,
		Thin: !sg.Enough,
		Eng: fmt.Sprintf(
			"read %d · hidden %d · bookmarked %d · thumbs %d up, %d down",
			e.Read,
			e.Hidden,
			e.Bookmarked,
			e.Up,
			e.Down,
		),
	}
	if !sg.Enough {
		row.Lead = "Not enough data yet"
		row.Hint = fmt.Sprintf("%d scored, %d needed", sg.Scored, minScored)
		return row
	}
	row.Lead = fmt.Sprintf("%d%%", sg.Pct)
	row.Hint = fmt.Sprintf("%d of %d scored are %s", sg.High, sg.Scored, highPlus())
	if sg.Unscored > 0 {
		row.Hint += fmt.Sprintf(", %d unscored", sg.Unscored)
	}
	return row
}

// slowestOf lists the maxSlowest active sources with the longest average
// fetch. A bar is its share of the slowest, at least minBarPct wide.
func slowestOf(active []feedRowData) []ovSlow {
	var timed []feedRowData
	for _, r := range active {
		if r.Stats.AvgMs > 0 {
			timed = append(timed, r)
		}
	}
	slices.SortStableFunc(timed, func(a, b feedRowData) int { return b.Stats.AvgMs - a.Stats.AvgMs })
	if len(timed) == 0 {
		return nil
	}
	top := timed[0].Stats.AvgMs
	out := make([]ovSlow, 0, maxSlowest)
	for _, r := range timed[:min(len(timed), maxSlowest)] {
		width := int(math.Round(float64(r.Stats.AvgMs) / float64(top) * 100))
		out = append(out, ovSlow{
			ID: r.ID, Name: r.Name, URL: r.URL, Icon: r.Icon,
			AvgText: r.Stats.AvgText, Width: max(minBarPct, width),
		})
	}
	return out
}
