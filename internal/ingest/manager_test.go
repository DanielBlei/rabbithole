package ingest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	zlog "github.com/rs/zerolog/log"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// newManagerForTest builds a Manager over a temp store and a stub runFn that
// blocks until release is closed, then returns outcome/err. The stub replaces
// the real fetch→score→record cycle so no network or model is touched.
func newManagerForTest(t *testing.T, outcome Outcome, runErr error) (*Manager, chan struct{}) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	profile := filepath.Join(t.TempDir(), "profile.md")
	if err := os.WriteFile(profile, []byte("# interests"), 0o644); err != nil {
		t.Fatalf("write profile: %v", err)
	}
	think := true
	cfg := &config.Config{Profile: profile}
	cfg.Inference.Think = &think

	m, err := NewManager(db, cfg, zerolog.InfoLevel)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	release := make(chan struct{})
	m.run = func(ctx context.Context, _ *config.Config, _ string, _ *store.Store, _ time.Time, _ Options) (Outcome, error) {
		select {
		case <-release:
			return outcome, runErr
		case <-ctx.Done():
			return Outcome{}, ctx.Err()
		}
	}
	return m, release
}

// waitIdle blocks until the manager's active run has fully finished (history
// row finalized), failing the test on timeout.
func waitIdle(t *testing.T, m *Manager) {
	t.Helper()
	m.mu.Lock()
	run := m.active
	m.mu.Unlock()
	if run == nil {
		return
	}
	select {
	case <-run.done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not finish")
	}
}

