// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	zlog "github.com/rs/zerolog/log"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/profile"
	"github.com/DanielBlei/rabbithole/internal/profilemgr"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// maxLogLines caps the per-run log buffer so a chatty run (trace-level model
// output, many feeds) can't grow memory unbounded; the buffer keeps the tail.
const maxLogLines = 800

// finishTimeout bounds the history-row finalization write after a run ends.
// The run's own context may already be cancelled, so the write gets its own.
const finishTimeout = 5 * time.Second

// pruneTimeout bounds the history trim after a run, which can delete many rows
// and logs.
const pruneTimeout = 30 * time.Second

// runFunc matches Run's signature; Manager calls through it so tests can
// substitute a stub instead of a real fetch→score→record cycle.
type runFunc func(
	context.Context,
	config.Config,
	profile.Snapshot,
	*store.Store,
	time.Time,
	Options,
) (Outcome, error)

type rescoreFunc func(
	context.Context,
	config.Config,
	profile.Snapshot,
	*store.Store,
	time.Time,
	time.Duration,
) (RescoreOutcome, error)

// RunKind identifies which scoring workflow owns a history row and live run.
type RunKind string

const (
	RunKindIngest  RunKind = "ingest"
	RunKindRescore RunKind = "rescore"
)

// noun words the run for its first and last log lines.
func (k RunKind) noun() string {
	if k == RunKindRescore {
		return "rescore"
	}
	return "ingestion"
}

// Manager owns ingest and explicit rescore runs inside the serve process. It
// enforces one shared single-flight slot, runs work on a server-owned context
// so it survives the triggering request, records every run in ingest_history,
// and captures logs for the web UI.
type Manager struct {
	db       *store.Store
	src      config.Source
	profiles *profilemgr.Service
	run      runFunc
	rescore  rescoreFunc
	logLevel zerolog.Level // --debug also prints the run's lines in the terminal
	client   *http.Client  // fetches feeds; nil means feeds.NewClient

	mu     sync.Mutex
	active *activeRun // nil when idle
	buf    *logBuffer // most recent run's log, kept after the run finishes

	// Icon passes run outside the run slot. iconMu admits one pass at a time;
	// root is their context, cancelled by Shutdown; bg tracks them so Shutdown
	// can wait; pending records a kick that arrived while a pass held iconMu. The
	// root, closed, pending and bg's Add are guarded by mu, and iconMu is released
	// under mu so a kick is either run by the finishing pass or starts its own.
	iconMu  sync.Mutex
	root    context.Context
	stop    context.CancelFunc
	closed  bool
	pending bool
	bg      sync.WaitGroup
}

// activeRun is the in-flight cycle's handle.
type activeRun struct {
	id      int64
	started time.Time
	cancel  context.CancelFunc
	done    chan struct{}
	kind    RunKind
	trigger string
	profile string
	window  time.Duration
}

// Status is a point-in-time snapshot for the web UI. Lines is the most recent
// run's captured log (live tail while running, final log after). Row-level
// facts (counts, trigger, outcome) live in ingest_history — read those from
// the store.
type Status struct {
	Running   bool
	RunID     int64     // history row of the live run; zero unless running
	StartedAt time.Time // zero unless running
	Lines     []string  // raw zerolog JSON, one event per line
	// Seq numbers the last line in Lines, counting from 1 across the whole
	// run; the lines before the first one in Lines fell off the ring.
	Seq     int64
	Kind    RunKind
	Profile string
}

// LogChunk is the part of a live run's log newer than a cursor.
type LogChunk struct {
	Lines []string // raw zerolog JSON, oldest first
	// Last is the sequence number of the newest line the run has logged. A
	// caller passes it back as the next cursor.
	Last int64
	// Dropped counts lines between the cursor and the first of Lines that fell
	// off the ring before they were read.
	Dropped int64
}

// NewManager returns a Manager for db that reads its run config from src. logLevel decides whether a run's own
// lines show in the terminal (see newRunLogger). Runs left mid-flight by a
// crashed process are marked failed first, so the UI never shows a run that
// isn't alive.
func NewManager(db *store.Store, src config.Source, logLevel zerolog.Level) (*Manager, error) {
	if err := db.InterruptStaleIngestRuns(context.Background()); err != nil {
		return nil, err
	}
	return &Manager{
		db: db, src: src, profiles: profilemgr.New(db),
		run: Run, rescore: RescoreRecent, logLevel: logLevel,
	}, nil
}

