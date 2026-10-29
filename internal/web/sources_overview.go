// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/rs/zerolog/log"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/library"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// maxSuggestions is how many Library sources the Overview suggests.
const maxSuggestions = 4

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
	// AllClear is set when nothing is failing, flaky or waiting for a first run.
	AllClear bool
	// LastRun is how long ago the newest finished run ended, empty when there
	// is none or a run is still going.
	LastRun string

	Ingest ingestBarData

	// Library holds the suggestions and the coverage. It is nil when they are
	// hidden: first run, or no library loaded.
	Library *overviewLibrary
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

	attention := len(ov.Failing) + len(ov.Flaky)
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

	ov.AllClear = len(ov.Failing) == 0 && len(ov.Flaky) == 0 && len(ov.Never) == 0
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
		ov.Library = s.overviewLibrary(ctx, s.library.Snapshot(), data.Rows)
	}
	return ov
}

// overviewLibrary builds the Library sections, or nil when the library has
// nothing to show.
func (s *Web) overviewLibrary(ctx context.Context, snap library.Snapshot, rows []feedRowData) *overviewLibrary {
	if snap.Off || !snap.Loaded || len(libraryEntries(snap)) == 0 {
		return nil
	}
	states, err := s.libraryStates(ctx, snap)
	if err != nil {
		log.Error().Err(err).Msg("plan the library for the overview")
		return nil
	}
	return &overviewLibrary{
		Suggestions: librarySuggestions(snap, states, rows),
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
