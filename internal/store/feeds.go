package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// feedFetchSchema is the per-feed fetch history — one row per feed per ingest
// run, append-only, so a feed's behaviour can be read backwards ("when did
// this start failing", "has its volume dropped") rather than only as a
// point-in-time verdict.
//
// Keyed on feed_id (derived from the feed's URL, see config.FeedID) rather
// than on the display name, so renaming a feed keeps its history. feed_name is
// stored alongside as the label at the time of the fetch.
const feedFetchSchema = `
CREATE TABLE IF NOT EXISTS feed_fetches (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	feed_id    TEXT NOT NULL,
	feed_name  TEXT NOT NULL DEFAULT '',
	url        TEXT NOT NULL DEFAULT '',
	status     TEXT NOT NULL,
	error      TEXT NOT NULL DEFAULT '',
	items      INTEGER NOT NULL DEFAULT 0,
	elapsed_ms INTEGER NOT NULL DEFAULT 0,
	fetched_at TIMESTAMP NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_feed_fetches_feed ON feed_fetches(feed_id, fetched_at DESC, id DESC);
`

// Feed fetch statuses for feed_fetches.status.
const (
	FeedStatusOK    = "ok"
	FeedStatusError = "error"
)

// defaultFeedFetchRetention is how many fetch rows per feed PruneFeedFetches
// keeps by default. Bounded growth without a config knob: at one ingest run an
// hour this is a couple of weeks of history per feed.
const defaultFeedFetchRetention = 200

// FeedFetch is one feed's fetch outcome, as handed to RecordFeedFetches.
// Error is empty on success; Items is what the fetch returned before any
// age/cap filtering, so the number reflects the feed, not the run's settings.
type FeedFetch struct {
	FeedID  string
	Name    string
	URL     string
	Items   int
	Error   string
	Elapsed time.Duration
	At      time.Time
}

// status maps the outcome onto the stored status value.
func (f FeedFetch) status() string {
	if f.Error != "" {
		return FeedStatusError
	}
	return FeedStatusOK
}

// FeedHealth is a feed's current standing, aggregated from its fetch history:
// the latest attempt, when it last worked, and how many attempts have failed
// since. LastOK is nil for a feed that has never fetched successfully.
// FailStreak counts consecutive failures and is zero when the latest attempt
// succeeded — so "failing since Tuesday" is distinguishable from "flaky".
type FeedHealth struct {
	FeedID     string
	Name       string
	URL        string
	LastFetch  time.Time
	LastOK     *time.Time
	Status     string
	Error      string
	Items      int
	Elapsed    time.Duration
	FailStreak int
	// Recent is the feed's last few attempts, newest first — the data behind
	// the viewer's at-a-glance history strip.
	Recent []FeedAttempt
}

// OK reports whether the latest recorded fetch succeeded.
func (h FeedHealth) OK() bool { return h.Status == FeedStatusOK }

// FeedAttempt is one historical fetch, trimmed to what a history strip needs.
type FeedAttempt struct {
	Status string
	Items  int
	Error  string
	At     time.Time
}

// OK reports whether this attempt succeeded.
func (a FeedAttempt) OK() bool { return a.Status == FeedStatusOK }