// A full happy-path run: Start flips Status to running and inserts a 'running'
// history row; a second Start while live is a single-flight no-op; the finished
// row carries the outcome's counts.
func TestManagerRunLifecycle(t *testing.T) {
	m, release := newManagerForTest(t, Outcome{Fetched: 20, Scored: 5, Skipped: 10, Failed: 1}, nil)
	ctx := context.Background()

	if err := m.Start(ctx, store.IngestTriggerManual); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if st := m.Status(); !st.Running || st.StartedAt.IsZero() {
		t.Fatalf("status not running after Start: %+v", st)
	}

	// Single-flight: a second Start neither errors nor inserts a second row.
	if err := m.Start(ctx, store.IngestTriggerManual); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	runs, _, err := m.db.ListIngestRuns(ctx, 10, 0)
	if err != nil {
		t.Fatalf("ListIngestRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("single-flight violated, %d history rows", len(runs))
	}

	close(release)
	waitIdle(t, m)

	if st := m.Status(); st.Running {
		t.Error("still running after finish")
	}
	last, err := m.db.LastIngestRun(ctx)
	if err != nil {
		t.Fatalf("LastIngestRun: %v", err)
	}
	want := store.IngestCounts{Fetched: 20, Scored: 5, Skipped: 10, Failed: 1}
	if last.Status != store.IngestStatusOK || last.Counts != want {
		t.Errorf("finished row wrong: %+v", last)
	}
}

// Cancel winds the run down through its context and records it as cancelled.
func TestManagerCancel(t *testing.T) {
	m, _ := newManagerForTest(t, Outcome{}, nil)
	ctx := context.Background()

	if err := m.Start(ctx, store.IngestTriggerManual); err != nil {
		t.Fatalf("Start: %v", err)
	}
	m.Cancel()
	waitIdle(t, m)

	last, err := m.db.LastIngestRun(ctx)
	if err != nil {
		t.Fatalf("LastIngestRun: %v", err)
	}
	if last.Status != store.IngestStatusCancelled {
		t.Errorf("status = %q, want cancelled", last.Status)
	}
	if !logContains(m, "ingest cancelled") {
		t.Errorf("log buffer missing final cancelled line: %q", m.Status().Lines)
	}
}

// logContains reports whether any captured log line contains substr.
func logContains(m *Manager, substr string) bool {
	for _, ln := range m.Status().Lines {
		if strings.Contains(ln, substr) {
			return true
		}
	}
	return false
}

// A run whose cycle errors is recorded as an error with the message preserved.
func TestManagerRunError(t *testing.T) {
	m, release := newManagerForTest(t, Outcome{}, errors.New("vllm: connection refused"))
	ctx := context.Background()

	if err := m.Start(ctx, store.IngestTriggerManual); err != nil {
		t.Fatalf("Start: %v", err)
	}
	close(release)
	waitIdle(t, m)

	last, err := m.db.LastIngestRun(ctx)
	if err != nil {
		t.Fatalf("LastIngestRun: %v", err)
	}
	if last.Status != store.IngestStatusError || last.Error != "vllm: connection refused" {
		t.Errorf("error row wrong: %+v", last)
	}
	if !logContains(m, "ingest failed") || !logContains(m, "vllm: connection refused") {
		t.Errorf("log buffer missing final failure line: %q", m.Status().Lines)
	}
}

// NewManager flips a stale 'running' row (crashed process) to an error before
// any new run can start.
func TestManagerInterruptsStaleRuns(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if _, err := db.StartIngestRun(ctx, store.IngestTriggerManual); err != nil {
		t.Fatalf("StartIngestRun: %v", err)
	}

	think := true
	cfg := &config.Config{}
	cfg.Inference.Think = &think
	if _, err := NewManager(db, cfg, zerolog.InfoLevel); err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	last, err := db.LastIngestRun(ctx)
	if err != nil {
		t.Fatalf("LastIngestRun: %v", err)
	}
	if last.Status != store.IngestStatusError || last.Error != "interrupted" {
		t.Errorf("stale row not interrupted: %+v", last)
	}
}

// A Debug-level line lands in the run's buffer even when the manager's
// configured logLevel is Info (the serve default without --debug) — the
// buffer always captures everything; only the stderr mirror is filtered.
func TestManagerBufferCapturesDebugRegardlessOfLogLevel(t *testing.T) {
	m, release := newManagerForTest(t, Outcome{}, nil) // newManagerForTest uses zerolog.InfoLevel
	orig := m.run
	m.run = func(ctx context.Context, cfg *config.Config, profile string, db *store.Store, day time.Time, opts Options) (Outcome, error) {
		zerolog.Ctx(ctx).Debug().Msg("debug-level line")
		return orig(ctx, cfg, profile, db, day, opts)
	}

	if err := m.Start(context.Background(), store.IngestTriggerManual); err != nil {
		t.Fatalf("Start: %v", err)
	}
	close(release)
	waitIdle(t, m)

	if !logContains(m, "debug-level line") {
		t.Errorf("buffer missing debug-level line despite Info logLevel: %q", m.Status().Lines)
	}
}

// A log line emitted through the per-run context logger lands in the run's
// buffer; a line emitted through the process-global logger during the same
// window does not — the run's log capture is isolated from concurrent,
// unrelated logging (e.g. an HTTP handler logging via the global logger while
// a run is in flight).
func TestManagerLogsRunContextOnly(t *testing.T) {
	m, release := newManagerForTest(t, Outcome{}, nil)
	orig := m.run
	m.run = func(ctx context.Context, cfg *config.Config, profile string, db *store.Store, day time.Time, opts Options) (Outcome, error) {
		zerolog.Ctx(ctx).Info().Msg("run-scoped line")
		zlog.Info().Msg("global-scoped line")
		return orig(ctx, cfg, profile, db, day, opts)
	}

	ctx := context.Background()
	if err := m.Start(ctx, store.IngestTriggerManual); err != nil {
		t.Fatalf("Start: %v", err)
	}
	close(release)
	waitIdle(t, m)

	if !logContains(m, "run-scoped line") {
		t.Errorf("log buffer missing run-scoped line: %q", m.Status().Lines)
	}
	if logContains(m, "global-scoped line") {
		t.Errorf("log buffer picked up a global-logger line, isolation broken: %q", m.Status().Lines)
	}
}

// Shutdown cancels and waits for an in-flight run before returning.
func TestManagerShutdownWaitsForActiveRun(t *testing.T) {
	m, _ := newManagerForTest(t, Outcome{}, nil)
	ctx := context.Background()
	if err := m.Start(ctx, store.IngestTriggerManual); err != nil {
		t.Fatalf("Start: %v", err)
	}

	m.Shutdown(context.Background())

	if st := m.Status(); st.Running {
		t.Error("still running after Shutdown")
	}
	last, err := m.db.LastIngestRun(ctx)
	if err != nil {
		t.Fatalf("LastIngestRun: %v", err)
	}
	if last.Status != store.IngestStatusCancelled {
		t.Errorf("status = %q, want cancelled", last.Status)
	}
}

// Shutdown is a no-op when no run is active.
func TestManagerShutdownIdleNoop(t *testing.T) {
	m, _ := newManagerForTest(t, Outcome{}, nil)
	m.Shutdown(context.Background())
	if st := m.Status(); st.Running {
		t.Error("Shutdown on an idle manager reported running")
	}
}

// Shutdown returns at ctx's deadline rather than hanging if the run ignores
// cancellation.
func TestManagerShutdownTimesOut(t *testing.T) {
	m, _ := newManagerForTest(t, Outcome{}, nil)
	m.run = func(ctx context.Context, _ *config.Config, _ string, _ *store.Store, _ time.Time, _ Options) (Outcome, error) {
		<-make(chan struct{}) // never returns; ignores ctx cancellation
		return Outcome{}, nil
	}
	if err := m.Start(context.Background(), store.IngestTriggerManual); err != nil {
		t.Fatalf("Start: %v", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		m.Shutdown(shutdownCtx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not return at ctx deadline")
	}
}
