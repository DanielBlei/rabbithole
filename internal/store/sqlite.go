// Package store persists seen feed items and digest history in SQLite.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/DanielBlei/ai-searcher/internal/feeds"
)

const schema = `
CREATE TABLE IF NOT EXISTS items (
	id               TEXT PRIMARY KEY,
	source           TEXT NOT NULL,
	title            TEXT NOT NULL,
	link             TEXT NOT NULL UNIQUE,
	summary          TEXT,
	published_at     TIMESTAMP,
	created_at       TIMESTAMP NOT NULL,
	updated_at       TIMESTAMP NOT NULL,
	llm_score        INTEGER,
	llm_score_reason TEXT,
	digested_on      DATE,
	status           TEXT NOT NULL DEFAULT 'unread',
	user_score       INTEGER,
	user_note        TEXT
);
CREATE INDEX IF NOT EXISTS idx_items_digested ON items(digested_on);
`

// Status values for the items.status column. llm_score/llm_score_reason are
// the model's verdict, written by the daily run; status/user_score/user_note
// are yours, written via UpdateUserState.
const (
	StatusUnread  = "unread"
	StatusRead    = "read"
	StatusSkipped = "skipped"
)

const minUserScore, maxUserScore = 0, 10

// ErrItemNotFound is returned by UpdateUserState when no item matches the
// given identifier.
var ErrItemNotFound = errors.New("item not found")

const (
	pragmaWAL         = "PRAGMA journal_mode=WAL"
	pragmaBusyTimeout = "PRAGMA busy_timeout=5000"
)

// Store is a SQLite-backed item store.
type Store struct {
	db *sql.DB
}

// Open opens (or creates) the database at path and runs migrations.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db dir: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if _, err := db.Exec(pragmaWAL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("set WAL mode: %w", err)
	}
	if _, err := db.Exec(pragmaBusyTimeout); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("set busy timeout: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// seenChunkSize caps how many ids go into a single "IN (...)" query, well
// under SQLite's default bound parameter limit.
const seenChunkSize = 500

// Seen returns the set of ids in ids that are already present in the store.
func (s *Store) Seen(ctx context.Context, ids []string) (map[string]bool, error) {
	seen := make(map[string]bool, len(ids))
	for start := 0; start < len(ids); start += seenChunkSize {
		chunk := ids[start:min(start+seenChunkSize, len(ids))]
		if err := s.seenChunk(ctx, chunk, seen); err != nil {
			return nil, err
		}
	}
	return seen, nil
}

func (s *Store) seenChunk(ctx context.Context, ids []string, seen map[string]bool) error {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx, "SELECT id FROM items WHERE id IN ("+placeholders+")", args...)
	if err != nil {
		return fmt.Errorf("query seen: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("scan seen: %w", err)
		}
		seen[id] = true
	}
	return rows.Err()
}

// DigestEntry is an item selected for a digest, with its score.
type DigestEntry struct {
	Item   feeds.Item
	Score  int
	Reason string
}