// SetClient sets the client runs fetch feeds through; call it before the first run.
func (m *Manager) SetClient(c *http.Client) { m.client = c }

// Start launches a run in the background and returns immediately. If a run is
// already in flight it is a no-op — the caller shows the live run instead
// (single-flight). ctx covers only the setup writes; the run itself gets a
// server-owned context so closing the browser/request never kills it.
func (m *Manager) Start(ctx context.Context, triggeredBy string) error {
	return m.start(ctx, RunKindIngest, triggeredBy, 0)
}

// StartRescore launches an explicit recent-item rescore using the active
// profile. It shares the ingest manager's single-flight slot, history, logs
// and cancellation lifecycle.
func (m *Manager) StartRescore(ctx context.Context, window time.Duration) error {
	if window <= 0 {
		return errors.New("rescore window must be positive")
	}
	return m.start(ctx, RunKindRescore, store.IngestTriggerProfileRescore, window)
}

func (m *Manager) start(
	ctx context.Context,
	kind RunKind,
	triggeredBy string,
	window time.Duration,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active != nil {
		return nil
	}

	// Resolve one immutable snapshot up front. A selection changed while this
	// run is in flight applies to the next run, never halfway through this one.
	activeProfile, err := m.profiles.Resolve(ctx)
	if err != nil {
		return err
	}
	id, err := m.db.StartIngestRun(ctx, triggeredBy)
	if err != nil {
		return err
	}

	runCtx, cancel := context.WithCancel(context.Background())
	run := &activeRun{
		id: id, started: time.Now(), cancel: cancel, done: make(chan struct{}),
		kind: kind, trigger: triggeredBy, profile: activeProfile.Name, window: window,
	}
	m.active = run
	m.buf = newLogBuffer(maxLogLines)
	go m.execute(runCtx, run, m.buf, activeProfile)
	return nil
}

// Cancel cancels the in-flight run, if any. The run winds down through its
// context (fetch and scoring both honor it) and is recorded as cancelled.
func (m *Manager) Cancel() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active != nil {
		m.active.cancel()
	}
}

// Shutdown cancels any in-flight run and icon pass and blocks until they have
// finished, or ctx is done, whichever comes first. No pass starts afterwards.
func (m *Manager) Shutdown(ctx context.Context) {
	m.mu.Lock()
	m.closed = true
	m.rootLocked()
	m.stop()
	run := m.active
	m.mu.Unlock()

	if run != nil {
		run.cancel()
		select {
		case <-run.done:
		case <-ctx.Done():
			return
		}
	}
	idle := make(chan struct{})
	go func() {
		m.bg.Wait()
		close(idle)
	}()
	select {
	case <-idle:
	case <-ctx.Done():
	}
}

// rootLocked returns the context icon passes run on, creating it on first use.
// Callers hold m.mu.
func (m *Manager) rootLocked() context.Context {
	if m.root == nil {
		m.root, m.stop = context.WithCancel(context.Background())
	}
	return m.root
}

// KickIcons looks up the icons of feeds that have none or a stale one, in the
// background. A kick that arrives while a pass runs is not dropped: the running
// pass is followed by one more. It does nothing after Shutdown.
func (m *Manager) KickIcons() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	if !m.iconMu.TryLock() {
		m.pending = true
		m.mu.Unlock()
		return
	}
	ctx := m.rootLocked()
	m.bg.Add(1)
	m.mu.Unlock()

	go func() {
		defer m.bg.Done()
		RefreshIcons(ctx, m.db, m.client, nil, time.Now())
		m.endIconPass(ctx)
	}()
}

// endIconPass finishes the pass that holds iconMu: while kicks arrived during it,
// it runs one more kick pass per round, then releases iconMu. After Shutdown a
// pending kick is dropped.
func (m *Manager) endIconPass(ctx context.Context) {
	for {
		m.mu.Lock()
		if !m.pending || m.closed {
			m.pending = false
			m.iconMu.Unlock()
			m.mu.Unlock()
			return
		}
		m.pending = false
		m.mu.Unlock()
		RefreshIcons(ctx, m.db, m.client, nil, time.Now())
	}
}

// Status returns the current snapshot: whether a run is live, when it started,
// and the most recent run's captured log lines.
func (m *Manager) Status() Status {
	m.mu.Lock()
	st := Status{}
	if m.active != nil {
		st.Running = true
		st.RunID = m.active.id
		st.StartedAt = m.active.started
		st.Kind = m.active.kind
		st.Profile = m.active.profile
	}
	buf := m.buf
	m.mu.Unlock()
	if buf != nil {
		st.Lines, st.Seq = buf.snapshotSeq()
	}
	return st
}

