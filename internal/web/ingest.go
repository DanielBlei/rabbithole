package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/DanielBlei/rabbithole/internal/store"
)

// ingestHistoryLimit is how many past runs the modal's history table shows.
const ingestHistoryLimit = 6

// ingestBannerWindow is how long after a successful run the "run complete"
// banner keeps rendering when the modal is opened.
const ingestBannerWindow = 5 * time.Minute

// ingestChipData drives the topbar status chip. The chip is ambient signal
// only for the states that need attention: State is "running" (pulsing),
// "failed" (red, with Ago), or "" — idle, in which case the chip renders
// hidden (the element must still exist so htmx OOB swaps can find it).
type ingestChipData struct {
	State string
	Ago   string
	OOB   bool // render with hx-swap-oob so a poll response updates the topbar
}

// ingestKV is one structured field of a captured log line.
type ingestKV struct {
	K, V string
}

// ingestLogLine is one captured zerolog event, parsed for display.
type ingestLogLine struct {
	Time  string // HH:MM:SS local
	Level string // INF/DBG/WRN/ERR/TRC
	Class string // inf/dbg/wrn/err — the colour class
	Debug bool   // hidden by the default INFO filter
	Msg   string
	Kvs   []ingestKV
}

// ingestRunView is one ingest_history row shaped for the template.
type ingestRunView struct {
	Running bool
	Status  string // running | ok | error | cancelled
	When    string // relative start time
	Took    string // run duration, empty while running
	Trigger string // manual | cron
	Counts  store.IngestCounts
	Error   string
}

// ingestBodyData models the runner modal's swappable body: the live state, the
// parsed log tail, the newest finished run, and the history table.
type ingestBodyData struct {
	Running    bool
	StartedAgo string
	Lines      []ingestLogLine
	Last       *ingestRunView // newest finished run; nil if never ran
	ShowBanner bool           // fresh successful finish — offer the feed refresh
	History    []ingestRunView
	Chip       ingestChipData
}

// ingestModalData wraps the body for the full-modal render. PromptUser feeds
// the faux shell prompt in the modal's title bar.
type ingestModalData struct {
	Body       ingestBodyData
	PromptUser string
}

// chromeData is the shared page-chrome model both pages embed for layout.html:
// the ambient topbar chip and the side menu's ingest action state. The same
// struct feeds the standalone chrome fragments (navIngest/navTab/ingWatch)
// that ingest responses re-render out-of-band so the chrome never goes stale.
type chromeData struct {
	Chip    ingestChipData
	IngDot  string // ingest status dot: err | run | "" (healthy/never — no dot)
	IngSub  string // ingest subline shown inside the open side menu
	Running bool   // a run is live — the ingWatch fragment polls while true
	OOB     bool   // render the fragments with hx-swap-oob
}

// chrome assembles the layout's shared state for a full page render. A history
// read failure only degrades the chrome (renders as never-ran) — it never
// blocks the page.
func (s *Web) chrome(ctx context.Context) chromeData {
	st := s.ing.Status()
	if st.Running {
		// A live run only pulses the side menu's dot and edge tab; no topbar chip.
		return chromeData{IngDot: "run", IngSub: "running…", Running: true}
	}
	last, err := s.db.LastIngestRun(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("reading last ingest run for page chrome")
	}
	now := time.Now()
	switch {
	case last == nil:
		return chromeData{IngSub: "never ran"}
	case last.Status == store.IngestStatusOK:
		return chromeData{IngSub: "ok · " + agoPhrase(last.StartedAt, now)}
	case last.Status == store.IngestStatusError:
		// Red chip, tab and menu item until the next successful run.
		ago := agoPhrase(last.StartedAt, now)
		return chromeData{
			Chip:   ingestChipData{State: "failed", Ago: ago},
			IngDot: "err", IngSub: "error · " + ago,
		}
	default:
		// Cancelled: amber chip and dot.
		ago := agoPhrase(last.StartedAt, now)
		return chromeData{
			Chip:   ingestChipData{State: "warn", Ago: ago},
			IngDot: "warn", IngSub: "cancelled · " + ago,
		}
	}
}

