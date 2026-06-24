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
	llm_score_model  TEXT,
	digested_on      DATE,
	status           TEXT NOT NULL DEFAULT 'unread',
	user_score       INTEGER,
	user_note        TEXT
);
CREATE INDEX IF NOT EXISTS idx_items_digested ON items(digested_on);
CREATE INDEX IF NOT EXISTS idx_items_created ON items(created_at);
`

// Status values for the items.status column. llm_score/llm_score_reason are
// the model's verdict, written by the daily run; status/user_score/user_note
// are yours, written via UpdateUserState.
const (
	StatusUnread  = "unread"
	StatusRead    = "read"
	StatusSkipped = "skipped"
)

// Sort values for ListFilter.SortBy: SortByScore (the default for an empty
// SortBy) ranks best-first by user/llm score; SortByLatest ranks newest-first
// by created_at; SortByOldest ranks oldest-first by created_at.
const (
	SortByScore  = "score"
	SortByLatest = "latest"
	SortByOldest = "oldest"
)

// unscoredSentinel stands in for a NULL score in ORDER BY so result order
// doesn't depend on the SQL engine's NULL-ordering default. SQLite (the only
// engine today) always sorts NULL smallest, so this is belt-and-suspenders
// here; it matters only if we ever add Postgres, whose default flips to NULLS
// FIRST under DESC. The value sits below the valid 0-10 score range, so
// unscored items sort last under SortByScore regardless.
const unscoredSentinel = -1

const minUserScore, maxUserScore = 0, 10

// ErrItemNotFound is returned by UpdateUserState when no item matches the
// given identifier.
var ErrItemNotFound = errors.New("item not found")

// ErrInvalidFilter is returned by List when a ListFilter holds an invalid
// value (an unrecognized status or sort mode). It wraps a more specific
// message; callers can errors.Is against it to tell a caller error (e.g. an
// HTTP 400) apart from an execution failure (HTTP 500).
var ErrInvalidFilter = errors.New("invalid list filter")

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
	for _, stmt := range addColumns {
		// Additive migrations for databases created before a column existed.
		// CREATE TABLE IF NOT EXISTS above leaves an existing table untouched,
		// so these backfill new columns. Idempotent: a "duplicate column"
		// error (column already present, e.g. on a fresh DB) is expected.
		if _, err := db.Exec(stmt); err != nil && !isDuplicateColumn(err) {
			_ = db.Close()
			return nil, fmt.Errorf("migrate schema: %w", err)
		}
	}
	return &Store{db: db}, nil
}

// addColumns holds additive column migrations applied after the base schema.
// Each must be safe to re-run; see Open for how duplicate-column errors are
// tolerated.
var addColumns = []string{
	"ALTER TABLE items ADD COLUMN llm_score_model TEXT",
}

func isDuplicateColumn(err error) bool {
	return err != nil && strings.Contains(err.Error(), "duplicate column name")
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

// DigestEntry is an item selected for a digest, with its score. Model names the
// LLM that produced Score/Reason, captured at digest time so a later config
// change doesn't misattribute an older score.
type DigestEntry struct {
	Item   feeds.Item
	Score  int
	Reason string
	Model  string
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
		(id, source, title, link, summary, published_at, created_at, updated_at, llm_score, llm_score_reason, llm_score_model, digested_on)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
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
			llmScoreModel  any
			digestDay      any
		)
		if d, ok := scored[it.ID]; ok {
			llmScore = d.Score
			llmScoreReason = d.Reason
			if d.Model != "" {
				llmScoreModel = d.Model
			}
			digestDay = dayStr
		}
		var publishedAt any
		if !it.Published.IsZero() {
			publishedAt = it.Published
		}
		if _, err := stmt.ExecContext(ctx, it.ID, it.Source, it.Title, it.Link,
			it.Summary, publishedAt, now, now, llmScore, llmScoreReason, llmScoreModel, digestDay); err != nil {
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
	ID             string
	Source         string
	Title          string
	Link           string
	Status         string
	LLMScore       *int
	LLMScoreReason *string
	LLMScoreModel  *string
	UserScore      *int
	UserNote       *string
	PublishedAt    *time.Time
}

// itemRowColumns is the SELECT list backing both List and Get, kept in one place
// so the column order stays in lockstep with scanItemRow's destinations.
const itemRowColumns = "id, source, title, link, status, llm_score, llm_score_reason, llm_score_model, user_score, user_note, published_at"

// rowScanner is satisfied by both *sql.Row (Get) and *sql.Rows (List), letting
// scanItemRow serve the single-row and multi-row reads from one mapping.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanItemRow maps one row of itemRowColumns onto an ItemRow, threading the
// nullable columns through the sql.Null* wrappers. Column order here must match
// itemRowColumns exactly.
func scanItemRow(sc rowScanner) (ItemRow, error) {
	var (
		r           ItemRow
		llmScore    sql.NullInt64
		llmReason   sql.NullString
		llmModel    sql.NullString
		userScore   sql.NullInt64
		userNote    sql.NullString
		publishedAt sql.NullTime
	)
	if err := sc.Scan(&r.ID, &r.Source, &r.Title, &r.Link, &r.Status,
		&llmScore, &llmReason, &llmModel, &userScore, &userNote, &publishedAt); err != nil {
		return ItemRow{}, err
	}
	if llmScore.Valid {
		v := int(llmScore.Int64)
		r.LLMScore = &v
	}
	if llmReason.Valid {
		r.LLMScoreReason = &llmReason.String
	}
	if llmModel.Valid {
		r.LLMScoreModel = &llmModel.String
	}
	if userScore.Valid {
		v := int(userScore.Int64)
		r.UserScore = &v
	}
	if userNote.Valid {
		r.UserNote = &userNote.String
	}
	if publishedAt.Valid {
		r.PublishedAt = &publishedAt.Time
	}
	return r, nil
}

// Get returns the single item identified by identifier, which may be either an
// item's id or its link (the same lookup UpdateUserState uses). It returns
// ErrItemNotFound when nothing matches.
func (s *Store) Get(ctx context.Context, identifier string) (ItemRow, error) {
	q := "SELECT " + itemRowColumns + " FROM items WHERE link = ? OR id = ?"
	r, err := scanItemRow(s.db.QueryRowContext(ctx, q, identifier, identifier))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ItemRow{}, fmt.Errorf("%w: %s", ErrItemNotFound, identifier)
		}
		return ItemRow{}, fmt.Errorf("get item %s: %w", identifier, err)
	}
	return r, nil
}

// ListFilter narrows List's results. Zero-value fields are unfiltered: an
// empty Status/Statuses or Source matches anything, a zero After/Before leaves
// that side of the created_at window open, an empty SortBy falls back to
// SortByScore, and Limit<=0 falls back to defaultListLimit.
//
// Status and Statuses both restrict by items.status; Statuses (an OR-set, via
// SQL IN) takes precedence when non-empty, with Status the single-value
// shorthand. Each value must be a recognized status.
//
// After/Before are plain absolute timestamps, not durations — pagination is
// the caller's concern (compute the next window's bounds and call List
// again), not something List tracks via a cursor.
type ListFilter struct {
	Status   string
	Statuses []string
	Source   string
	After    time.Time
	Before   time.Time
	SortBy   string
	Limit    int
}

// List's result-count bounds: defaultListLimit applies when ListFilter.Limit
// is unset (<=0); maxListLimit caps any caller-supplied value so an API client
// can't request an unbounded result set.
const (
	defaultListLimit = 50
	maxListLimit     = 200
)

// isValidSortBy reports whether sortBy is one of the recognized ListFilter
// sort modes, or empty (meaning "use the default").
func isValidSortBy(sortBy string) bool {
	switch sortBy {
	case "", SortByScore, SortByLatest, SortByOldest:
		return true
	default:
		return false
	}
}

// List returns items matching filter. By default (SortByScore) results are
// ranked best-first: highest of user_score/llm_score (whichever is set;
// user_score wins when both are), with source as a tiebreak; unscored items
// sort last. SortByLatest ranks newest-first by created_at, SortByOldest
// oldest-first.
// validate reports whether the filter's status/sort values are recognized,
// returning an ErrInvalidFilter-wrapped error otherwise. Shared by List and
// Count so both reject bad input identically.
func (filter ListFilter) validate() error {
	if filter.Status != "" && !isValidStatus(filter.Status) {
		return fmt.Errorf("%w: status %q", ErrInvalidFilter, filter.Status)
	}
	for _, st := range filter.Statuses {
		if !isValidStatus(st) {
			return fmt.Errorf("%w: status %q", ErrInvalidFilter, st)
		}
	}
	if !isValidSortBy(filter.SortBy) {
		return fmt.Errorf("%w: sort %q", ErrInvalidFilter, filter.SortBy)
	}
	return nil
}

// whereClause builds the shared WHERE fragments and their args from the
// filter's status/source/time bounds — everything except sort and limit — so
// List and Count restrict rows identically.
func (filter ListFilter) whereClause() (where []string, args []any) {
	if len(filter.Statuses) > 0 {
		placeholders := make([]string, len(filter.Statuses))
		for i, st := range filter.Statuses {
			placeholders[i] = "?"
			args = append(args, st)
		}
		where = append(where, "status IN ("+strings.Join(placeholders, ", ")+")")
	} else if filter.Status != "" {
		where = append(where, "status = ?")
		args = append(args, filter.Status)
	}
	if filter.Source != "" {
		where = append(where, "source = ?")
		args = append(args, filter.Source)
	}
	if !filter.After.IsZero() {
		where = append(where, "created_at >= ?")
		args = append(args, filter.After)
	}
	if !filter.Before.IsZero() {
		where = append(where, "created_at < ?")
		args = append(args, filter.Before)
	}
	return where, args
}

// Count returns how many items match filter's status/source/time bounds,
// ignoring SortBy and Limit. Unlike len(List(...)) it isn't capped by the list
// limit, so it's the right call for a total-pool stat (e.g. "available").
func (s *Store) Count(ctx context.Context, filter ListFilter) (int, error) {
	if err := filter.validate(); err != nil {
		return 0, err
	}
	where, args := filter.whereClause()
	q := "SELECT COUNT(*) FROM items"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	var n int
	if err := s.db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count items: %w", err)
	}
	return n, nil
}

func (s *Store) List(ctx context.Context, filter ListFilter) ([]ItemRow, error) {
	if err := filter.validate(); err != nil {
		return nil, err
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = defaultListLimit
	} else if limit > maxListLimit {
		limit = maxListLimit
	}

	where, args := filter.whereClause()
	q := "SELECT " + itemRowColumns + " FROM items"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	switch filter.SortBy {
	case SortByLatest:
		q += " ORDER BY created_at DESC"
	case SortByOldest:
		q += " ORDER BY created_at ASC"
	default:
		q += " ORDER BY COALESCE(user_score, llm_score, ?) DESC, source ASC"
		args = append(args, unscoredSentinel)
	}
	q += " LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query list: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var items []ItemRow
	for rows.Next() {
		r, err := scanItemRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scan list: %w", err)
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
