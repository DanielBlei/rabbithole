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
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/DanielBlei/rabbithole/internal/httplog"
	"github.com/DanielBlei/rabbithole/internal/ingest"
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
	// ElapsedMS is a live run's age in milliseconds; Duration words it until
	// the page's clock takes over.
	ElapsedMS int64
	// Toggle gives the row its expand button. A running row with no manager run
	// behind it has nothing to expand.
	Toggle bool
	// Err is the run's error text; empty for a cancelled run, whose text is
	// only the word again.
	Err    string
	Open   bool
	Detail *ingestionDetail
}

// ingestionDetail is the expanded row of one run: its counts, the per-feed
// results and the captured log.
type ingestionDetail struct {
	ID       int64
	Counts   store.IngestCounts
	Rescore  bool
	Feeds    []ingestionFeed
	HasFeeds bool
	// Logs are the lines shown: info and above, plus debug and trace once the
	// reader asked for them (Debug). HasLog tells a run with a captured log from
	// one with none, whatever the filter left.
	Logs   []ingestLogLine
	HasLog bool
	// NoInfo is set when no info line is shown, so the log says why it is empty.
	NoInfo bool
	Debug  bool
	// Live marks the run in flight: its counts are not recorded yet and its log
	// is the manager's buffer. Seq is the number of the last line in Logs, the
	// cursor the log's poll continues from.
	Live bool
	Seq  int64
}

