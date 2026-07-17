package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ingestSchema is the ingest run history — one row per fetch→score→record
// cycle, whether triggered from the web UI or (later) by a scheduler. Open
// execs it alongside the items/todos/ideas migrations.
//
// A row is inserted as status 'running' when the run starts and finalized by
// FinishIngestRun; finished_at is NULL while the run is live. A 'running' row
// left behind by a crashed process is flipped to an error by
// InterruptStaleIngestRuns on the next startup, so "running" in this table is
// always at most one live row.
const ingestSchema = `
CREATE TABLE IF NOT EXISTS ingest_history (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	started_at   TIMESTAMP NOT NULL,
	finished_at  TIMESTAMP,
	status       TEXT NOT NULL,
	triggered_by TEXT NOT NULL,
	fetched      INTEGER NOT NULL DEFAULT 0,
	new_items    INTEGER NOT NULL DEFAULT 0,
	scored       INTEGER NOT NULL DEFAULT 0,
	skipped      INTEGER NOT NULL DEFAULT 0,
	failed       INTEGER NOT NULL DEFAULT 0,
	error        TEXT NOT NULL DEFAULT ''
);
`

// Ingest run statuses. Running means the row's run is still in flight.
const (
	IngestStatusRunning   = "running"
	IngestStatusOK        = "ok"
	IngestStatusError     = "error"
	IngestStatusCancelled = "cancelled"
)

// Ingest run triggers: who started the run.
const (
	IngestTriggerManual = "manual"
	IngestTriggerCron   = "cron"
)

// ErrIngestRunNotFound is returned when no history row matches the given id.
var ErrIngestRunNotFound = errors.New("ingest run not found")

// IngestCounts are one run's item totals, recorded when the run finishes.
type IngestCounts struct {
	Fetched  int // items fetched across all feeds
	NewItems int // fresh, not-yet-seen items considered for scoring
	Scored   int // items the model scored
	Skipped  int // items skipped as already scored
	Failed   int // items the model failed to score
}

// IngestRun is one row of ingest_history. FinishedAt is nil while the run is
// live. Error carries the failure message for error/cancelled runs.
type IngestRun struct {
	ID          int64
	StartedAt   time.Time
	FinishedAt  *time.Time
	Status      string
	TriggeredBy string
	Counts      IngestCounts
	Error       string
}

// ingestColumns is the SELECT list backing the ingest queries, kept in one
// place so its order stays in lockstep with scanIngestRun.
const ingestColumns = "id, started_at, finished_at, status, triggered_by, fetched, new_items, scored, skipped, failed, error"

// scanIngestRun maps one row of ingestColumns onto an IngestRun. Column order
// must match ingestColumns exactly.
func scanIngestRun(sc rowScanner) (IngestRun, error) {
	var (
		r          IngestRun
		finishedAt sql.NullTime
	)
	if err := sc.Scan(&r.ID, &r.StartedAt, &finishedAt, &r.Status, &r.TriggeredBy,
		&r.Counts.Fetched, &r.Counts.NewItems, &r.Counts.Scored, &r.Counts.Skipped,
		&r.Counts.Failed, &r.Error); err != nil {
		return IngestRun{}, err
	}
	if finishedAt.Valid {
		r.FinishedAt = &finishedAt.Time
	}
	return r, nil
}

// StartIngestRun inserts a new 'running' history row for a run triggered by
// triggeredBy (IngestTriggerManual/IngestTriggerCron) and returns its id, which
// the caller hands to FinishIngestRun when the run ends.
func (s *Store) StartIngestRun(ctx context.Context, triggeredBy string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		"INSERT INTO ingest_history (started_at, status, triggered_by) VALUES (?, ?, ?)",
		time.Now(), IngestStatusRunning, triggeredBy)
	if err != nil {
		return 0, fmt.Errorf("insert ingest run: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("ingest run insert id: %w", err)
	}
	return id, nil
}

// FinishIngestRun finalizes a running row: stamps finished_at, sets the outcome
// status (IngestStatusOK/Error/Cancelled), the item counts, and the failure
// message (empty for an ok run). Returns ErrIngestRunNotFound if id doesn't
// match a running row.
func (s *Store) FinishIngestRun(
	ctx context.Context,
	id int64,
	status string,
	counts IngestCounts,
	errMsg string,
) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE ingest_history
		 SET finished_at = ?, status = ?, fetched = ?, new_items = ?, scored = ?, skipped = ?, failed = ?, error = ?
		 WHERE id = ? AND status = ?`,
		time.Now(), status, counts.Fetched, counts.NewItems, counts.Scored, counts.Skipped, counts.Failed,
		errMsg, id, IngestStatusRunning)
	if err != nil {
		return fmt.Errorf("finish ingest run %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %d", ErrIngestRunNotFound, id)
	}
	return nil
}

// LastIngestRun returns the most recently started run, or nil if none exists
// yet (a fresh database).
func (s *Store) LastIngestRun(ctx context.Context) (*IngestRun, error) {
	r, err := scanIngestRun(s.db.QueryRowContext(ctx,
		"SELECT "+ingestColumns+" FROM ingest_history ORDER BY started_at DESC, id DESC LIMIT 1"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("last ingest run: %w", err)
	}
	return &r, nil
}

// ListIngestRuns returns the newest limit runs, most recent first.
func (s *Store) ListIngestRuns(ctx context.Context, limit int) ([]IngestRun, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+ingestColumns+" FROM ingest_history ORDER BY started_at DESC, id DESC LIMIT ?", limit)
	if err != nil {
		return nil, fmt.Errorf("query ingest runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var runs []IngestRun
	for rows.Next() {
		r, err := scanIngestRun(rows)
		if err != nil {
			return nil, fmt.Errorf("scan ingest run: %w", err)
		}
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// InterruptStaleIngestRuns flips any leftover 'running' rows to an error —
// crash recovery for a process that died mid-run. Called once at startup by
// the run manager, before any new run can start, so a stale "running" can
// never be mistaken for a live one.
func (s *Store) InterruptStaleIngestRuns(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx,
		"UPDATE ingest_history SET finished_at = ?, status = ?, error = 'interrupted' WHERE status = ?",
		time.Now(), IngestStatusError, IngestStatusRunning); err != nil {
		return fmt.Errorf("interrupt stale ingest runs: %w", err)
	}
	return nil
}
