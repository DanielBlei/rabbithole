// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

// Package store persists seen feed items and digest history in SQLite.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/feeds"
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
	llm_profile_id   TEXT,
	llm_profile_name TEXT,
	llm_profile_hash TEXT,
	digested_on      DATE,
	status           TEXT NOT NULL DEFAULT 'unread',
	user_score       INTEGER,
	user_note        TEXT,
	bookmarked       BOOLEAN NOT NULL DEFAULT 0,
	tags             TEXT
);
CREATE INDEX IF NOT EXISTS idx_items_digested ON items(digested_on);
CREATE INDEX IF NOT EXISTS idx_items_created ON items(created_at);
-- matches the itemDate expression the date filter and date sorts use.
CREATE INDEX IF NOT EXISTS idx_items_date ON items(COALESCE(published_at, created_at));
CREATE INDEX IF NOT EXISTS idx_items_bookmarked ON items(bookmarked);
`

// Postgres wants TIMESTAMPTZ over TIMESTAMP, a real FALSE for the boolean
// default, and its own parenthesis rule for an expression index.
const schemaPG = `
CREATE TABLE IF NOT EXISTS items (
	id               TEXT PRIMARY KEY,
	source           TEXT NOT NULL,
	title            TEXT NOT NULL,
	link             TEXT NOT NULL UNIQUE,
	summary          TEXT,
	published_at     TIMESTAMPTZ,
	created_at       TIMESTAMPTZ NOT NULL,
	updated_at       TIMESTAMPTZ NOT NULL,
	llm_score        INTEGER,
	llm_score_reason TEXT,
	llm_score_model  TEXT,
	llm_profile_id   TEXT,
	llm_profile_name TEXT,
	llm_profile_hash TEXT,
	digested_on      DATE,
	status           TEXT NOT NULL DEFAULT 'unread',
	user_score       INTEGER,
	user_note        TEXT,
	bookmarked       BOOLEAN NOT NULL DEFAULT FALSE,
	tags             TEXT
);
CREATE INDEX IF NOT EXISTS idx_items_digested ON items(digested_on);
CREATE INDEX IF NOT EXISTS idx_items_created ON items(created_at);
-- matches the itemDate expression the date filter and date sorts use.
CREATE INDEX IF NOT EXISTS idx_items_date ON items ((COALESCE(published_at, created_at)));
CREATE INDEX IF NOT EXISTS idx_items_bookmarked ON items(bookmarked);
`

// schemaVersionPG records what SQLite keeps in PRAGMA user_version, which
// Postgres has no equivalent for. The number is the same on both.
const schemaVersionPG = `
CREATE TABLE IF NOT EXISTS schema_version (
	id      INTEGER PRIMARY KEY CHECK (id = 1),
	version INTEGER NOT NULL
);
`

// schemaVersion stamps the database via PRAGMA user_version. Version 2 moved
// configured feeds into SQLite; version 3 added feeds.type; version 4 adds
// interest profiles and score provenance. Version 3 upgrades in place.
const schemaVersion = 4

// Each engine's DDL is listed in its dialect: schemas() for a new database, and
// additiveTables() for tables added since schemaVersion last moved, which are
// created only where they are missing so an existing database gains them without
// being recreated. The existence check comes first rather than leaning on
// IF NOT EXISTS, because Postgres checks a role's rights on the schema before it
// looks at whether the table is already there: an unconditional pass would
// require CREATE on every open, which rules out a DML-only runtime role — the way
// a hosted database is normally set up. Only new tables that nothing older
// depends on belong in the additive list; changing an existing table still means
// a schemaVersion bump.

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
// by itemDate; SortByOldest ranks oldest-first by itemDate.
const (
	SortByScore  = "score"
	SortByLatest = "latest"
	SortByOldest = "oldest"
)

// unscoredSentinel stands in for a NULL score in ORDER BY so result order
// doesn't depend on the SQL engine's NULL-ordering default: SQLite sorts NULL
// smallest, Postgres puts it first under DESC. The value sits below the valid
// 0-10 score range, so unscored items sort last under SortByScore either way.
// It is spliced into the SQL rather than bound, since Postgres cannot infer a
// parameter's type inside COALESCE in an ORDER BY.
const unscoredSentinel = -1

// The score scale, both ends included: what the model returns, what a user
// rating may be set to, and what the list filter's bounds are checked against.
const minScore, maxScore = 0, 10

// ErrItemNotFound is returned by UpdateUserState when no item matches the
// given identifier.
var ErrItemNotFound = errors.New("item not found")

// ErrSchemaVersion is returned by Open when there is a database schema version missmatch
var ErrSchemaVersion = errors.New("incompatible database schema")

// ErrInvalidFilter is returned by List when a ListFilter holds an invalid
// value (an unrecognized status or sort mode). It wraps a more specific
// message; callers can errors.Is against it to tell a caller error (e.g. an
// HTTP 400) apart from an execution failure (HTTP 500).
var ErrInvalidFilter = errors.New("invalid list filter")

// dsnPragmas run on every pooled connection (modernc.org/sqlite applies each
// _pragma at connection open), so per-connection settings hold across the pool.
var dsnPragmas = []string{
	"journal_mode(WAL)",
	"busy_timeout(5000)",
	"foreign_keys(1)",          // enforce REFERENCES / ON DELETE CASCADE
	"auto_vacuum(incremental)", // reclaim space; only takes on a fresh DB
}

// dsn builds the sqlite connection string, carrying the pragmas as _pragma
// query params.
func dsn(path string) string {
	q := url.Values{}
	for _, p := range dsnPragmas {
		q.Add("_pragma", p)
	}
	return "file:" + path + "?" + q.Encode()
}

// sqlTimeLayout is RFC3339 in UTC with fixed-width nanoseconds. Fixed width is
// the point: SQLite compares these as text, and time.RFC3339Nano trims trailing
// zeros, which would sort ".050" after ".1".
const sqlTimeLayout = "2006-01-02T15:04:05.000000000Z"

// Every time value binds through sqlTime — a raw time.Time renders in a layout
// that compares wrong.
func sqlTime(t time.Time) string { return t.UTC().Format(sqlTimeLayout) }

// itemDate is an item's own publication date, falling back to when we first
// saw it for feeds that publish no date. What the date filter and the
// latest/oldest sorts run on, and what the UI shows on the row.
const itemDate = "COALESCE(published_at, created_at)"

// sqlTimeOrNull is sqlTime for a nullable column; a zero time stores NULL.
func sqlTimeOrNull(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return sqlTime(t)
}

// Store is an item store. d spells the queries for whichever engine db holds.
// writer is the single-writer guard, held on Postgres by `serve` and nil
// everywhere else.
type Store struct {
	db     *sql.DB
	d      dialect
	writer *singleWriter
}

// OpenFrom opens whichever engine the config names: a db_path means SQLite, a
// url means Postgres. config.Load has already rejected setting both or neither.
// It places no claim on the store, so any number of processes may hold one —
// which is what lets a CLI command run while a server is up.
func OpenFrom(ctx context.Context, cfg config.StoreConfig) (*Store, error) {
	return openFrom(ctx, cfg, false)
}

// OpenExclusive is OpenFrom plus the single-writer guard, for the long-lived
// process that owns the store. On Postgres it takes an advisory lock held for the
// life of the Store and refuses to open while another server holds it, rather
// than starting and reconciling that server's running ingest away. SQLite has no
// equivalent to lock, so the rule there stays the convention docs/store.md
// describes.
func OpenExclusive(ctx context.Context, cfg config.StoreConfig) (*Store, error) {
	return openFrom(ctx, cfg, true)
}

func openFrom(ctx context.Context, cfg config.StoreConfig, exclusive bool) (*Store, error) {
	if !cfg.IsPostgres() {
		return Open(cfg.DBPath)
	}
	pg, err := cfg.ResolvePostgres()
	if err != nil {
		return nil, err
	}
	opts := pgOptions{
		maxConns:        cfg.MaxConns,
		connMaxLifetime: cfg.ConnMaxLifetime.Std(),
		exclusive:       exclusive,
	}
	return openPostgres(ctx, pg.DSN, pg.Label, opts)
}

// Open opens the SQLite database at path, creating it when it does not exist yet.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db dir: %w", err)
		}
	}
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	d := sqliteDialect{}
	if err := initSchema(context.Background(), db, d, path); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db, d: d}, nil
}

// initSchema creates every table on a new database and stamps it with schemaVersion.
// An existing database is checked against that version and rejected on a mismatch.
// Either way, the additive tables are then created where they are missing. label
// names the database in errors, and must not carry a password.
func initSchema(ctx context.Context, db *sql.DB, d dialect, label string) error {
	fresh, err := d.isFresh(ctx, db)
	if err != nil {
		return err
	}
	if !fresh {
		version, err := d.readVersion(ctx, db)
		if err != nil {
			return err
		}
		switch version {
		case schemaVersion:
		case 3:
			if err := d.migrateV3(ctx, db); err != nil {
				return fmt.Errorf("migrate database %s from version 3 to 4: %w", label, err)
			}
		default:
			return fmt.Errorf("%w: %s is version %d, this build expects %d — delete it and run ingest again",
				ErrSchemaVersion, label, version, schemaVersion)
		}
	} else {
		for _, stmt := range d.schemas() {
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("create schema: %w", err)
			}
		}
		if err := d.stampVersion(ctx, db, schemaVersion); err != nil {
			return err
		}
	}
	for _, t := range d.additiveTables() {
		exists, err := d.tableExists(ctx, db, t.table)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		if _, err := db.ExecContext(ctx, t.ddl); err != nil {
			return fmt.Errorf("create additive table %s: %w", t.table, err)
		}
	}
	return nil
}

// Close releases the database handle, giving up the single-writer guard first
// while the connection carrying it can still be reached.
func (s *Store) Close() error {
	releaseWriter(s.writer)
	return s.db.Close()
}

// Ping reports whether the database is still reachable, for the readiness check.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// linkChunkSize caps how many links go into a single "IN (...)" query, well
// under SQLite's default bound parameter limit.
const linkChunkSize = 500

// ScoredLinks returns the subset of links that already carry an LLM score.
// Dedup keys on link — the canonical UNIQUE column — rather than the derived id,
// which can shift when a feed re-issues a different GUID for the same article.
// Only an already-scored link counts as "done": a link recorded without a score
// is reported absent so the caller re-scores it. This stops re-sending scored
// items to the model while still retrying ones whose scoring never landed.
func (s *Store) ScoredLinks(ctx context.Context, links []string) (map[string]bool, error) {
	scored := make(map[string]bool, len(links))
	for start := 0; start < len(links); start += linkChunkSize {
		chunk := links[start:min(start+linkChunkSize, len(links))]
		if err := s.scoredChunk(ctx, chunk, scored); err != nil {
			return nil, err
		}
	}
	return scored, nil
}

func (s *Store) scoredChunk(ctx context.Context, links []string, scored map[string]bool) error {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(links)), ",")
	args := make([]any, len(links))
	for i, l := range links {
		args[i] = l
	}
	rows, err := s.query(ctx,
		"SELECT link FROM items WHERE llm_score IS NOT NULL AND link IN ("+placeholders+")", args...)
	if err != nil {
		return fmt.Errorf("query scored links: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var link string
		if err := rows.Scan(&link); err != nil {
			return fmt.Errorf("scan scored link: %w", err)
		}
		scored[link] = true
	}
	return rows.Err()
}

// DigestEntry is a scored item. Model and Profile* capture the exact scoring
// provenance so later config or profile edits do not misattribute an older
// score. Digested stamps the entry with the run day (digested_on).
type DigestEntry struct {
	Item        feeds.Item
	Score       int
	Reason      string
	Model       string
	ProfileID   string
	ProfileName string
	ProfileHash string
	Digested    bool
}

// Record writes items in one transaction, keyed on link (the canonical UNIQUE
// column). A link not yet present is inserted; a link already present is updated
// in place with the fresh score — so re-scoring an item whose earlier run left
// it unscored overwrites the placeholder instead of being dropped. Scored
// entries are written with their score, reason and model; those also flagged
// Digested get the digest date too. Items with no scored entry are inserted
// seen-only (NULL score) so they show up in lists and get retried next run.
//
// The conflict update is guarded so it never clobbers a real score with NULL,
// and it touches only the llm_* / digested_on / updated_at columns: a row's
// id, created_at and user-owned status/user_score/user_note are preserved.
func (s *Store) Record(ctx context.Context, all []feeds.Item, scored []DigestEntry, day time.Time) error {
	byID := make(map[string]DigestEntry, len(scored))
	for _, d := range scored {
		byID[d.Item.ID] = d
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	const q = `INSERT INTO items
		(id, source, title, link, summary, published_at, created_at, updated_at, llm_score, llm_score_reason, llm_score_model, llm_profile_id, llm_profile_name, llm_profile_hash, digested_on, tags)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(link) DO UPDATE SET
			llm_score        = excluded.llm_score,
			llm_score_reason = excluded.llm_score_reason,
			llm_score_model  = excluded.llm_score_model,
			llm_profile_id   = excluded.llm_profile_id,
			llm_profile_name = excluded.llm_profile_name,
			llm_profile_hash = excluded.llm_profile_hash,
			digested_on      = COALESCE(excluded.digested_on, items.digested_on),
			updated_at       = excluded.updated_at,
			tags             = excluded.tags
		WHERE excluded.llm_score IS NOT NULL`
	stmt, err := s.prepareTx(ctx, tx, q)
	if err != nil {
		return fmt.Errorf("prepare insert: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	now := sqlTime(time.Now())
	dayStr := day.Format("2006-01-02")
	for _, it := range all {
		var (
			llmScore       any
			llmScoreReason any
			llmScoreModel  any
			profileID      any
			profileName    any
			profileHash    any
			digestDay      any
		)
		if d, ok := byID[it.ID]; ok {
			llmScore = d.Score
			llmScoreReason = d.Reason
			if d.Model != "" {
				llmScoreModel = d.Model
			}
			if d.ProfileID != "" {
				profileID = d.ProfileID
			}
			if d.ProfileName != "" {
				profileName = d.ProfileName
			}
			if d.ProfileHash != "" {
				profileHash = d.ProfileHash
			}
			if d.Digested {
				digestDay = dayStr
			}
		}
		publishedAt := sqlTimeOrNull(it.Published)
		// A feed with no tags stores NULL rather than an empty string, so "untagged" is one value everywhere.
		var tags any
		if joined := strings.Join(it.Tags, ","); joined != "" {
			tags = joined
		}
		if _, err := stmt.ExecContext(ctx, it.ID, it.Source, it.Title, it.Link,
			it.Summary, publishedAt, now, now, llmScore, llmScoreReason, llmScoreModel,
			profileID, profileName, profileHash, digestDay, tags); err != nil {
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
	// ClearUserScore drops the rating back to unrated. A nil UserScore means
	// "leave alone" and 0 is a valid rating, so removing one needs its own field.
	ClearUserScore bool
	UserNote       *string
	Bookmarked     *bool
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
	if patch.UserScore != nil && (*patch.UserScore < minScore || *patch.UserScore > maxScore) {
		return fmt.Errorf("user score %d out of range %d-%d", *patch.UserScore, minScore, maxScore)
	}
	if patch.ClearUserScore && patch.UserScore != nil {
		return errors.New("user score cannot be set and cleared at once")
	}

	sets := []string{"updated_at = ?"}
	args := []any{sqlTime(time.Now())}
	if patch.Status != nil {
		sets = append(sets, "status = ?")
		args = append(args, *patch.Status)
	}
	if patch.UserScore != nil {
		sets = append(sets, "user_score = ?")
		args = append(args, *patch.UserScore)
	}
	if patch.ClearUserScore {
		sets = append(sets, "user_score = NULL")
	}
	if patch.UserNote != nil {
		sets = append(sets, "user_note = ?")
		args = append(args, *patch.UserNote)
	}
	if patch.Bookmarked != nil {
		sets = append(sets, "bookmarked = ?")
		args = append(args, *patch.Bookmarked)
	}
	args = append(args, identifier, identifier)

	q := fmt.Sprintf("UPDATE items SET %s WHERE link = ? OR id = ?", strings.Join(sets, ", "))
	res, err := s.exec(ctx, q, args...)
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
	LLMProfileID   *string
	LLMProfileName *string
	LLMProfileHash *string
	UserScore      *int
	UserNote       *string
	PublishedAt    *time.Time
	Bookmarked     bool
	Tags           []string
}

// itemRowColumns is the SELECT list backing both List and Get, kept in one place
// so the column order stays in lockstep with scanItemRow's destinations.
const itemRowColumns = "id, source, title, link, status, llm_score, llm_score_reason, llm_score_model, llm_profile_id, llm_profile_name, llm_profile_hash, user_score, user_note, published_at, bookmarked, tags"

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
		profileID   sql.NullString
		profileName sql.NullString
		profileHash sql.NullString
		userScore   sql.NullInt64
		userNote    sql.NullString
		publishedAt sql.NullTime
		tags        sql.NullString
	)
	if err := sc.Scan(&r.ID, &r.Source, &r.Title, &r.Link, &r.Status,
		&llmScore, &llmReason, &llmModel, &profileID, &profileName, &profileHash,
		&userScore, &userNote, &publishedAt, &r.Bookmarked, &tags); err != nil {
		return ItemRow{}, err
	}
	if tags.Valid && tags.String != "" {
		r.Tags = strings.Split(tags.String, ",")
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
	if profileID.Valid {
		r.LLMProfileID = &profileID.String
	}
	if profileName.Valid {
		r.LLMProfileName = &profileName.String
	}
	if profileHash.Valid {
		r.LLMProfileHash = &profileHash.String
	}
	if userScore.Valid {
		v := int(userScore.Int64)
		r.UserScore = &v
	}
	if userNote.Valid {
		r.UserNote = &userNote.String
	}
	r.PublishedAt = utcTimePtr(publishedAt)
	return r, nil
}

// Get returns the single item identified by identifier, which may be either an
// item's id or its link (the same lookup UpdateUserState uses). It returns
// ErrItemNotFound when nothing matches.
func (s *Store) Get(ctx context.Context, identifier string) (ItemRow, error) {
	q := "SELECT " + itemRowColumns + " FROM items WHERE link = ? OR id = ?"
	r, err := scanItemRow(s.queryRow(ctx, q, identifier, identifier))
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
// that side of the itemDate window open, a nil MinScore/MaxScore leaves that
// end of the score band open, a false Bookmarked matches anything, an empty
// SortBy falls back to SortByScore, and Limit<=0 falls back to
// defaultListLimit.
//
// Status and Statuses both restrict by items.status; Statuses (an OR-set, via
// SQL IN) takes precedence when non-empty, with Status the single-value
// shorthand. Each value must be a recognized status.
//
// Bookmarked is a one-way filter: true restricts to bookmarked items; false
// (the zero value) means "don't filter on bookmark" — there's no "only
// un-bookmarked" mode, since that's not a view anyone asks for.
//
// After/Before are plain absolute timestamps, not durations — pagination is
// the caller's concern (compute the next window's bounds and call List
// again), not something List tracks via a cursor.
type ListFilter struct {
	Status     string
	Statuses   []string
	Source     string
	After      time.Time
	Before     time.Time
	Bookmarked bool
	SortBy     string
	Limit      int

	// Search keeps items whose title, source or tags contain this text,
	// case-insensitively. Free text, unlike Source's exact match.
	Search string

	// Sources keeps items from any one of these feeds, where Source above is a
	// single exact match. Set both and Sources wins, the way Statuses does.
	Sources []string

	// Tags keeps items carrying any one of these tags. The column holds them
	// comma-joined, so each is matched with its delimiters — "AI" doesn't hit
	// "AIOps".
	Tags []string
	// RatedOnly keeps only items that have a user_score (the user's rating).
	RatedOnly bool
	// ScoredBy keeps only items that were scored by the given model (llm_score_model).
	ScoredBy string

	// MinScore and MaxScore keep items whose llm_score falls inside
	// [MinScore, MaxScore], both ends included. A nil side leaves that end
	// unbounded; both nil is unfiltered.
	//
	// Pointers rather than ints because 0 is a real score, so the zero value
	// can't stand for "unset". An item with no llm_score carries no score
	// rather than a zero, so it is out whenever either bound is set: a band of
	// scores is a question about scored items.
	MinScore *int
	MaxScore *int
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
// sort last. SortByLatest ranks newest-first by itemDate, SortByOldest
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
	for _, bound := range []*int{filter.MinScore, filter.MaxScore} {
		if bound != nil && (*bound < minScore || *bound > maxScore) {
			return fmt.Errorf("%w: score %d out of range %d-%d", ErrInvalidFilter, *bound, minScore, maxScore)
		}
	}
	if filter.MinScore != nil && filter.MaxScore != nil && *filter.MinScore > *filter.MaxScore {
		return fmt.Errorf("%w: score range %d-%d is inverted", ErrInvalidFilter, *filter.MinScore, *filter.MaxScore)
	}
	return nil
}

// whereClause builds the shared WHERE fragments and their args from the
// filter's status/source/time bounds — everything except sort and limit — so
// List and Count restrict rows identically.
func (filter ListFilter) whereClause(d dialect) (where []string, args []any) {
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
	if len(filter.Sources) > 0 {
		placeholders := make([]string, len(filter.Sources))
		for i, src := range filter.Sources {
			placeholders[i] = "?"
			args = append(args, src)
		}
		where = append(where, "source IN ("+strings.Join(placeholders, ", ")+")")
	} else if filter.Source != "" {
		where = append(where, "source = ?")
		args = append(args, filter.Source)
	}
	// OR within the set: an item carrying any of the tags is in. Each side
	// wraps both the column and the tag in commas, so a tag only matches a
	// whole entry in the joined list.
	if len(filter.Tags) > 0 {
		ors := make([]string, len(filter.Tags))
		for i, tag := range filter.Tags {
			ors[i] = d.contains("',' || lower(COALESCE(tags, '')) || ','", "',' || lower(?) || ','")
			args = append(args, tag)
		}
		where = append(where, "("+strings.Join(ors, " OR ")+")")
	}
	if !filter.After.IsZero() {
		where = append(where, itemDate+" >= ?")
		args = append(args, sqlTime(filter.After))
	}
	if !filter.Before.IsZero() {
		where = append(where, itemDate+" < ?")
		args = append(args, sqlTime(filter.Before))
	}
	if filter.Bookmarked {
		where = append(where, "bookmarked = TRUE")
	}
	if filter.RatedOnly {
		where = append(where, "user_score IS NOT NULL")
	}
	if filter.ScoredBy != "" {
		where = append(where, "llm_score_model = ?")
		args = append(args, filter.ScoredBy)
	}
	// Both ends included, so a band names the scores it shows. The NOT NULL is
	// the point rather than a formality: an unscored item isn't a zero, and
	// letting it answer "0 to 3" would fill the low band with everything the
	// model never looked at.
	if filter.MinScore != nil || filter.MaxScore != nil {
		where = append(where, "llm_score IS NOT NULL")
		if filter.MinScore != nil {
			where = append(where, "llm_score >= ?")
			args = append(args, *filter.MinScore)
		}
		if filter.MaxScore != nil {
			where = append(where, "llm_score <= ?")
			args = append(args, *filter.MaxScore)
		}
	}
	// One OR group, so it ANDs with the bounds above. A substring test rather
	// than LIKE '%x%': the text is whatever was typed, and this has no
	// wildcards to escape. tags is NULL when the item carries none.
	if filter.Search != "" {
		where = append(where, "("+d.contains("lower(title)", "lower(?)")+
			" OR "+d.contains("lower(source)", "lower(?)")+
			" OR "+d.contains("lower(COALESCE(tags, ''))", "lower(?)")+")")
		args = append(args, filter.Search, filter.Search, filter.Search)
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
	where, args := filter.whereClause(s.d)
	q := "SELECT COUNT(*) FROM items"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	var n int
	if err := s.queryRow(ctx, q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count items: %w", err)
	}
	return n, nil
}

func (s *Store) List(ctx context.Context, filter ListFilter) ([]ItemRow, error) {
	if err := filter.validate(); err != nil {
		return nil, err
	}
	return s.listItems(ctx, filter, false)
}

// ListAll returns all items matching filter, ignoring Limit and the 200-row cap.
// It uses the same filtering and sorting as List, but returns every matching row.
func (s *Store) ListAll(ctx context.Context, filter ListFilter) ([]ItemRow, error) {
	if err := filter.validate(); err != nil {
		return nil, err
	}
	return s.listItems(ctx, filter, true)
}

// listItems is a shared private implementation of List and ListAll.
func (s *Store) listItems(ctx context.Context, filter ListFilter, all bool) ([]ItemRow, error) {
	limit := filter.Limit
	if !all {
		if limit <= 0 {
			limit = defaultListLimit
		} else if limit > maxListLimit {
			limit = maxListLimit
		}
	}
	where, args := filter.whereClause(s.d)
	q := "SELECT " + itemRowColumns + " FROM items"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	switch filter.SortBy {
	// id breaks ties so a page holds still: items from one ingest run that
	// carry no published date share a created_at down to the second.
	case SortByLatest:
		q += " ORDER BY " + itemDate + " DESC, id DESC"
	case SortByOldest:
		q += " ORDER BY " + itemDate + " ASC, id ASC"
	default:
		// The model's score alone: a user rating is recorded for later use and
		// does not reorder anything yet. Source then id break ties, so a page of
		// equally scored items holds still between renders.
		q += fmt.Sprintf(" ORDER BY COALESCE(llm_score, %d) DESC, source ASC, id ASC", unscoredSentinel)
	}
	if !all {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.query(ctx, q, args...)
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
	rows, err := s.query(ctx, "SELECT source, COUNT(*) FROM items GROUP BY source ORDER BY source")
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

// Tags returns every distinct tag carried by stored items, sorted, case kept as
// recorded. This is the domain of values ListFilter.Tags can match against. The
// column holds them comma-joined, so the splitting happens here: the distinct
// combinations are few (one per feed), which is a much smaller scan than it
// looks.
func (s *Store) Tags(ctx context.Context) ([]string, error) {
	rows, err := s.query(ctx,
		"SELECT DISTINCT tags FROM items WHERE tags IS NOT NULL AND tags != ''")
	if err != nil {
		return nil, fmt.Errorf("query tags: %w", err)
	}
	defer func() { _ = rows.Close() }()

	seen := map[string]string{} // lowercased -> as recorded, so casing can't split a tag in two
	for rows.Next() {
		var joined string
		if err := rows.Scan(&joined); err != nil {
			return nil, fmt.Errorf("scan tags: %w", err)
		}
		for _, tag := range strings.Split(joined, ",") {
			if tag = strings.TrimSpace(tag); tag != "" {
				if _, ok := seen[strings.ToLower(tag)]; !ok {
					seen[strings.ToLower(tag)] = tag
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	tags := make([]string, 0, len(seen))
	for _, tag := range seen {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	return tags, nil
}