const (
	// Only rows whose tags actually differ are written, so a restart with an
	// unchanged feeds file is a no-op rather than a table-wide rewrite. IS NOT
	// is the null-safe comparison, which matters because untagged stores NULL.
	sqlSyncSourceTags = `UPDATE items SET tags = ? WHERE source = ? AND tags IS NOT ?`

	sqlInsertFeedFetch = `INSERT INTO feed_fetches
		(feed_id, feed_name, url, status, error, items, elapsed_ms, fetched_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`

	// The latest attempt per feed, plus the last successful attempt's time and
	// the number of failures since it. One windowed query rather than a query
	// per feed, so the viewer costs a single round trip however many feeds are
	// configured.
	//
	// Two details worth keeping:
	//   * Recency is ordered by the autoincrement id, not fetched_at. id is
	//     insertion order — exactly "which attempt came later" — and an integer
	//     compare, so it can't be tripped up by timestamps stored as text
	//     (where a UTC-offset change breaks lexicographic ordering).
	//   * last_ok selects the row via ROW_NUMBER rather than MAX(fetched_at):
	//     an aggregate loses the column's declared type and comes back to the
	//     driver as a string, which won't scan into a time.
	sqlFeedHealth = `
		WITH latest AS (
			SELECT *, ROW_NUMBER() OVER (PARTITION BY feed_id ORDER BY id DESC) AS rn
			FROM feed_fetches
		),
		last_ok AS (
			SELECT feed_id, id AS ok_id, fetched_at AS ok_at FROM (
				SELECT feed_id, id, fetched_at,
				       ROW_NUMBER() OVER (PARTITION BY feed_id ORDER BY id DESC) AS rn
				FROM feed_fetches WHERE status = '` + FeedStatusOK + `'
			) WHERE rn = 1
		)
		SELECT l.feed_id, l.feed_name, l.url, l.status, l.error, l.items, l.elapsed_ms, l.fetched_at,
		       o.ok_at,
		       (SELECT COUNT(*) FROM feed_fetches f
		         WHERE f.feed_id = l.feed_id AND f.status <> '` + FeedStatusOK + `'
		           AND (o.ok_id IS NULL OR f.id > o.ok_id)) AS fail_streak
		FROM latest l LEFT JOIN last_ok o ON o.feed_id = l.feed_id
		WHERE l.rn = 1`

	// The newest `limit` attempts for every feed in one pass, for the history
	// strips. Ordered so callers get each feed's attempts newest-first.
	sqlRecentFeedAttempts = `
		WITH ranked AS (
			SELECT feed_id, status, items, error, fetched_at,
			       ROW_NUMBER() OVER (PARTITION BY feed_id ORDER BY id DESC) AS rn
			FROM feed_fetches
		)
		SELECT feed_id, status, items, error, fetched_at FROM ranked
		WHERE rn <= ? ORDER BY feed_id, rn`

	// Retention: keep the newest `keep` rows per feed, drop the rest. Rows for
	// feeds no longer configured are left alone — they're bounded by the same
	// per-feed cap, and keeping them means re-adding a feed restores its
	// history rather than starting blank.
	sqlPruneFeedFetches = `
		DELETE FROM feed_fetches WHERE id IN (
			SELECT id FROM (
				SELECT id, ROW_NUMBER() OVER (PARTITION BY feed_id ORDER BY id DESC) AS rn
				FROM feed_fetches
			) WHERE rn > ?
		)`
)

// SyncSourceTags brings recorded items in step with the tags their feed
// carries in the feeds file, keyed by source name. Items are tagged when
// they're inserted, but an item already scored is never re-inserted — so
// without this, retagging a feed (or tagging one for the first time) would
// only ever reach items ingested afterwards.
//
// Sources absent from tags are left alone: a feed dropped from the file keeps
// the tags its items were recorded with rather than silently losing them.
func (s *Store) SyncSourceTags(ctx context.Context, tags map[string][]string) error {
	if len(tags) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, sqlSyncSourceTags)
	if err != nil {
		return fmt.Errorf("prepare tag sync: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for source, list := range tags {
		// An untagged feed stores NULL, matching what the insert path writes.
		var joined any
		if j := strings.Join(list, ","); j != "" {
			joined = j
		}
		if _, err := stmt.ExecContext(ctx, joined, source, joined); err != nil {
			return fmt.Errorf("sync tags for %q: %w", source, err)
		}
	}
	return tx.Commit()
}

