// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/DanielBlei/rabbithole/internal/httplog"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// ingestionMaxPage bounds ?page so an absurd value cannot overflow the offset.
const ingestionMaxPage = 100000

// ingestionPage is the model of the Ingestion section of Sources: the ingest
// bar and one page of recent runs.
type ingestionPage struct {
	Bar  ingestBarData
	Runs []ingestionRun
	// Page is the one-based page shown; Prev and Next are the neighbouring
	// pages the pager links to.
	Page, PrevPage, NextPage int
	HasPrev, HasNext         bool
	// OOB renders the bar and runs unit with hx-swap-oob, for the live refresh.
	OOB bool
	// Badge is the Manage link's failing count, sent with the live refresh.
	Badge *sourceCounts
}

// ingestionRun is one ingest_history row shaped for the runs table.
type ingestionRun struct {
	ID      int64
	Running bool
	// Status is ok | error | cancelled | running, the dot class and the row's
	// data-status; StatusLabel is its word.
	Status, StatusLabel string
	// Started is "5m ago"; StartedISO and StartedTitle feed the <time> element.
	Started, StartedISO, StartedTitle string
	NewCell, Duration, TriggerLabel   string
	// Err is the run's error text; empty for a cancelled run, whose text is
	// only the word again.
	Err    string
	Open   bool
	Detail *ingestionDetail
}

// ingestionDetail is the expanded row of one run: its counts, the per-feed
// results and the captured log.
type ingestionDetail struct {
	Counts   store.IngestCounts
	Rescore  bool
	Feeds    []ingestionFeed
	HasFeeds bool
	Logs     []ingestLogLine
}

// ingestionFeed is one feed's result within a run.
type ingestionFeed struct {
	Name, Result, Took string
	Items              int
	OK                 bool
	// Why is the plain-language reason of a failed fetch; Raw is the fetch
	// error as recorded, for the tooltip.
	Why, Raw string
}

// handleIngestion renders the Ingestion section of Sources.
func (s *Web) handleIngestion(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data, err := s.sourcesPageData(ctx, "", "ingestion")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data.Title = "Ingestion"
	data.Ingestion, err = s.ingestionView(ctx, ingestionPageFromQuery(r))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.writeSourcesPage(w, data)
}

