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
	link             TEXT NOT NULL,
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
CREATE INDEX IF NOT EXISTS idx_items_link ON items(link);
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
// given link.
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

// Seen returns the set of item IDs already present in the store.
func (s *Store) Seen(ctx context.Context, ids []string) (map[string]bool, error) {
	seen := make(map[string]bool, len(ids))
	if len(ids) == 0 {
		return seen, nil
	}
	rows, err := s.db.QueryContext(ctx, "SELECT id FROM items")
	if err != nil {
		return nil, fmt.Errorf("query seen: %w", err)
	}
	defer func() { _ = rows.Close() }()

	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan seen: %w", err)
		}
		if want[id] {
			seen[id] = true
		}
	}
	return seen, rows.Err()
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

// UpdateUserState applies patch to the item identified by link. It is the
// single mutation path for user-owned state: CLI commands and any future
// API handler both call it directly.
func (s *Store) UpdateUserState(ctx context.Context, link string, patch UserPatch) error {
	if patch.Status != nil {
		switch *patch.Status {
		case StatusUnread, StatusRead, StatusSkipped:
		default:
			return fmt.Errorf("invalid status %q", *patch.Status)
		}
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
	args = append(args, link)

	q := fmt.Sprintf("UPDATE items SET %s WHERE link = ?", strings.Join(sets, ", "))
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("update item %s: %w", link, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrItemNotFound, link)
	}
	return nil
}