// RecordFeedFetches appends one history row per fetch outcome, in a single
// transaction. Callers treat a failure here as non-fatal — history is
// observability, and losing it must never fail an otherwise good ingest run.
func (s *Store) RecordFeedFetches(ctx context.Context, fetches []FeedFetch) error {
	if len(fetches) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, sqlInsertFeedFetch)
	if err != nil {
		return fmt.Errorf("prepare feed fetch insert: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, f := range fetches {
		if _, err := stmt.ExecContext(ctx, f.FeedID, f.Name, f.URL, f.status(),
			f.Error, f.Items, f.Elapsed.Milliseconds(), f.At); err != nil {
			return fmt.Errorf("insert feed fetch %q: %w", f.Name, err)
		}
	}
	return tx.Commit()
}

// FeedHealthByID aggregates every feed's fetch history into its current
// standing, keyed by feed ID. recentLimit caps how many past attempts each
// entry carries in Recent; pass 0 to skip loading them.
//
// A map (rather than a slice) because callers join it onto the configured feed
// list, which owns the ordering.
func (s *Store) FeedHealthByID(ctx context.Context, recentLimit int) (map[string]FeedHealth, error) {
	rows, err := s.db.QueryContext(ctx, sqlFeedHealth)
	if err != nil {
		return nil, fmt.Errorf("query feed health: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]FeedHealth)
	for rows.Next() {
		var (
			h         FeedHealth
			lastOK    sql.NullTime
			elapsedMs int64
		)
		if err := rows.Scan(&h.FeedID, &h.Name, &h.URL, &h.Status, &h.Error,
			&h.Items, &elapsedMs, &h.LastFetch, &lastOK, &h.FailStreak); err != nil {
			return nil, fmt.Errorf("scan feed health: %w", err)
		}
		if lastOK.Valid {
			h.LastOK = &lastOK.Time
		}
		h.Elapsed = time.Duration(elapsedMs) * time.Millisecond
		out[h.FeedID] = h
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if recentLimit <= 0 || len(out) == 0 {
		return out, nil
	}

	recent, err := s.recentFeedAttempts(ctx, recentLimit)
	if err != nil {
		return nil, err
	}
	for id, attempts := range recent {
		if h, ok := out[id]; ok {
			h.Recent = attempts
			out[id] = h
		}
	}
	return out, nil
}

// recentFeedAttempts returns each feed's newest attempts, newest first.
func (s *Store) recentFeedAttempts(ctx context.Context, limit int) (map[string][]FeedAttempt, error) {
	rows, err := s.db.QueryContext(ctx, sqlRecentFeedAttempts, limit)
	if err != nil {
		return nil, fmt.Errorf("query recent feed attempts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string][]FeedAttempt)
	for rows.Next() {
		var (
			id string
			a  FeedAttempt
		)
		if err := rows.Scan(&id, &a.Status, &a.Items, &a.Error, &a.At); err != nil {
			return nil, fmt.Errorf("scan feed attempt: %w", err)
		}
		out[id] = append(out[id], a)
	}
	return out, rows.Err()
}

// PruneFeedFetches trims the history to the newest keep rows per feed. Pass
// keep <= 0 for the default retention. Append-only tables need a ceiling; this
// is it, and it's per-feed so a busy feed can't crowd out a quiet one's
// history.
func (s *Store) PruneFeedFetches(ctx context.Context, keep int) error {
	if keep <= 0 {
		keep = defaultFeedFetchRetention
	}
	if _, err := s.db.ExecContext(ctx, sqlPruneFeedFetches, keep); err != nil {
		return fmt.Errorf("prune feed fetches: %w", err)
	}
	return nil
}

// FailingSince reports when a feed's current failure run began — the time of
// the first failure after its last success — and whether it is failing at all.
func (h FeedHealth) FailingSince() (time.Time, bool) {
	if h.OK() || len(h.Recent) == 0 {
		return time.Time{}, false
	}
	// Recent is newest-first; walk back through the unbroken run of failures.
	since := h.Recent[0].At
	for _, a := range h.Recent {
		if a.OK() {
			break
		}
		since = a.At
	}
	return since, true
}
