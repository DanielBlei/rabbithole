package ingest

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	zlog "github.com/rs/zerolog/log"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// maxLogLines caps the per-run log buffer so a chatty run (trace-level model
// output, many feeds) can't grow memory unbounded; the buffer keeps the tail.
const maxLogLines = 800

// finishTimeout bounds the history-row finalization write after a run ends.
// The run's own context may already be cancelled, so the write gets its own.
const finishTimeout = 5 * time.Second

// runFunc matches Run's signature; Manager calls through it so tests can
// substitute a stub instead of a real fetch→score→record cycle.
type runFunc func(context.Context, *config.Config, string, *store.Store, time.Time, Options) (Outcome, error)

// Manager owns manual (and later scheduled) ingest runs inside the serve
// process: it enforces single-flight — at most one cycle at a time — runs the
// cycle on a server-owned context so it survives the HTTP request that
// triggered it, records every run in the store's ingest_history, and captures
// the run's log output for the web UI.
type Manager struct {
	db  *store.Store
	cfg *config.Config
	run runFunc

	mu     sync.Mutex
	active *activeRun // nil when idle
	buf    *logBuffer // most recent run's log, kept after the run finishes
}

// activeRun is the in-flight cycle's handle.
type activeRun struct {
	id      int64
	started time.Time
	cancel  context.CancelFunc
	done    chan struct{}
}

// Status is a point-in-time snapshot for the web UI. Lines is the most recent
// run's captured log (live tail while running, final log after). Row-level
// facts (counts, trigger, outcome) live in ingest_history — read those from
// the store.
type Status struct {
	Running   bool
	StartedAt time.Time // zero unless running
	Lines     []string  // raw zerolog JSON, one event per line
}

// NewManager returns a Manager for db/cfg. History rows left as 'running' by
// a process that died mid-run are flipped to errors first, so the UI never
// reports a run that is no longer alive.
func NewManager(db *store.Store, cfg *config.Config) (*Manager, error) {
	if err := db.InterruptStaleIngestRuns(context.Background()); err != nil {
		return nil, err
	}
	return &Manager{db: db, cfg: cfg, run: Run}, nil
}

// Start launches a run in the background and returns immediately. If a run is
// already in flight it is a no-op — the caller shows the live run instead
// (single-flight). ctx covers only the setup writes; the run itself gets a
// server-owned context so closing the browser/request never kills it.
func (m *Manager) Start(ctx context.Context, triggeredBy string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active != nil {
		return nil
	}

	// Load the profile up front so a bad config fails the triggering request
	// rather than the background run.
	profile, err := m.cfg.LoadProfile()
	if err != nil {
		return err
	}
	id, err := m.db.StartIngestRun(ctx, triggeredBy)
	if err != nil {
		return err
	}

	runCtx, cancel := context.WithCancel(context.Background())
	run := &activeRun{id: id, started: time.Now(), cancel: cancel, done: make(chan struct{})}
	m.active = run
	m.buf = newLogBuffer(maxLogLines)
	go m.execute(runCtx, run, m.buf, profile)
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

// Status returns the current snapshot: whether a run is live, when it started,
// and the most recent run's captured log lines.
func (m *Manager) Status() Status {
	m.mu.Lock()
	st := Status{}
	if m.active != nil {
		st.Running = true
		st.StartedAt = m.active.started
	}
	buf := m.buf
	m.mu.Unlock()
	if buf != nil {
		st.Lines = buf.snapshot()
	}
	return st
}

// execute runs one cycle to completion and finalizes its history row.
func (m *Manager) execute(ctx context.Context, run *activeRun, buf *logBuffer, profile string) {
	defer close(run.done)

	restore := captureLogs(buf)
	outcome, err := m.run(ctx, m.cfg, profile, m.db, time.Now(), Options{
		Think:  *m.cfg.Inference.Think,
		Record: true,
	})
	restore()

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
	tail := zerolog.New(buf).With().Timestamp().Logger()
	switch status {
	case store.IngestStatusCancelled:
		tail.Warn().Msg("ingest cancelled")
	case store.IngestStatusError:
		tail.Error().Str("error", msg).Msg("ingest failed")
	}
	counts := store.IngestCounts{
		Fetched:  outcome.Fetched,
		NewItems: len(outcome.Unseen),
		Scored:   outcome.Scored,
		Skipped:  outcome.Skipped,
		Failed:   outcome.Failed,
	}

	// The run's ctx may be cancelled (that's how Cancel works); the history
	// write must still land, so it gets its own short-lived context.
	finishCtx, cancel := context.WithTimeout(context.Background(), finishTimeout)
	defer cancel()
	if ferr := m.db.FinishIngestRun(finishCtx, run.id, status, counts, msg); ferr != nil {
		zlog.Error().Err(ferr).Int64("run", run.id).Msg("finalizing ingest history row failed")
	}

	m.mu.Lock()
	m.active = nil
	m.mu.Unlock()
}

// captureLogs redirects the global zerolog logger for the duration of a run:
// every event — the ingest cycle logs via the global, and so do feeds, rank and
// inference — is written as JSON into buf at debug level, while stderr keeps
// receiving the console format filtered to the level the process was started
// with (so a serve without --debug doesn't suddenly get chatty). Returns the
// restore func. Only ever active during a run; single-flight makes the global
// swap safe from overlapping captures.
func captureLogs(buf *logBuffer) (restore func()) {
	prevLogger := zlog.Logger
	prevLevel := zerolog.GlobalLevel()

	console := minLevelWriter{
		w:   zerolog.LevelWriterAdapter{Writer: zerolog.ConsoleWriter{Out: os.Stderr}},
		min: prevLevel,
	}
	zlog.Logger = zerolog.New(zerolog.MultiLevelWriter(console, buf)).
		Level(zerolog.DebugLevel).
		With().Timestamp().Logger()
	zerolog.SetGlobalLevel(zerolog.DebugLevel)

	return func() {
		zlog.Logger = prevLogger
		zerolog.SetGlobalLevel(prevLevel)
	}
}

// minLevelWriter drops events below min — it keeps stderr at the operator's
// chosen verbosity while the sibling buffer writer receives everything.
type minLevelWriter struct {
	w   zerolog.LevelWriter
	min zerolog.Level
}

func (m minLevelWriter) Write(p []byte) (int, error) {
	return m.w.Write(p)
}

func (m minLevelWriter) WriteLevel(l zerolog.Level, p []byte) (int, error) {
	if l < m.min {
		return len(p), nil
	}
	return m.w.WriteLevel(l, p)
}

// logBuffer is a concurrency-safe ring of the run's most recent log lines.
// zerolog writes one JSON event per Write call, so each call appends one line.
type logBuffer struct {
	mu    sync.Mutex
	lines []string
	max   int
}

func newLogBuffer(max int) *logBuffer {
	return &logBuffer{max: max}
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lines = append(b.lines, strings.TrimRight(string(p), "\n"))
	if len(b.lines) > b.max {
		b.lines = b.lines[len(b.lines)-b.max:]
	}
	return len(p), nil
}

func (b *logBuffer) snapshot() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.lines))
	copy(out, b.lines)
	return out
}
