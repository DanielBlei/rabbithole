// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ingestRunFetchSchema links an ingest run to the feed_fetches rows it wrote.
// fetch_id has no foreign key, like every other reference to feed_fetches:
// that table is capped per feed, so a link can outlive its fetch row and is
// then ignored on read and swept by PruneIngestRuns.
const ingestRunFetchSchema = `
CREATE TABLE IF NOT EXISTS ingest_run_fetches (
	run_id   INTEGER NOT NULL REFERENCES ingest_history(id) ON DELETE CASCADE,
	fetch_id INTEGER NOT NULL,
	PRIMARY KEY (run_id, fetch_id)
)`

// run_id is BIGINT to match ingest_history.id.
const ingestRunFetchSchemaPG = `
CREATE TABLE IF NOT EXISTS ingest_run_fetches (
	run_id   BIGINT NOT NULL REFERENCES ingest_history(id) ON DELETE CASCADE,
	fetch_id BIGINT NOT NULL,
	PRIMARY KEY (run_id, fetch_id)
)`

// Retention for PruneIngestRuns when a bound is not given: how many runs keep
// their history row, and how many of the newest keep their log body.
const (
	defaultIngestRunRetention = 100
	defaultIngestLogRetention = 25
)

// RunFeedResult is one feed's outcome within a run, read back from the
// feed_fetches row the run wrote. Status is FeedStatusOK or FeedStatusError.
type RunFeedResult struct {
	FeedID  string
	Name    string
	URL     string
	Status  string
	Error   string
	Items   int
	Elapsed time.Duration
	At      time.Time
}

// OK reports whether the feed fetched successfully in this run.
func (r RunFeedResult) OK() bool { return r.Status == FeedStatusOK }

const (
	sqlInsertRunFetch = "INSERT INTO ingest_run_fetches (run_id, fetch_id) VALUES (?, ?)"

	// Failed feeds first, then by name. The join drops a link whose fetch row
	// was pruned.
	sqlRunFeedResults = `
		SELECT f.feed_id, f.feed_name, f.url, f.status, f.error, f.items, f.elapsed_ms, f.fetched_at
		FROM ingest_run_fetches r JOIN feed_fetches f ON f.id = r.fetch_id
		WHERE r.run_id = ?
		ORDER BY CASE WHEN f.status = '` + FeedStatusOK + `' THEN 1 ELSE 0 END, f.feed_name, f.id`

	// A finished run past the newest keepRuns. A running row is never listed,
	// though it still counts toward the newest.
	sqlPrunedIngestRunIDs = `
		SELECT id FROM ingest_history
		WHERE status <> '` + IngestStatusRunning + `' AND id NOT IN (
			SELECT id FROM ingest_history ORDER BY started_at DESC, id DESC LIMIT ?)`

	sqlPruneRunLinks = `DELETE FROM ingest_run_fetches WHERE run_id IN (` + sqlPrunedIngestRunIDs + `)`
	sqlPruneRunLogs  = `DELETE FROM ingest_run_logs WHERE run_id IN (` + sqlPrunedIngestRunIDs + `)`
	sqlPruneRuns     = `DELETE FROM ingest_history WHERE id IN (` + sqlPrunedIngestRunIDs + `)`

	// Log bodies are the bulk of the data, so they are kept for fewer runs than
	// the history rows are.
	sqlPruneOldLogs = `
		DELETE FROM ingest_run_logs WHERE run_id NOT IN (
			SELECT id FROM ingest_history ORDER BY started_at DESC, id DESC LIMIT ?)`

	sqlPruneDanglingRunLinks = `
		DELETE FROM ingest_run_fetches WHERE NOT EXISTS (
			SELECT 1 FROM feed_fetches f WHERE f.id = ingest_run_fetches.fetch_id)`
)

// RecordRunFetches is RecordFeedFetches plus the run link: it appends one
// history row per fetch outcome and, when runID is positive, one link row each,
// all in a single transaction. A runID of zero or less writes no links.
func (s *Store) RecordRunFetches(ctx context.Context, runID int64, fetches []FeedFetch) error {
	if len(fetches) == 0 {
		return nil
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		for _, f := range fetches {
			var fetchID int64
			if err := s.txQueryRow(ctx, tx, sqlInsertFeedFetch, f.FeedID, f.Name, f.URL, f.status(),
				f.Error, f.Items, f.Elapsed.Milliseconds(), sqlTime(f.At)).Scan(&fetchID); err != nil {
				return fmt.Errorf("insert feed fetch %q: %w", f.Name, err)
			}
			if runID <= 0 {
				continue
			}
			if _, err := s.txExec(ctx, tx, sqlInsertRunFetch, runID, fetchID); err != nil {
				return fmt.Errorf("link feed fetch %q to run %d: %w", f.Name, runID, err)
			}
		}
		return nil
	})
}

// RunFeedResults returns the feeds a run fetched, failed ones first and then by
// name. It is empty for a run with no recorded fetches (the CLI, a rescore, a
// cancelled run, a run whose fetch rows were pruned).
func (s *Store) RunFeedResults(ctx context.Context, runID int64) ([]RunFeedResult, error) {
	rows, err := s.query(ctx, sqlRunFeedResults, runID)
	if err != nil {
		return nil, fmt.Errorf("query run %d feed results: %w", runID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []RunFeedResult
	for rows.Next() {
		var (
			r         RunFeedResult
			elapsedMs int64
		)
		if err := rows.Scan(&r.FeedID, &r.Name, &r.URL, &r.Status, &r.Error,
			&r.Items, &elapsedMs, &r.At); err != nil {
			return nil, fmt.Errorf("scan run feed result: %w", err)
		}
		r.Elapsed = time.Duration(elapsedMs) * time.Millisecond
		r.At = utcTime(r.At)
		out = append(out, r)
	}
	return out, rows.Err()
}

// PruneIngestRuns deletes finished runs past the newest keepRuns, with their
// logs and feed links, then deletes the log bodies past the newest keepLogs,
// then the links whose fetch row no longer exists. A value of zero or less
// means the default. A running row is never deleted. One transaction, so a
// reader never sees a run without its links half way.
func (s *Store) PruneIngestRuns(ctx context.Context, keepRuns, keepLogs int) error {
	if keepRuns <= 0 {
		keepRuns = defaultIngestRunRetention
	}
	if keepLogs <= 0 {
		keepLogs = defaultIngestLogRetention
	}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		// Children first, explicitly: the cascade is a backstop, not the plan.
		if _, err := s.txExec(ctx, tx, sqlPruneRunLinks, keepRuns); err != nil {
			return fmt.Errorf("prune run links: %w", err)
		}
		if _, err := s.txExec(ctx, tx, sqlPruneRunLogs, keepRuns); err != nil {
			return fmt.Errorf("prune run logs: %w", err)
		}
		if _, err := s.txExec(ctx, tx, sqlPruneRuns, keepRuns); err != nil {
			return fmt.Errorf("prune runs: %w", err)
		}
		if _, err := s.txExec(ctx, tx, sqlPruneOldLogs, keepLogs); err != nil {
			return fmt.Errorf("prune old logs: %w", err)
		}
		if _, err := s.txExec(ctx, tx, sqlPruneDanglingRunLinks); err != nil {
			return fmt.Errorf("prune dangling run links: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("prune ingest runs: %w", err)
	}
	return nil
}