// Record inserts newly seen items in one transaction. Digested entries are
// written with their score, reason and digest date; the rest are recorded as
// seen-only so they are skipped on future runs. Existing IDs are ignored.
func (s *Store) Record(ctx context.Context, all []feeds.Item, digested []DigestEntry, day time.Time) error {
	scored := make(map[string]DigestEntry, len(digested))
	for _, d := range digested {
		scored[d.Item.ID] = d
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	const q = `INSERT OR IGNORE INTO items
		(id, source, title, link, summary, published_at, created_at, updated_at, llm_score, llm_score_reason, digested_on)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	stmt, err := tx.PrepareContext(ctx, q)
	if err != nil {
		return fmt.Errorf("prepare insert: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	now := time.Now()
	dayStr := day.Format("2006-01-02")
	for _, it := range all {
		var (
			llmScore       any
			llmScoreReason any
			digestDay      any
		)
		if d, ok := scored[it.ID]; ok {
			llmScore = d.Score
			llmScoreReason = d.Reason
			digestDay = dayStr
		}
		var publishedAt any
		if !it.Published.IsZero() {
			publishedAt = it.Published
		}
		if _, err := stmt.ExecContext(ctx, it.ID, it.Source, it.Title, it.Link,
			it.Summary, publishedAt, now, now, llmScore, llmScoreReason, digestDay); err != nil {
			return fmt.Errorf("insert item %s: %w", it.ID, err)
		}
	}
	return tx.Commit()
}

// UserPatch carries optional updates to an item's user-owned fields — as
// opposed to llm_score/llm_score_reason, which are the model's verdict. Nil
// fields are left unchanged. JSON-shaped so a future HTTP handler can decode
// a request body straight into it and call UpdateUserState unchanged.
type UserPatch struct {
	Status    *string
	UserScore *int
	UserNote  *string
}

// isValidStatus reports whether status is one of the recognized items.status
// values. Shared by UpdateUserState and List so the set of valid statuses
// has a single home.
func isValidStatus(status string) bool {
	switch status {
	case StatusUnread, StatusRead, StatusSkipped:
		return true
	default:
		return false
	}
}

// UpdateUserState applies patch to the item identified by identifier, which
// may be either an item's id or its link. It is the single mutation path for
// user-owned state: CLI commands and any future API handler both call it
// directly.
func (s *Store) UpdateUserState(ctx context.Context, identifier string, patch UserPatch) error {
	if patch.Status != nil && !isValidStatus(*patch.Status) {
		return fmt.Errorf("invalid status %q", *patch.Status)
	}
	if patch.UserScore != nil && (*patch.UserScore < minUserScore || *patch.UserScore > maxUserScore) {
		return fmt.Errorf("user score %d out of range %d-%d", *patch.UserScore, minUserScore, maxUserScore)
	}

	sets := []string{"updated_at = ?"}
	args := []any{time.Now()}
	if patch.Status != nil {
		sets = append(sets, "status = ?")
		args = append(args, *patch.Status)
	}
	if patch.UserScore != nil {
		sets = append(sets, "user_score = ?")
		args = append(args, *patch.UserScore)
	}
	if patch.UserNote != nil {
		sets = append(sets, "user_note = ?")
		args = append(args, *patch.UserNote)
	}
	args = append(args, identifier, identifier)

	q := fmt.Sprintf("UPDATE items SET %s WHERE link = ? OR id = ?", strings.Join(sets, ", "))
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("update item %s: %w", identifier, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrItemNotFound, identifier)
	}
	return nil
}

// ItemRow is a compact, read-only view of an item for display (e.g. the
// `items list` CLI command).
type ItemRow struct {
	ID        string
	Source    string
	Title     string
	Link      string
	Status    string
	LLMScore  *int
	UserScore *int
}

// ListFilter narrows List's results. Zero-value fields are unfiltered: an
// empty Status or Source matches anything, and Limit<=0 falls back to
// defaultListLimit.
type ListFilter struct {
	Status string
	Source string
	Limit  int
}

// defaultListLimit caps List's results when ListFilter.Limit is unset.
const defaultListLimit = 50

// List returns items matching filter, ranked best-first: highest of
// user_score/llm_score (whichever is set; user_score wins when both are),
// with source as a tiebreak.
func (s *Store) List(ctx context.Context, filter ListFilter) ([]ItemRow, error) {
	if filter.Status != "" && !isValidStatus(filter.Status) {
		return nil, fmt.Errorf("invalid status %q", filter.Status)
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}

	var where []string
	var args []any
	if filter.Status != "" {
		where = append(where, "status = ?")
		args = append(args, filter.Status)
	}
	if filter.Source != "" {
		where = append(where, "source = ?")
		args = append(args, filter.Source)
	}

	q := "SELECT id, source, title, link, status, llm_score, user_score FROM items"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY COALESCE(user_score, llm_score) DESC, source ASC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query list: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var items []ItemRow
	for rows.Next() {
		var (
			r         ItemRow
			llmScore  sql.NullInt64
			userScore sql.NullInt64
		)
		if err := rows.Scan(&r.ID, &r.Source, &r.Title, &r.Link, &r.Status, &llmScore, &userScore); err != nil {
			return nil, fmt.Errorf("scan list: %w", err)
		}
		if llmScore.Valid {
			v := int(llmScore.Int64)
			r.LLMScore = &v
		}
		if userScore.Valid {
			v := int(userScore.Int64)
			r.UserScore = &v
		}
		items = append(items, r)
	}
	return items, rows.Err()
}

// SourceCount pairs a source name with how many items are recorded for it.
type SourceCount struct {
	Source string
	Count  int
}

// Sources returns the distinct sources present in the store, each with its
// item count, ordered by source name. This is the domain of values that
// ListFilter.Source can match against.
func (s *Store) Sources(ctx context.Context) ([]SourceCount, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT source, COUNT(*) FROM items GROUP BY source ORDER BY source")
	if err != nil {
		return nil, fmt.Errorf("query sources: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var counts []SourceCount
	for rows.Next() {
		var c SourceCount
		if err := rows.Scan(&c.Source, &c.Count); err != nil {
			return nil, fmt.Errorf("scan sources: %w", err)
		}
		counts = append(counts, c)
	}
	return counts, rows.Err()
}
