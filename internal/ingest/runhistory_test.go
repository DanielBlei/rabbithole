// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/profile"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// newRealRunManager builds a Manager whose runs use the real cycle over feeds
// served from loopback, so fetches reach the store.
func newRealRunManager(t *testing.T, feeds ...config.Feed) (*Manager, *store.Store) {
	t.Helper()
	m, db, _ := newRealRunManagerAt(t, feeds...)
	return m, db
}

// newRealRunManagerAt is newRealRunManager that also returns the database path.
func newRealRunManagerAt(t *testing.T, feeds ...config.Feed) (*Manager, *store.Store, string) {
	t.Helper()
	cfg := testConfig(t, feeds...)
	db := openStore(t, cfg)
	m, err := NewManager(db, config.Static(cfg.Config), zerolog.InfoLevel)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	m.SetClient(loopbackClient)
	return m, db, cfg.dbPath
}

func runAndWait(t *testing.T, m *Manager) {
	t.Helper()
	if err := m.Start(context.Background(), store.IngestTriggerManual); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitIdle(t, m)
}

// seedFinishedRuns inserts n finished history rows, oldest first.
func seedFinishedRuns(t *testing.T, db *store.Store, n int) {
	t.Helper()
	ctx := context.Background()
	for range n {
		id, err := db.StartIngestRun(ctx, store.IngestTriggerManual)
		if err != nil {
			t.Fatalf("StartIngestRun: %v", err)
		}
		if err := db.FinishIngestRun(ctx, id, store.IngestStatusOK, store.IngestCounts{}, ""); err != nil {
			t.Fatalf("FinishIngestRun: %v", err)
		}
	}
}

func historyLen(t *testing.T, db *store.Store) int {
	t.Helper()
	runs, _, err := db.ListIngestRuns(context.Background(), 1000, 0)
	if err != nil {
		t.Fatalf("ListIngestRuns: %v", err)
	}
	return len(runs)
}