// handleIngest returns the runner modal fragment (opened from the side menu
// into #modal), showing the current state — idle summary or live run.
func (s *Web) handleIngest(w http.ResponseWriter, r *http.Request) {
	body, err := s.ingestBody(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := feedTmpl.ExecuteTemplate(w, "ingestModal", ingestModalData{Body: body, PromptUser: s.user}); err != nil {
		// The response is already partially written; log rather than double-write.
		log.Error().Err(err).Msg("render ingest modal")
	}
}

// handleIngestRun starts a manual run (a no-op joining the live run if one is
// already in flight — single-flight lives in the manager) and re-renders the
// modal body in its running state.
func (s *Web) handleIngestRun(w http.ResponseWriter, r *http.Request) {
	if err := s.ing.Start(r.Context(), store.IngestTriggerManual); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderIngestBody(w, r.Context())
}

// handleIngestCancel cancels the in-flight run, if any, and re-renders the
// body. The run winds down asynchronously; polling picks up the final state.
func (s *Web) handleIngestCancel(w http.ResponseWriter, r *http.Request) {
	s.ing.Cancel()
	s.renderIngestBody(w, r.Context())
}

// handleIngestStatus is the poll target: the modal body re-renders itself
// every 2s while a run is live (the hx-get is only emitted in the running
// state, so polling stops by itself when the run ends).
func (s *Web) handleIngestStatus(w http.ResponseWriter, r *http.Request) {
	s.renderIngestBody(w, r.Context())
}

// renderIngestBody writes the modal-body fragment plus the out-of-band chrome
// updates, so every poll/mutation response also keeps the topbar chip, the
// side menu's ingest item and the edge tab in sync on whatever page is open.
func (s *Web) renderIngestBody(w http.ResponseWriter, ctx context.Context) {
	body, err := s.ingestBody(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := feedTmpl.ExecuteTemplate(w, "ingestBody", body); err != nil {
		log.Error().Err(err).Msg("render ingest body")
		return
	}
	s.writeIngestChrome(w, ctx)
}

// handleIngestChrome returns only the OOB chrome fragments (chip, side
// menu ingest item, edge tab, watcher). It's requested with swap:none — by the
// modal's dismiss JS and by the ingWatch poller while a run is live — so the
// ambient state stays fresh even with the runner modal closed.
func (s *Web) handleIngestChrome(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	s.writeIngestChrome(w, r.Context())
}

// writeIngestChrome appends the four OOB chrome fragments to a response.
func (s *Web) writeIngestChrome(w http.ResponseWriter, ctx context.Context) {
	c := s.chrome(ctx)
	c.OOB = true
	chip := c.Chip
	chip.OOB = true
	for _, f := range []struct {
		name string
		data any
	}{
		{"ingestChip", chip}, {"navIngest", c}, {"navTab", c}, {"ingWatch", c},
	} {
		if err := feedTmpl.ExecuteTemplate(w, f.name, f.data); err != nil {
			log.Error().Err(err).Str("fragment", f.name).Msg("render ingest chrome")
		}
	}
}

// ingestBody assembles the modal body's view model from the manager snapshot
// and the history table.
func (s *Web) ingestBody(ctx context.Context) (ingestBodyData, error) {
	st := s.ing.Status()
	runs, err := s.db.ListIngestRuns(ctx, ingestHistoryLimit)
	if err != nil {
		return ingestBodyData{}, err
	}

	now := time.Now()
	data := ingestBodyData{Running: st.Running}
	if st.Running {
		data.StartedAgo = agoPhrase(st.StartedAt, now)
	}
	for _, raw := range st.Lines {
		data.Lines = append(data.Lines, parseIngestLogLine(raw))
	}

	for _, r := range runs {
		v := toIngestRunView(r, now)
		data.History = append(data.History, v)
		if data.Last == nil && r.Status != store.IngestStatusRunning {
			last := v
			data.Last = &last
			if r.Status == store.IngestStatusOK && r.FinishedAt != nil &&
				now.Sub(*r.FinishedAt) < ingestBannerWindow {
				data.ShowBanner = true
			}
		}
	}

	data.Chip = ingestChip(st.Running, data.Last)
	return data, nil
}

// ingestChip derives the topbar chip state: "failed" after an error, "warn"
// after a cancel, hidden otherwise — including while running and after a successful run.
func ingestChip(running bool, last *ingestRunView) ingestChipData {
	if running || last == nil {
		return ingestChipData{}
	}
	switch last.Status {
	case store.IngestStatusError:
		return ingestChipData{State: "failed", Ago: last.When}
	case store.IngestStatusCancelled:
		return ingestChipData{State: "warn", Ago: last.When}
	default:
		return ingestChipData{}
	}
}

// toIngestRunView shapes one history row for display.
func toIngestRunView(r store.IngestRun, now time.Time) ingestRunView {
	v := ingestRunView{
		Running: r.Status == store.IngestStatusRunning,
		Status:  r.Status,
		When:    agoPhrase(r.StartedAt, now),
		Trigger: r.TriggeredBy,
		Counts:  r.Counts,
		Error:   r.Error,
	}
	if r.FinishedAt != nil {
		v.Took = fmtRunDur(r.FinishedAt.Sub(r.StartedAt))
	}
	return v
}

// agoPhrase renders a start time as readable prose: relTime's bare "5m"/"3h"
// units get an " ago", "just now" stays as is, and the date fallback ("2 Jan")
// reads as "on 2 Jan".
func agoPhrase(t, now time.Time) string {
	v := relTime(t, now)
	if v == "just now" {
		return v
	}
	if c := v[len(v)-1]; c == 'm' || c == 'h' || c == 'd' {
		return v + " ago"
	}
	return "on " + v
}

// fmtRunDur renders a run duration compactly: "42s", "3m 05s", "1h 12m".
func fmtRunDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// parseIngestLogLine parses one captured zerolog JSON event into its display
// form: local wall time, level tag, message, and the remaining fields as
// key=value pairs in stable (sorted) order. A line that isn't JSON — it can't
// happen from zerolog, but the buffer is just bytes — degrades to a raw
// message rather than being dropped.
func parseIngestLogLine(raw string) ingestLogLine {
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return ingestLogLine{Level: "???", Class: "inf", Msg: raw}
	}

	ll := ingestLogLine{}
	if lv, ok := m["level"].(string); ok {
		ll.Level, ll.Class, ll.Debug = levelTag(lv)
	}
	if ts, ok := m["time"].(string); ok {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			ll.Time = t.Local().Format("15:04:05")
		}
	}
	if msg, ok := m["message"].(string); ok {
		ll.Msg = msg
	}
	delete(m, "level")
	delete(m, "time")
	delete(m, "message")

	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		ll.Kvs = append(ll.Kvs, ingestKV{K: k, V: fmt.Sprint(m[k])})
	}
	return ll
}

// levelTag maps a zerolog level name onto its display tag, colour class, and
// whether the default INFO filter hides it.
func levelTag(level string) (tag, class string, debug bool) {
	switch level {
	case "debug":
		return "DBG", "dbg", true
	case "trace":
		return "TRC", "dbg", true
	case "warn":
		return "WRN", "wrn", false
	case "error", "fatal", "panic":
		return "ERR", "err", false
	default:
		return "INF", "inf", false
	}
}
