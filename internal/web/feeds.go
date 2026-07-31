// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// feedStripLen is how many past fetch attempts the per-feed history strip
// shows. Enough to see a pattern ("failing since three runs ago") without
// turning the row into a chart.
const feedStripLen = 12

// Feed row display states.
const (
	feedStateOK    = "ok"
	feedStateError = "error"
	feedStateNever = "never" // configured but never fetched
	feedStateOff   = "off"   // parked
)

// feedsData is the model for the read-only feeds viewer modal: the resolved
// feed set, each entry's effective settings and where they came from, joined
// with its fetch history.
type feedsData struct {
	Path     string // the file the feeds were read from
	Total    int
	Enabled  int
	Failing  int    // enabled feeds whose latest fetch errored
	Defaults string // one-line summary of the file's defaults block
	Error    string
	Rows     []feedRowData
}

// feedRowData is one feed's line in the viewer. The *On strings name where an
// inherited value came from (rendered as the asterisk's tooltip); a value the
// feed sets itself has an empty *On.
type feedRowData struct {
	Name string
	URL  string
	Host string // URL's host — the compact label under the feed name

	Enabled bool
	Since   string
	SinceOn string
	Cap     string // max_items, or "—" when uncapped
	CapOn   string
	Tags    []string

	// Health, aggregated from the feed's fetch history.
	State      string
	Detail     string // error message, or the item-count phrase
	LastFetch  string // relative time of the latest attempt, "" when never
	FailStreak int    // consecutive failures; >1 is worth showing
	Since1st   string // when the current failure run began
	Strip      []feedTick
}

// feedTick is one attempt in the history strip, oldest-first for display.
type feedTick struct {
	OK    bool
	Title string // hover text: outcome + when
}

// handleFeeds renders the feeds-viewer modal fragment: what the server
// resolved from the feeds file, not the raw file text. Effective values matter
// more than the source here — with dozens of feeds and a defaults cascade,
// "what will actually be fetched, and is it still working" is the question.
func (s *Web) handleFeeds(w http.ResponseWriter, r *http.Request) {
	feedSet := s.cfg.Feeds
	data := feedsData{
		Path:     feedSet.Path,
		Total:    feedSet.Len(),
		Defaults: defaultsSummary(s.cfg),
	}

	// History is a join, not a requirement: a store read failure still leaves a
	// useful config view, so it degrades to a note rather than an error page.
	health, err := s.db.FeedHealthByID(r.Context(), feedStripLen)
	if err != nil {
		log.Error().Err(err).Msg("read feed health")
		data.Error = "fetch history unavailable: " + err.Error()
	}

	now := time.Now()
	data.Rows = make([]feedRowData, 0, feedSet.Len())
	for _, f := range feedSet.All {
		row := toFeedRow(f, health[f.ID], now)
		if f.Enabled {
			data.Enabled++
		}
		if row.State == feedStateError {
			data.Failing++
		}
		data.Rows = append(data.Rows, row)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := feedTmpl.ExecuteTemplate(w, "feedsModal", data); err != nil {
		// Status is likely already written; log rather than double-write.
		log.Error().Err(err).Msg("render feeds modal")
	}
}

// toFeedRow shapes one resolved feed plus its health into a display row.
func toFeedRow(f config.ResolvedFeed, h store.FeedHealth, now time.Time) feedRowData {
	row := feedRowData{
		Name:    f.Name,
		URL:     f.URL,
		Host:    hostOf(f.URL),
		Enabled: f.Enabled,
		Since:   shortDur(f.Since),
		Cap:     capLabel(f.MaxItems),
		Tags:    f.Tags,
	}
	if f.SinceFrom.Inherited() {
		row.SinceOn = string(f.SinceFrom)
	}
	if f.MaxItemsFrom.Inherited() {
		row.CapOn = string(f.MaxItemsFrom)
	}

	// A disabled feed reports as parked regardless of what its last fetch did:
	// stale history from before it was switched off would read as a live
	// problem. Its strip is still worth showing — that's the point of keeping
	// history — so it's built either way.
	row.Strip = toFeedStrip(h.Recent, now)
	if !f.Enabled {
		row.State, row.Detail = feedStateOff, "disabled"
		return row
	}

	switch {
	case h.LastFetch.IsZero():
		row.State, row.Detail = feedStateNever, "not fetched yet"
	case h.OK():
		row.State = feedStateOK
		row.Detail = itemsPhrase(h.Items)
		row.LastFetch = agoPhrase(h.LastFetch, now)
	default:
		row.State = feedStateError
		row.Detail = h.Error
		row.LastFetch = agoPhrase(h.LastFetch, now)
		row.FailStreak = h.FailStreak
		if since, failing := h.FailingSince(); failing {
			row.Since1st = agoPhrase(since, now)
		}
	}
	return row
}

// toFeedStrip renders the attempt history oldest-first, so the strip reads
// left-to-right as time moving forward and the newest tick sits nearest the
// status text it explains.
func toFeedStrip(attempts []store.FeedAttempt, now time.Time) []feedTick {
	if len(attempts) == 0 {
		return nil
	}
	out := make([]feedTick, 0, len(attempts))
	for i := len(attempts) - 1; i >= 0; i-- {
		a := attempts[i]
		title := itemsPhrase(a.Items)
		if !a.OK() {
			title = a.Error
			if title == "" {
				title = "failed"
			}
		}
		out = append(out, feedTick{OK: a.OK(), Title: title + " · " + agoPhrase(a.At, now)})
	}
	return out
}

// itemsPhrase renders a fetch's item count with correct pluralization.
func itemsPhrase(n int) string {
	if n == 1 {
		return "1 item"
	}
	return strconv.Itoa(n) + " items"
}

// defaultsSummary renders the feeds file's defaults block as one line for the
// modal's legend, naming the global fallback for anything the block leaves
// unset — so the cascade is legible without opening the file.
func defaultsSummary(cfg *config.Config) string {
	d := cfg.Feeds.Defaults
	var parts []string
	if d.Since != nil {
		parts = append(parts, "since "+shortDur(d.Since.Std()))
	} else {
		parts = append(parts, "since "+shortDur(cfg.Ingest.Since.Std())+" (from ingest.since)")
	}
	if d.MaxItems != nil {
		parts = append(parts, "max_items "+capLabel(*d.MaxItems))
	} else {
		parts = append(parts, "uncapped")
	}
	if d.Enabled != nil && !*d.Enabled {
		parts = append(parts, "disabled by default")
	}
	return strings.Join(parts, " · ")
}

// capLabel renders a max_items value; zero means uncapped.
func capLabel(n int) string {
	if n <= 0 {
		return "—"
	}
	return strconv.Itoa(n)
}

// shortDur renders a lookback window the way it's written in config — "7d",
// "36h", "90m" — rather than Go's "168h0m0s".
func shortDur(d time.Duration) string {
	switch {
	case d <= 0:
		return "—"
	case d%(24*time.Hour) == 0:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d"
	case d%time.Hour == 0:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	case d%time.Minute == 0:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	default:
		return d.String()
	}
}

// hostOf pulls the host out of a feed URL for the compact cell label, falling
// back to the raw string when it isn't a parseable absolute URL.
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	return u.Host
}