// ingestionLogAppend is the answer to a live log poll: the lines after the
// poll's cursor and the cursor for the next poll.
type ingestionLogAppend struct {
	ID    int64
	Seq   int64
	Debug bool
	Logs  []ingestLogLine
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
	bar := s.ingestBarKnown(data.Counts.Enabled, data.last, false, false)
	data.Ingestion, err = s.ingestionPageFor(ctx, bar, ingestionPageFromQuery(r), nil, false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.writeSourcesPage(w, data)
}

// handleIngestionLive answers the refresh the page makes when a run starts or
// ends: the bar and the runs, back on the page they were on, and the Manage
// badge, all out of band. The run in flight is open, and so is each finished row
// ?open names.
func (s *Web) handleIngestionLive(w http.ResponseWriter, r *http.Request) {
	httplog.Quiet(r)
	q := r.URL.Query()
	page, err := s.ingestionView(
		r.Context(), ingestionPageFromQuery(r), openRunsFromQuery(r), q.Get("debug") == "1",
	)
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
// the summary row alone with collapse=1. The run in flight expands to its live
// log, which then polls for newer lines; a finished run expands to its stored
// detail. Its log holds info lines and above unless debug=1.
func (s *Web) handleIngestionRun(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad run id", http.StatusBadRequest)
		return
	}
	q := r.URL.Query()
	s.writeRunRow(w, r, id, q.Get("collapse") == "1", q.Get("debug") == "1", false)
}

// writeRunRow renders one run's <tbody>. retarget sends it to the run's row
// whatever element asked, for a log poll that finds its run over; a run that
// is gone then answers 200 with no body, which swaps the poller away.
func (s *Web) writeRunRow(w http.ResponseWriter, r *http.Request, id int64, collapse, debug, retarget bool) {
	ctx := r.Context()
	// The manager is read before the store: a run ends in the store first and
	// leaves the manager second, so this order never sees it gone from both.
	st := s.ing.Status()
	run, lines, err := s.db.GetIngestRunWithLog(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrIngestRunNotFound) {
			if retarget {
				w.WriteHeader(http.StatusOK)
				return
			}
			http.Error(w, "run not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	live := run.Status == store.IngestStatusRunning && st.Running && st.RunID == id
	row := toIngestionRun(*run, s.clock())
	if live {
		liveRun(&row, st)
	}
	if !collapse && (live || !row.Running) {
		if err := s.openRun(ctx, &row, *run, lines, st, live, debug); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	var buf bytes.Buffer
	if err := sourcesTmpl.ExecuteTemplate(&buf, "ingestionRow", row); err != nil {
		log.Error().Err(err).Msg("render ingestion run")
		http.Error(w, "could not render run", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if retarget {
		w.Header().Set("HX-Retarget", fmt.Sprintf("#run-%d", id))
		w.Header().Set("HX-Reswap", "outerHTML")
	}
	_, _ = w.Write(buf.Bytes())
}

// handleIngestionRunLog serves one run's log in parts, for the log element:
//   - after=N polls the live run: the lines numbered above N (a marker first if
//     some fell off the ring), then the cursor for the next poll. Nothing new
//     answers 204. Once the run is over it answers with the run's finished row
//     in place of the log.
//   - full=1 answers the whole log element, for a reader who picks debug after
//     the log opened with info lines only.
//
// debug=1 keeps debug and trace lines in either answer.
func (s *Web) handleIngestionRunLog(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad run id", http.StatusBadRequest)
		return
	}
	httplog.Quiet(r)
	q := r.URL.Query()
	debug := q.Get("debug") == "1"

	if q.Get("full") == "1" {
		s.writeRunLog(w, r, id, debug)
		return
	}
	after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
	chunk, live := s.ing.LogSince(id, after)
	if !live {
		s.writeRunRow(w, r, id, false, debug, true)
		return
	}
	if chunk.Last == after && chunk.Dropped == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.writeIngestionTmpl(w, "ingestionLogAppend", ingestionLogAppend{
		ID: id, Seq: chunk.Last, Debug: debug, Logs: displayLogLines(chunk.Lines, chunk.Dropped, debug),
	})
}

// writeRunLog renders one run's whole log element.
func (s *Web) writeRunLog(w http.ResponseWriter, r *http.Request, id int64, debug bool) {
	st := s.ing.Status()
	run, stored, err := s.db.GetIngestRunWithLog(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrIngestRunNotFound) {
			http.Error(w, "run not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	live := run.Status == store.IngestStatusRunning && st.Running && st.RunID == id
	d := &ingestionDetail{ID: id, Live: live, Debug: debug}
	d.setLog(runLog(*run, stored, st, live), debug)
	s.writeIngestionTmpl(w, "ingestionLog", d)
}

// writeIngestionTmpl executes one sources template to the response.
func (s *Web) writeIngestionTmpl(w http.ResponseWriter, name string, data any) {
	var buf bytes.Buffer
	if err := sourcesTmpl.ExecuteTemplate(&buf, name, data); err != nil {
		log.Error().Err(err).Str("template", name).Msg("render ingestion")
		http.Error(w, "could not render ingestion", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

// ingestionView builds the model for one page of runs (one-based). A page past
// the last one shows the last. The run in flight is always open; a finished
// run is open when its id is in open. Open logs hold debug lines when debug is
// set.
func (s *Web) ingestionView(
	ctx context.Context, page int, open map[int64]bool, debug bool,
) (ingestionPage, error) {
	bar, err := s.ingestBarView(ctx, false, false)
	if err != nil {
		return ingestionPage{}, err
	}
	return s.ingestionPageFor(ctx, bar, page, open, debug)
}

// ingestionPageFor is ingestionView for a caller that already built the bar.
func (s *Web) ingestionPageFor(
	ctx context.Context, bar ingestBarData, page int, open map[int64]bool, debug bool,
) (ingestionPage, error) {
	st := s.ing.Status()
	var err error
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
		row := toIngestionRun(run, now)
		live := row.Running && st.Running && st.RunID == run.ID
		if live {
			liveRun(&row, st)
		}
		if live || open[run.ID] {
			_, lines, err := s.db.GetIngestRunWithLog(ctx, run.ID)
			if err != nil {
				return ingestionPage{}, err
			}
			if err := s.openRun(ctx, &row, run, lines, st, live, debug); err != nil {
				return ingestionPage{}, err
			}
		}
		out.Runs = append(out.Runs, row)
	}
	return out, nil
}

// openRun expands a row with its detail: the manager's log for the live run,
// the stored one (lines) otherwise. A run whose row is final but whose log is
// not stored yet still reads from the manager.
func (s *Web) openRun(
	ctx context.Context, row *ingestionRun, run store.IngestRun, lines []string, st ingest.Status,
	live, debug bool,
) error {
	results, err := s.db.RunFeedResults(ctx, run.ID)
	if err != nil {
		return err
	}
	row.Open = true
	row.Detail = toIngestionDetail(run, results)
	row.Detail.Live = live
	row.Detail.Debug = debug
	row.Detail.setLog(runLog(run, lines, st, live), debug)
	return nil
}

// runLogSrc is the raw log of one run, as much of it as a reader can have.
type runLogSrc struct {
	Lines []string
	// Seq numbers the last of Lines when they come from the manager's buffer;
	// Dropped counts the lines before them that fell off its ring.
	Seq, Dropped int64
}

// runLog picks a run's log: the manager's buffer while it is live, or while the
// run has ended but its log is not stored yet; the stored log otherwise.
func runLog(run store.IngestRun, stored []string, st ingest.Status, live bool) runLogSrc {
	if live || (len(stored) == 0 && st.Running && st.RunID == run.ID) {
		return runLogSrc{Lines: st.Lines, Seq: st.Seq, Dropped: st.Seq - int64(len(st.Lines))}
	}
	return runLogSrc{Lines: stored}
}

// setLog fills the detail's log from src. Debug and trace lines are left out
// unless debug is set, so a log opens light and the rest is fetched on request.
func (d *ingestionDetail) setLog(src runLogSrc, debug bool) {
	d.HasLog = len(src.Lines) > 0
	d.Logs = displayLogLines(src.Lines, src.Dropped, debug)
	d.NoInfo = true
	for _, l := range d.Logs {
		if !l.Debug {
			d.NoInfo = false
			break
		}
	}
	if d.Live {
		d.Seq = src.Seq
	}
}

// liveRun marks a row as the run in flight: it has a toggle, and its clock
// counts from the manager's start, the same as the bar's.
func liveRun(row *ingestionRun, st ingest.Status) {
	row.Toggle = true
	row.ElapsedMS, row.Duration = runClock(st.StartedAt, time.Now())
}

// openRunsFromQuery reads the ids of the rows the page shows open from
// ?open=1,5. Anything that is not a positive id is ignored, and the list is cut
// at one page of runs.
func openRunsFromQuery(r *http.Request) map[int64]bool {
	open := map[int64]bool{}
	for _, f := range strings.Split(r.URL.Query().Get("open"), ",") {
		if len(open) == ingestHistoryLimit {
			break
		}
		if id, err := strconv.ParseInt(f, 10, 64); err == nil && id > 0 {
			open[id] = true
		}
	}
	return open
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
		Toggle:       r.Status != store.IngestStatusRunning,
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
		run.NewCell = "-"
		run.ElapsedMS, run.Duration = runClock(r.StartedAt, now)
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
func toIngestionDetail(r store.IngestRun, results []store.RunFeedResult) *ingestionDetail {
	d := &ingestionDetail{
		ID:       r.ID,
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
	return d
}

// fmtFeedTook words one feed's fetch time: milliseconds under a second.
func fmtFeedTook(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmtRunDur(d)
}
