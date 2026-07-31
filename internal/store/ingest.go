// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
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

// ingestLogSchema holds each run's full captured log in its own table so
// listing runs never drags the log bodies along.
const ingestLogSchema = `
CREATE TABLE IF NOT EXISTS ingest_run_logs (
	run_id INTEGER PRIMARY KEY REFERENCES ingest_history(id) ON DELETE CASCADE,
	log    TEXT NOT NULL DEFAULT ''
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
	Fetched  int // items within the configured recency window, across all feeds
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

// SQL inventory for ingest_history / ingest_run_logs — kept together so the
// queries are easy to scan and maintain rather than buried in each method.
const (
	sqlInsertIngestRun = "INSERT INTO ingest_history (started_at, status, triggered_by) VALUES (?, ?, ?)"

	sqlFinishIngestRun = `UPDATE ingest_history
		SET finished_at = ?, status = ?, fetched = ?, new_items = ?, scored = ?, skipped = ?, failed = ?, error = ?
		WHERE id = ? AND status = ?`

	sqlInterruptIngestRuns = "UPDATE ingest_history SET finished_at = ?, status = ?, error = 'interrupted' WHERE status = ?"

	sqlLastIngestRun = "SELECT " + ingestColumns + " FROM ingest_history ORDER BY started_at DESC, id DESC LIMIT 1"

	sqlListIngestRuns = "SELECT " + ingestColumns + " FROM ingest_history ORDER BY started_at DESC, id DESC LIMIT ? OFFSET ?"

	// Joins the run's stored log (empty string when none was saved).
	sqlGetIngestRunWithLog = "SELECT " + ingestColumns + ", COALESCE(l.log, '') " +
		"FROM ingest_history h LEFT JOIN ingest_run_logs l ON l.run_id = h.id WHERE h.id = ?"

	sqlSaveIngestRunLog = "INSERT INTO ingest_run_logs (run_id, log) VALUES (?, ?) " +
		"ON CONFLICT(run_id) DO UPDATE SET log = excluded.log"
)

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

// scanIngestRunWithLog is scanIngestRun plus a trailing log column, for the
// sqlGetIngestRunWithLog join. Column order must match that query.
func scanIngestRunWithLog(sc rowScanner) (IngestRun, string, error) {
	var (
		r          IngestRun
		finishedAt sql.NullTime
		log        string
	)
	if err := sc.Scan(&r.ID, &r.StartedAt, &finishedAt, &r.Status, &r.TriggeredBy,
		&r.Counts.Fetched, &r.Counts.NewItems, &r.Counts.Scored, &r.Counts.Skipped,
		&r.Counts.Failed, &r.Error, &log); err != nil {
		return IngestRun{}, "", err
	}
	if finishedAt.Valid {
		r.FinishedAt = &finishedAt.Time
	}
	return r, log, nil
}

// StartIngestRun inserts a new 'running' history row for a run triggered by
// triggeredBy (IngestTriggerManual/IngestTriggerCron) and returns its id, which
// the caller hands to FinishIngestRun when the run ends.
func (s *Store) StartIngestRun(ctx context.Context, triggeredBy string) (int64, error) {
	res, err := s.db.ExecContext(ctx, sqlInsertIngestRun,
		sqlTime(time.Now()), IngestStatusRunning, triggeredBy)
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
	res, err := s.db.ExecContext(ctx, sqlFinishIngestRun,
		sqlTime(time.Now()), status, counts.Fetched, counts.NewItems, counts.Scored, counts.Skipped, counts.Failed,
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
	r, err := scanIngestRun(s.db.QueryRowContext(ctx, sqlLastIngestRun))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("last ingest run: %w", err)
	}
	return &r, nil
}

// GetIngestRunWithLog returns one run and its captured log lines in a single
// query. The log slice is nil when the run has no stored log (e.g. a run from
// before logs were persisted). ErrIngestRunNotFound if no run matches id.
func (s *Store) GetIngestRunWithLog(ctx context.Context, id int64) (*IngestRun, []string, error) {
	r, log, err := scanIngestRunWithLog(s.db.QueryRowContext(ctx, sqlGetIngestRunWithLog, id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, fmt.Errorf("%w: %d", ErrIngestRunNotFound, id)
		}
		return nil, nil, fmt.Errorf("get ingest run %d: %w", id, err)
	}
	var lines []string
	if log != "" {
		lines = strings.Split(log, "\n")
	}
	return &r, lines, nil
}

// ListIngestRuns returns up to limit runs starting at offset, most recent
// first, plus hasMore: whether a further page exists. It queries limit+1 rows
// and trims the extra, so paging needs no separate COUNT.
func (s *Store) ListIngestRuns(ctx context.Context, limit, offset int) (runs []IngestRun, hasMore bool, err error) {
	rows, err := s.db.QueryContext(ctx, sqlListIngestRuns, limit+1, offset)
	if err != nil {
		return nil, false, fmt.Errorf("query ingest runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		r, err := scanIngestRun(rows)
		if err != nil {
			return nil, false, fmt.Errorf("scan ingest run: %w", err)
		}
		runs = append(runs, r)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(runs) > limit {
		return runs[:limit], true, nil
	}
	return runs, false, nil
}

// SaveIngestRunLog records a run's captured log lines (newline-joined) against
// its id. Upsert so a re-save can't error.
func (s *Store) SaveIngestRunLog(ctx context.Context, runID int64, lines []string) error {
	if _, err := s.db.ExecContext(ctx, sqlSaveIngestRunLog,
		runID, strings.Join(lines, "\n")); err != nil {
		return fmt.Errorf("save ingest run log %d: %w", runID, err)
	}
	return nil
}

// InterruptStaleIngestRuns flips any leftover 'running' rows to an error —
// crash recovery for a process that died mid-run. Called once at startup by
// the run manager, before any new run can start, so a stale "running" can
// never be mistaken for a live one.
func (s *Store) InterruptStaleIngestRuns(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, sqlInterruptIngestRuns,
		sqlTime(time.Now()), IngestStatusError, IngestStatusRunning); err != nil {
		return fmt.Errorf("interrupt stale ingest runs: %w", err)
	}
	return nil
}