// LogSince returns the lines of run id's log after sequence number after (zero
// for all of them). It reports false unless id is the run in flight; a caller
// then reads the run's stored log instead.
func (m *Manager) LogSince(id, after int64) (LogChunk, bool) {
	m.mu.Lock()
	buf := m.buf
	live := m.active != nil && m.active.id == id
	m.mu.Unlock()
	if !live || buf == nil {
		return LogChunk{}, false
	}
	return buf.since(after), true
}

// execute runs one cycle to completion and finalizes its history row.
func (m *Manager) execute(
	ctx context.Context,
	run *activeRun,
	buf *logBuffer,
	activeProfile profile.Snapshot,
) {
	defer close(run.done)

	runLogger := newRunLogger(buf, m.logLevel)
	ctx = runLogger.WithContext(ctx)
	var (
		counts store.IngestCounts
		sites  []IconSite
	)
	startLog := runLogger.Info().Str("trigger", run.trigger).Str("profile", activeProfile.Name)
	if run.kind == RunKindRescore {
		startLog = startLog.Str("window", run.window.String())
	}
	startLog.Msg("starting " + run.kind.noun())
	// One snapshot for the whole run: a config change lands on the next run.
	cfg, _, err := m.src.Active(ctx)
	if err != nil {
		err = fmt.Errorf("read config: %w", err)
	}
	switch {
	case err != nil:
	case run.kind == RunKindRescore:
		var outcome RescoreOutcome
		outcome, err = m.rescore(
			ctx, cfg, activeProfile, m.db, time.Now(), run.window,
		)
		counts = store.IngestCounts{
			Fetched: outcome.Candidates,
			Scored:  outcome.Scored,
			Failed:  outcome.Failed,
		}
	default:
		var outcome Outcome
		outcome, err = m.run(ctx, cfg, activeProfile, m.db, time.Now(), Options{
			Think:  cfg.Inference.ThinkEnabled(),
			Record: true,
			Client: m.client,
			RunID:  run.id,
		})
		sites = outcome.Sites
		counts = store.IngestCounts{
			Fetched:  outcome.Fetched,
			NewItems: len(outcome.Unseen),
			Scored:   outcome.Scored,
			Skipped:  outcome.Skipped,
			Failed:   outcome.Failed,
		}
	}

	// A cancelled run doesn't always surface context.Canceled: FetchAll treats
	// per-feed failures — including cancelled fetches — as skippable, so Run can
	// return nil with zero items. The run context is the source of truth.
	status, msg := store.IngestStatusOK, ""
	switch {
	case errors.Is(err, context.Canceled) || ctx.Err() != nil:
		status, msg = store.IngestStatusCancelled, "cancelled"
	case err != nil:
		status, msg = store.IngestStatusError, err.Error()
	}
	// The cycle's own logging stops at the failure point ("ingest complete" is
	// only emitted on success), so append the outcome for the incomplete paths —
	// otherwise the UI's log tail ends mid-stream and reads as still running.
	switch status {
	case store.IngestStatusCancelled:
		runLogger.Warn().Str("kind", string(run.kind)).Msg(string(run.kind) + " cancelled")
	case store.IngestStatusError:
		runLogger.Error().Str("kind", string(run.kind)).Str("error", msg).
			Msg(string(run.kind) + " failed")
	}
	// Every run closes with the same line, after any failure or cancel line
	// that says why.
	runLogger.Info().
		Str("status", string(status)).
		Str("elapsed", time.Since(run.started).Round(100*time.Millisecond).String()).
		Msg(run.kind.noun() + " finished")

	// The run's ctx may be cancelled (that's how Cancel works); the history
	// write must still land, so it gets its own short-lived context.
	finishCtx, cancel := context.WithTimeout(context.Background(), finishTimeout)
	defer cancel()
	if ferr := m.db.FinishIngestRun(finishCtx, run.id, status, counts, msg); ferr != nil {
		zlog.Error().Err(ferr).Int64("run", run.id).Msg("finalizing ingest history row failed")
	}
	// Persist the captured log so past runs stay inspectable, not just the
	// latest (m.buf only holds the most recent run). Best-effort — a save
	// failure must never fail the run.
	if ferr := m.db.SaveIngestRunLog(finishCtx, run.id, buf.snapshot()); ferr != nil {
		zlog.Error().Err(ferr).Int64("run", run.id).Msg("saving ingest run log failed")
	}
	// Trim old runs once this one is finished: the row is no longer 'running',
	// so it counts toward the retained newest. Best-effort, like the log save.
	pruneCtx, pruneCancel := context.WithTimeout(context.Background(), pruneTimeout)
	defer pruneCancel()
	if ferr := m.db.PruneIngestRuns(pruneCtx, 0, 0); ferr != nil {
		zlog.Error().Err(ferr).Int64("run", run.id).Msg("pruning ingest history failed")
	}

	// The one line a plain serve prints for a run, and all an unattended run
	// leaves in the journal. The blow-by-blow is in the modal.
	outcomeLog := zlog.Info()
	if status == store.IngestStatusError {
		outcomeLog = zlog.Error().Str("error", msg)
	}
	outcomeLog.
		Int64("run", run.id).
		Str("status", string(status)).
		Int("fetched", counts.Fetched).
		Int("new", counts.NewItems).
		Int("scored", counts.Scored).
		Int("skipped", counts.Skipped).
		Int("failed", counts.Failed).
		Str("kind", string(run.kind)).
		Str("profile", activeProfile.Name).
		Str("took", time.Since(run.started).Round(time.Second).String()).
		Msg("scoring run finished")

	// The pass is claimed under the lock that frees the slot, so Shutdown either
	// sees it in bg or stops it from starting. It follows a run that succeeded
	// and reported the sites it fetched.
	m.mu.Lock()
	m.active = nil
	pass := run.kind == RunKindIngest && status == store.IngestStatusOK && sites != nil && !m.closed
	var passCtx context.Context
	if pass {
		passCtx = m.rootLocked()
		m.bg.Add(1)
	}
	m.mu.Unlock()
	if pass {
		defer m.bg.Done()
		m.iconMu.Lock()
		RefreshIcons(passCtx, m.db, m.client, sites, time.Now())
		m.endIconPass(passCtx)
	}
}