// handleIngestionLive answers the refresh the page makes when a run starts or
// ends: the bar and the runs, back on the page they were on, and the Manage
// badge, all out of band.
func (s *Web) handleIngestionLive(w http.ResponseWriter, r *http.Request) {
	httplog.Quiet(r)
	page, err := s.ingestionView(r.Context(), ingestionPageFromQuery(r))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	page.OOB = true
	page.Badge = s.navBadge(r.Context())
	var buf bytes.Buffer
	if err := sourcesTmpl.ExecuteTemplate(&buf, "ingestionLiveReply", page); err != nil {
		log.Error().Err(err).Msg("render ingestion live")
		http.Error(w, "could not render ingestion", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

// handleIngestionRun swaps one run's <tbody>: expanded with its detail, or
// the summary row alone with collapse=1. A run still in progress has no
// detail yet, so it always answers with the summary.
func (s *Web) handleIngestionRun(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad run id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	run, lines, err := s.db.GetIngestRunWithLog(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrIngestRunNotFound) {
			http.Error(w, "run not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	row := toIngestionRun(*run, s.clock())
	if r.URL.Query().Get("collapse") != "1" && !row.Running {
		results, err := s.db.RunFeedResults(ctx, id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		row.Open = true
		row.Detail = toIngestionDetail(*run, results, lines)
	}

	var buf bytes.Buffer
	if err := sourcesTmpl.ExecuteTemplate(&buf, "ingestionRow", row); err != nil {
		log.Error().Err(err).Msg("render ingestion run")
		http.Error(w, "could not render run", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

// ingestionView builds the model for one page of runs (one-based). A page past
// the last one shows the last.
func (s *Web) ingestionView(ctx context.Context, page int) (ingestionPage, error) {
	bar, err := s.ingestBarView(ctx, false, false)
	if err != nil {
		return ingestionPage{}, err
	}
	runs, more, err := s.db.ListIngestRuns(ctx, ingestHistoryLimit, (page-1)*ingestHistoryLimit)
	if err != nil {
		return ingestionPage{}, err
	}
	if len(runs) == 0 && page > 1 {
		total, err := s.db.CountIngestRuns(ctx)
		if err != nil {
			return ingestionPage{}, err
		}
		page = max(1, (total+ingestHistoryLimit-1)/ingestHistoryLimit)
		if runs, more, err = s.db.ListIngestRuns(ctx, ingestHistoryLimit, (page-1)*ingestHistoryLimit); err != nil {
			return ingestionPage{}, err
		}
	}
	out := ingestionPage{
		Bar: bar, Page: page,
		PrevPage: page - 1, NextPage: page + 1,
		HasPrev: page > 1, HasNext: more,
	}
	now := s.clock()
	for _, run := range runs {
		out.Runs = append(out.Runs, toIngestionRun(run, now))
	}
	return out, nil
}

// ingestionPageFromQuery reads the one-based page from ?page=N, clamping
// anything missing or invalid to 1.
func ingestionPageFromQuery(r *http.Request) int {
	n, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil || n < 1 {
		return 1
	}
	return min(n, ingestionMaxPage)
}

// toIngestionRun shapes one history row for the runs table.
func toIngestionRun(r store.IngestRun, now time.Time) ingestionRun {
	run := ingestionRun{
		ID:           r.ID,
		Running:      r.Status == store.IngestStatusRunning,
		Started:      agoPhrase(r.StartedAt, now),
		StartedISO:   r.StartedAt.UTC().Format(time.RFC3339),
		StartedTitle: r.StartedAt.Local().Format("2 Jan 2006, 15:04:05"),
		TriggerLabel: ingestionTrigger(r.TriggeredBy),
		Duration:     "-",
	}
	switch r.Status {
	case store.IngestStatusRunning:
		run.Status, run.StatusLabel = "running", "Running"
	case store.IngestStatusOK:
		run.Status, run.StatusLabel = "ok", "Ok"
	case store.IngestStatusError:
		run.Status, run.StatusLabel = "error", "Failed"
	default:
		// Anything else reads as cancelled, as the bar and the chip do.
		run.Status, run.StatusLabel = "cancelled", "Cancelled"
	}
	if run.Status != "cancelled" {
		run.Err = r.Error
	}
	switch {
	case run.Running:
		run.NewCell, run.Duration = "-", "in progress"
	case r.TriggeredBy == store.IngestTriggerProfileRescore:
		run.NewCell = fmt.Sprintf("%d of %d rescored", r.Counts.Scored, r.Counts.Fetched)
	default:
		run.NewCell = strconv.Itoa(r.Counts.NewItems)
	}
	if r.FinishedAt != nil {
		run.Duration = fmtRunDur(r.FinishedAt.Sub(r.StartedAt))
	}
	return run
}

// ingestionTrigger words how a run was started.
func ingestionTrigger(t string) string {
	switch t {
	case store.IngestTriggerManual:
		return "Manual"
	case store.IngestTriggerProfileRescore:
		return "Profile rescore"
	case store.IngestTriggerCron:
		return "Scheduled"
	default:
		return t
	}
}

// toIngestionDetail shapes the expanded part of a run.
func toIngestionDetail(r store.IngestRun, results []store.RunFeedResult, lines []string) *ingestionDetail {
	d := &ingestionDetail{
		Counts:   r.Counts,
		Rescore:  r.TriggeredBy == store.IngestTriggerProfileRescore,
		HasFeeds: len(results) > 0,
	}
	for _, f := range results {
		feed := ingestionFeed{Name: f.Name, Items: f.Items, OK: f.OK(), Took: fmtFeedTook(f.Elapsed)}
		if f.OK() {
			feed.Result = "Ok"
		} else {
			feed.Result, feed.Raw, feed.Why = "Failed", f.Error, explainFeedError(f.Error)
		}
		d.Feeds = append(d.Feeds, feed)
	}
	for _, raw := range lines {
		d.Logs = append(d.Logs, parseIngestLogLine(raw))
	}
	return d
}

// fmtFeedTook words one feed's fetch time: milliseconds under a second.
func fmtFeedTook(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmtRunDur(d)
}
