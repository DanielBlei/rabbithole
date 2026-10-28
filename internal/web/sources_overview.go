// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"fmt"
	"strings"
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
	// AllClear is set when nothing is failing, flaky or waiting for a first run.
	AllClear bool
	// LastRun is how long ago the newest finished run ended, empty when there
	// is none or a run is still going.
	LastRun string

	Ingest ingestBarData
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
