// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"fmt"
	"strconv"
	"time"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// sideData is the read-only panel beside an open editor: the source's last 30
// days and its recent fetch attempts. Nil while a source is being added.
type sideData struct {
	// Status is the explanation of the source's health, with the last fetch.
	Status string
	// Never is a source that has not been fetched: signal and activity read as
	// empty rather than as zeros.
	Never bool
	Stats rowStats
	// ItemsText is the weekly volume and the newest item's age.
	ItemsText string
	// SignalText is the signal share and its denominator, or why there is none.
	SignalText string
	// ActivityText is what was done with the window's items.
	ActivityText string
	// FetchText is the average and latest fetch time.
	FetchText string
	// Effective is the lookback and cap the source runs with, and where each
	// comes from.
	Effective string

	History []histRow
	// HistoryHint is "last N attempts", empty without attempts.
	HistoryHint string
}

// histRow is one fetch attempt in the panel's history list.
type histRow struct {
	Ago string
	// At is the attempt's full timestamp, the ago's tooltip.
	At string
	OK bool
	// Detail is the item count, or the clipped error; Title is the full error.
	Detail  string
	Title   string
	Elapsed string
}

// side builds the editor's panel from the row (with its stats applied) and the
// feed's health.
func (r *feedRowData) side(h store.FeedHealth, now time.Time) *sideData {
	s := &sideData{Stats: r.Stats, Never: r.State == feedStateNever}

	switch r.State {
	case feedStateOK:
		s.Status = "Fetching fine."
	case feedStateNever:
		s.Status = "Waiting for first run"
	case feedStateOff:
		s.Status = "Disabled. Ingestion skips it."
	default:
		s.Status = r.Explain
	}
	if !h.LastFetch.IsZero() {
		s.Status += " Last fetch " + agoPhrase(h.LastFetch, now) + "."
	}

	s.ItemsText = r.Stats.PerWeekText + " per week"
	if r.Stats.LastPhrase != "" {
		s.ItemsText += ", last item " + r.Stats.LastPhrase
	}

	sg := r.Stats.Sig
	switch {
	case s.Never:
		s.SignalText = "No items yet."
		s.ActivityText = "None yet."
	default:
		s.SignalText = "Not enough data yet. " + signalDetail(sg)
		if sg.Enough {
			s.SignalText = strconv.Itoa(sg.Pct) + "%: " + signalDetail(sg)
		}
		e := r.Stats.Eng
		s.ActivityText = fmt.Sprintf("%d read, %d hidden, %d bookmarked, %d thumbs up, %d down",
			e.Read, e.Hidden, e.Bookmarked, e.Up, e.Down)
	}

	switch {
	case r.Stats.AvgText != "" && r.Stats.LastText != "":
		s.FetchText = r.Stats.AvgText + " average, " + r.Stats.LastText + " last"
	case r.Stats.AvgText != "":
		s.FetchText = r.Stats.AvgText + " average"
	case h.LastFetch.IsZero():
		s.FetchText = "Not fetched yet."
	default:
		s.FetchText = "No successful fetch to time."
	}

	s.Effective = r.Since + " back (" + originWord(r.SinceOn) + "), "
	if r.MaxItems <= 0 {
		s.Effective += "no item cap"
	} else {
		s.Effective += "up to " + r.Cap + " items"
	}
	s.Effective += " (" + originWord(r.CapOn) + ")"

	s.History = make([]histRow, len(h.Recent))
	for i, a := range h.Recent {
		hr := histRow{
			Ago: agoPhrase(a.At, now), At: a.At.Local().Format("2 Jan 2006, 15:04:05"), OK: a.OK(),
			Elapsed: fmtMs(int(a.Elapsed.Round(time.Millisecond) / time.Millisecond)),
		}
		if a.OK() {
			hr.Detail = itemsPhrase(a.Items)
		} else {
			hr.Title = a.Error
			if hr.Title == "" {
				hr.Title = "failed"
			}
			hr.Detail = clipText(hr.Title)
		}
		s.History[i] = hr
	}
	if n := len(h.Recent); n == 1 {
		s.HistoryHint = "last attempt"
	} else if n > 1 {
		s.HistoryHint = "last " + strconv.Itoa(n) + " attempts"
	}
	return s
}

// signalDetail is the denominator behind a signal share.
func signalDetail(sg sigStats) string {
	if sg.Enough {
		return fmt.Sprintf("%d of %d scored are %s, %d unscored", sg.High, sg.Scored, highPlus(), sg.Unscored)
	}
	return fmt.Sprintf("Only %d scored so far (needs %d), %d unscored", sg.Scored, minScored, sg.Unscored)
}

// originWord words where a resolved value came from; a blank origin is the
// source itself.
func originWord(from string) string {
	switch config.Origin(from) {
	case config.OriginDefaults:
		return originDefaults
	case config.OriginBuiltin:
		return originBuiltin
	default:
		return originOwn
	}
}