// newRunLogger builds the logger for one run. Everything lands in buf, which the
// modal tails live and the store keeps. The terminal only gets a copy under
// --debug; otherwise a run would bury the request log there, while showing you
// nothing the modal isn't already showing. execute still prints the outcome.
//
// The logger is scoped to this run, so nothing logged elsewhere reaches buf.
func newRunLogger(buf *logBuffer, logLevel zerolog.Level) zerolog.Logger {
	var w io.Writer = buf
	if logLevel <= zerolog.DebugLevel {
		w = zerolog.MultiLevelWriter(zerolog.ConsoleWriter{Out: os.Stderr}, buf)
	}
	return zerolog.New(w).
		Level(zerolog.DebugLevel).
		With().Timestamp().Logger()
}

// logBuffer is a concurrency-safe ring of the run's most recent log lines.
// zerolog writes one JSON event per Write call, so each call appends one line.
type logBuffer struct {
	mu    sync.Mutex
	lines []string
	total int64 // lines ever written; the newest line's sequence number
	max   int
}

func newLogBuffer(max int) *logBuffer {
	return &logBuffer{max: max}
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lines = append(b.lines, strings.TrimRight(string(p), "\n"))
	b.total++
	if len(b.lines) > b.max {
		b.lines = b.lines[len(b.lines)-b.max:]
	}
	return len(p), nil
}

func (b *logBuffer) snapshot() []string {
	lines, _ := b.snapshotSeq()
	return lines
}

// snapshotSeq returns the buffered lines and the sequence number of the last.
func (b *logBuffer) snapshotSeq() ([]string, int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.lines))
	copy(out, b.lines)
	return out, b.total
}

// since returns the lines numbered above after. Sequence numbers start at 1 and
// never reset, so a cursor older than the ring's oldest line gets the oldest
// onward with the count it missed in Dropped.
func (b *logBuffer) since(after int64) LogChunk {
	b.mu.Lock()
	defer b.mu.Unlock()
	oldest := b.total - int64(len(b.lines)) + 1
	after = min(max(after, 0), b.total)
	chunk := LogChunk{Last: b.total}
	first := after + 1
	if first < oldest {
		chunk.Dropped = oldest - first
		first = oldest
	}
	if first <= b.total {
		chunk.Lines = append([]string(nil), b.lines[first-oldest:]...)
	}
	return chunk
}
