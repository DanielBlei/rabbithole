// Package store persists seen feed items and digest history in SQLite.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/DanielBlei/ai-searcher/internal/feeds"
)

const schema = `
CREATE TABLE IF NOT EXISTS items (
	id          TEXT PRIMARY KEY,
	source      TEXT NOT NULL,
	title       TEXT NOT NULL,
	link        TEXT NOT NULL,
	summary     TEXT,
	published   TIMESTAMP,
	first_seen  TIMESTAMP NOT NULL,
	score       INTEGER,
	reason      TEXT,
	digested_on DATE,
	feedback    INTEGER
);
CREATE INDEX IF NOT EXISTS idx_items_digested ON items(digested_on);
`

const pragmaWAL = "PRAGMA journal_mode=WAL"

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
		(id, source, title, link, summary, published, first_seen, score, reason, digested_on)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	stmt, err := tx.PrepareContext(ctx, q)
	if err != nil {
		return fmt.Errorf("prepare insert: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	now := time.Now()
	dayStr := day.Format("2006-01-02")
	for _, it := range all {
		var (
			score     any
			reason    any
			digestDay any
		)
		if d, ok := scored[it.ID]; ok {
			score = d.Score
			reason = d.Reason
			digestDay = dayStr
		}
		var pub any
		if !it.Published.IsZero() {
			pub = it.Published
		}
		if _, err := stmt.ExecContext(ctx, it.ID, it.Source, it.Title, it.Link,
			it.Summary, pub, now, score, reason, digestDay); err != nil {
			return fmt.Errorf("insert item %s: %w", it.ID, err)
		}
	}
	return tx.Commit()
}