// A manager ingest run links each feed it fetched, failed ones included, to
// its own history row.
func TestManagerRunLinksFeedFetches(t *testing.T) {
	good := config.Feed{Name: "Good", URL: serveRSS(t, feedRSS("A", "https://x.test/a"))}
	bad := config.Feed{Name: "Bad", URL: serveRSS(t, "not xml at all")}
	m, db := newRealRunManager(t, good, bad)

	runAndWait(t, m)

	last, err := db.LastIngestRun(context.Background())
	if err != nil || last == nil {
		t.Fatalf("LastIngestRun: %v, %v", last, err)
	}
	got, err := db.RunFeedResults(context.Background(), last.ID)
	if err != nil {
		t.Fatalf("RunFeedResults: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("linked feeds = %d, want 2: %+v", len(got), got)
	}
	byName := map[string]store.RunFeedResult{}
	for _, r := range got {
		byName[r.Name] = r
	}
	if !byName["Good"].OK() || byName["Bad"].OK() {
		t.Errorf("results = %+v", byName)
	}
}

// A run with RunID 0, as the CLI makes, records feed history but links none.
func TestRunWithoutRunIDWritesNoLinks(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t, config.Feed{Name: "Solo", URL: serveRSS(t, feedRSS("A", "https://x.test/a"))})
	db := openStore(t, cfg)
	runID, err := db.StartIngestRun(ctx, store.IngestTriggerManual)
	if err != nil {
		t.Fatalf("StartIngestRun: %v", err)
	}

	if _, err := Run(ctx, cfg.Config, testProfile("test"), db, time.Now(),
		Options{Record: true, Client: loopbackClient}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got, err := db.RunFeedResults(ctx, runID)
	if err != nil {
		t.Fatalf("RunFeedResults: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("links = %+v, want none", got)
	}
	health, err := db.FeedHealthByID(ctx, 10)
	if err != nil {
		t.Fatalf("FeedHealthByID: %v", err)
	}
	if len(health) != 1 {
		t.Errorf("feed history rows = %d, want 1", len(health))
	}
}

// The manager hands its history row id to the cycle.
func TestManagerPassesRunID(t *testing.T) {
	m, release := newManagerForTest(t, Outcome{}, nil)
	got := make(chan int64, 1)
	m.run = func(
		_ context.Context, _ config.Config, _ profile.Snapshot, _ *store.Store, _ time.Time, opts Options,
	) (Outcome, error) {
		got <- opts.RunID
		return Outcome{}, nil
	}
	if err := m.Start(context.Background(), store.IngestTriggerManual); err != nil {
		t.Fatalf("Start: %v", err)
	}
	close(release)
	waitIdle(t, m)
	last, err := m.db.LastIngestRun(context.Background())
	if err != nil || last == nil {
		t.Fatalf("LastIngestRun: %v, %v", last, err)
	}
	if id := <-got; id != last.ID {
		t.Errorf("Options.RunID = %d, want %d", id, last.ID)
	}
}

// A rescore run fetches nothing, so it writes no feed fetch rows or links.
func TestManagerRescoreWritesNoFetches(t *testing.T) {
	m, db := newRealRunManager(t, config.Feed{Name: "Solo", URL: serveRSS(t, feedRSS("A", "https://x.test/a"))})
	m.rescore = func(
		context.Context, config.Config, profile.Snapshot, *store.Store, time.Time, time.Duration,
	) (RescoreOutcome, error) {
		return RescoreOutcome{Candidates: 3, Scored: 3}, nil
	}
	if err := m.StartRescore(context.Background(), ProfileRescoreWindow); err != nil {
		t.Fatalf("StartRescore: %v", err)
	}
	waitIdle(t, m)

	last, err := db.LastIngestRun(context.Background())
	if err != nil || last == nil {
		t.Fatalf("LastIngestRun: %v, %v", last, err)
	}
	if got, err := db.RunFeedResults(context.Background(), last.ID); err != nil || len(got) != 0 {
		t.Errorf("rescore links = %+v, err %v", got, err)
	}
	health, err := db.FeedHealthByID(context.Background(), 10)
	if err != nil {
		t.Fatalf("FeedHealthByID: %v", err)
	}
	if len(health) != 0 {
		t.Errorf("rescore wrote feed history: %+v", health)
	}
}

// A finished run trims the history to the newest 100 rows, itself included.
func TestManagerPrunesHistoryAfterRun(t *testing.T) {
	m, release := newManagerForTest(t, Outcome{}, nil)
	seedFinishedRuns(t, m.db, 105)
	if n := historyLen(t, m.db); n != 105 {
		t.Fatalf("seeded runs = %d, want 105", n)
	}

	if err := m.Start(context.Background(), store.IngestTriggerManual); err != nil {
		t.Fatalf("Start: %v", err)
	}
	close(release)
	waitIdle(t, m)

	if n := historyLen(t, m.db); n != 100 {
		t.Errorf("history after run = %d, want 100", n)
	}
	last, err := m.db.LastIngestRun(context.Background())
	if err != nil || last == nil || last.Status != store.IngestStatusOK {
		t.Errorf("finished run lost or not ok: %+v, %v", last, err)
	}
}

// A failing prune is logged and never changes the run's outcome. The links
// table is dropped through a second connection so only the prune transaction
// fails; the history row and its counts still land.
func TestManagerPruneErrorDoesNotFailRun(t *testing.T) {
	m, db, path := newRealRunManagerAt(t)
	m.run = func(
		context.Context, config.Config, profile.Snapshot, *store.Store, time.Time, Options,
	) (Outcome, error) {
		return Outcome{Fetched: 4}, nil
	}
	seedFinishedRuns(t, db, 105)
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	if _, err := raw.ExecContext(context.Background(), "DROP TABLE ingest_run_fetches"); err != nil {
		t.Fatalf("drop links table: %v", err)
	}

	runAndWait(t, m)

	last, err := db.LastIngestRun(context.Background())
	if err != nil || last == nil {
		t.Fatalf("LastIngestRun: %v, %v", last, err)
	}
	if last.Status != store.IngestStatusOK || last.Counts.Fetched != 4 {
		t.Errorf("run = %+v, want ok with its counts", last)
	}
	if n := historyLen(t, db); n != 106 {
		t.Errorf("history = %d, want 106 (prune failed, nothing trimmed)", n)
	}
}

// A cancelled run ends cancelled, writes no links and still prunes.
func TestManagerCancelledRunWritesNoLinks(t *testing.T) {
	started := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	m, db := newRealRunManager(t, config.Feed{Name: "Slow", URL: srv.URL})
	seedFinishedRuns(t, db, 105)

	if err := m.Start(context.Background(), store.IngestTriggerManual); err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("feed was never requested")
	}
	m.Cancel()
	waitIdle(t, m)

	last, err := db.LastIngestRun(context.Background())
	if err != nil || last == nil {
		t.Fatalf("LastIngestRun: %v, %v", last, err)
	}
	if last.Status != store.IngestStatusCancelled {
		t.Errorf("status = %q, want cancelled", last.Status)
	}
	if got, err := db.RunFeedResults(context.Background(), last.ID); err != nil || len(got) != 0 {
		t.Errorf("cancelled run links = %+v, err %v", got, err)
	}
	if n := historyLen(t, db); n != 100 {
		t.Errorf("history after cancelled run = %d, want 100", n)
	}
}
