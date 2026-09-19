// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" driver
)

// dialect carries the few things the supported engines do not spell the same
// way. Queries are written once against SQLite's form and translated here, so
// adding an engine means implementing this rather than duplicating the store.
//
// Keep it small. Once the list of differences outgrows a handful, two separate
// implementations are easier to reason about than a seam this wide.
type dialect interface {
	// name identifies the engine in errors and logs.
	name() string

	// rebind converts a query's `?` placeholders to the engine's own form.
	rebind(q string) string

	// contains builds a case-sensitive substring test. Callers lower both
	// sides themselves where the match should ignore case.
	contains(haystack, needle string) string

	// isFresh reports whether the database has yet to be set up. Each engine
	// answers in its own terms, since neither has a portable way to ask.
	isFresh(ctx context.Context, db *sql.DB) (bool, error)

	// schemas is the DDL for a new database, and additiveSchemas the tables
	// added since schemaVersion last moved, created if missing on every open.
	// Each entry is executed on its own.
	schemas() []string
	additiveSchemas() []string

	// uniqueViolation reports which unique index an error came from, and ""
	// when it is not a uniqueness failure. It exists because a check inside a
	// transaction is not a guarantee on either engine: SQLite serializes
	// writers so the check usually holds, while Postgres READ COMMITTED lets
	// two callers past it and fails the loser at commit.
	uniqueViolation(err error) string

	// migrateV3 upgrades a version 3 database in place to schemaVersion. Only
	// SQLite has a version 3 to upgrade: Postgres joined at the current number,
	// so a Postgres database reporting 3 was not written by this build.
	migrateV3(ctx context.Context, db *sql.DB) error

	// readVersion reads the stamped schema version, and stampVersion writes
	// it. SQLite keeps it in PRAGMA user_version, which Postgres has no
	// equivalent for, so the two record the same number in different places.
	readVersion(ctx context.Context, db *sql.DB) (int, error)
	stampVersion(ctx context.Context, db *sql.DB, version int) error
}

// sqliteDialect is the form every query in this package is written in, so its
// translations are identity.
type sqliteDialect struct{}

func (sqliteDialect) name() string { return "sqlite" }

func (sqliteDialect) rebind(q string) string { return q }

func (sqliteDialect) contains(haystack, needle string) string {
	return "instr(" + haystack + ", " + needle + ") > 0"
}

// isFresh asks whether the file holds any table at all, which is what a
// database this package has never touched looks like.
func (sqliteDialect) isFresh(ctx context.Context, db *sql.DB) (bool, error) {
	var tables int
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sqlite_master WHERE type = 'table'").Scan(&tables); err != nil {
		return false, fmt.Errorf("inspect database: %w", err)
	}
	return tables == 0, nil
}

func (sqliteDialect) schemas() []string {
	return []string{
		schema, todoSchema, ideaSchema, ingestSchema, ingestLogSchema,
		feedFetchSchema, feedConfigSchema, profileSchema,
	}
}

func (sqliteDialect) additiveSchemas() []string { return []string{authSchema} }

// migrateV3 adds local profiles and nullable provenance columns. It is one
// transaction so an interrupted upgrade cannot expose a half-migrated store.
func (sqliteDialect) migrateV3(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		"ALTER TABLE items ADD COLUMN llm_profile_id TEXT",
		"ALTER TABLE items ADD COLUMN llm_profile_name TEXT",
		"ALTER TABLE items ADD COLUMN llm_profile_hash TEXT",
		profileSchema,
		fmt.Sprintf("PRAGMA user_version = %d", schemaVersion),
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// uniqueViolation reads the column out of modernc's message, which reads
// "constraint failed: UNIQUE constraint failed: feeds.name (2067)". The
// trailing code is the SQLite result code and is cut off.
func (sqliteDialect) uniqueViolation(err error) string {
	if err == nil {
		return ""
	}
	const marker = "UNIQUE constraint failed: "
	msg := err.Error()
	i := strings.Index(msg, marker)
	if i < 0 {
		return ""
	}
	name := strings.TrimSpace(msg[i+len(marker):])
	if cut := strings.IndexByte(name, ' '); cut >= 0 {
		name = name[:cut]
	}
	return name
}

func (sqliteDialect) readVersion(ctx context.Context, db *sql.DB) (int, error) {
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return version, nil
}

func (sqliteDialect) stampVersion(ctx context.Context, db *sql.DB, version int) error {
	// PRAGMA takes no bound parameters; version is a compile-time constant.
	if _, err := db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		return fmt.Errorf("stamp schema version: %w", err)
	}
	return nil
}

// postgresDialect translates the SQLite form every query is written in.
type postgresDialect struct{}

func (postgresDialect) name() string { return "postgres" }

// rebind numbers placeholders left to right. Every `?` in this package is a
// bound parameter, never a character in a string literal, which
// TestNoLiteralQuestionMarkInSQL enforces.
func (postgresDialect) rebind(q string) string {
	var b strings.Builder
	b.Grow(len(q) + 8)
	n := 0
	for _, r := range q {
		if r == '?' {
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (postgresDialect) contains(haystack, needle string) string {
	return "strpos(" + haystack + ", " + needle + ") > 0"
}

// isFresh asks after items rather than after schema_version, so a database
// holding this application's tables but no version row is refused by the
// version check instead of being stamped over.
func (postgresDialect) isFresh(ctx context.Context, db *sql.DB) (bool, error) {
	exists, err := pgTableExists(ctx, db, "items")
	if err != nil {
		return false, err
	}
	return !exists, nil
}

func (postgresDialect) schemas() []string {
	return []string{
		schemaVersionPG, schemaPG, todoSchemaPG, ideaSchemaPG,
		// ingest_run_logs carries a foreign key into ingest_history.
		ingestSchemaPG, ingestLogSchemaPG, feedFetchSchemaPG, feedConfigSchemaPG,
		profileSchemaPG,
	}
}

func (postgresDialect) additiveSchemas() []string { return []string{authSchemaPG} }

// migrateV3 refuses rather than translating: Postgres arrived with the current
// schema version, so a version 3 database under this engine is a database this
// application never wrote.
func (postgresDialect) migrateV3(context.Context, *sql.DB) error {
	return fmt.Errorf("no Postgres database was ever version 3; a new one is created at version %d",
		schemaVersion)
}

// uniqueViolation matches SQLSTATE 23505 and reports the constraint that
// failed, which is the index name the schema declares.
func (postgresDialect) uniqueViolation(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return pgErr.ConstraintName
	}
	return ""
}

// readVersion treats a database with no schema_version table as version 0,
// which fails the caller's equality check rather than passing silently.
func (postgresDialect) readVersion(ctx context.Context, db *sql.DB) (int, error) {
	exists, err := pgTableExists(ctx, db, "schema_version")
	if err != nil {
		return 0, err
	}
	if !exists {
		return 0, nil
	}
	var version int
	err = db.QueryRowContext(ctx, "SELECT version FROM schema_version WHERE id = 1").Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return version, nil
}

func (postgresDialect) stampVersion(ctx context.Context, db *sql.DB, version int) error {
	if _, err := db.ExecContext(ctx,
		`INSERT INTO schema_version (id, version) VALUES (1, $1)
		 ON CONFLICT (id) DO UPDATE SET version = excluded.version`, version); err != nil {
		return fmt.Errorf("stamp schema version: %w", err)
	}
	return nil
}

// Pool limits for Postgres. SQLite gets none: its concurrency is already shaped
// by the WAL and busy_timeout pragmas in its DSN. A hosted Postgres caps
// connections per database, and a pooler in front of it caps them again, so the
// store stays well inside a free tier's budget. ConnMaxLifetime matters most:
// poolers recycle server connections underneath us, and a handle held forever
// eventually talks to something that has gone away.
const (
	pgMaxOpenConns    = 10
	pgMaxIdleConns    = 5
	pgConnMaxLifetime = 30 * time.Minute
	pgConnMaxIdleTime = 5 * time.Minute
)

// openPostgres connects to dsn and brings the schema up to date. label names
// the database in errors and must never carry the password.
func openPostgres(ctx context.Context, dsn, label string) (*Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	db.SetMaxOpenConns(pgMaxOpenConns)
	db.SetMaxIdleConns(pgMaxIdleConns)
	db.SetConnMaxLifetime(pgConnMaxLifetime)
	db.SetConnMaxIdleTime(pgConnMaxIdleTime)

	// sql.Open is lazy, so without this the first failure would surface from
	// whichever query happened to run first rather than from opening the store.
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect to postgres at %s: %w", label, err)
	}
	d := postgresDialect{}
	if err := initSchema(ctx, db, d, label); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db, d: d}, nil
}

// pgTableExists resolves the name against the connection's search_path, so it
// answers for the schema the store is actually using.
func pgTableExists(ctx context.Context, db *sql.DB, table string) (bool, error) {
	var exists bool
	if err := db.QueryRowContext(ctx, "SELECT to_regclass($1) IS NOT NULL", table).Scan(&exists); err != nil {
		return false, fmt.Errorf("inspect database: %w", err)
	}
	return exists, nil
}

// Timestamps come back in different zones per engine: SQLite yields UTC, since
// the stored text ends in Z, while pgx yields the process's local zone whatever
// the server's timezone is set to. The instant is the same either way, but
// callers bucket by day and format clock times, so a row written at 23:30 UTC
// would land on a different date depending on the engine and the machine.
// Every scanned time is normalized here so both engines agree.
func utcTime(t time.Time) time.Time { return t.UTC() }

// utcTimePtr is utcTime for a nullable column, returning nil for NULL.
func utcTimePtr(n sql.NullTime) *time.Time {
	if !n.Valid {
		return nil
	}
	t := n.Time.UTC()
	return &t
}

// Every statement in this package reaches the database through the wrappers
// below rather than through s.db directly, so no query can miss rebinding.
// A call site that bypasses them works on SQLite and fails on Postgres, which
// is exactly the bug a reviewer cannot see. TestNoDirectDatabaseAccess holds
// the line.

func (s *Store) query(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return s.db.QueryContext(ctx, s.d.rebind(q), args...)
}

func (s *Store) queryRow(ctx context.Context, q string, args ...any) *sql.Row {
	return s.db.QueryRowContext(ctx, s.d.rebind(q), args...)
}

func (s *Store) exec(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return s.db.ExecContext(ctx, s.d.rebind(q), args...)
}

func (s *Store) txQueryRow(ctx context.Context, tx *sql.Tx, q string, args ...any) *sql.Row {
	return tx.QueryRowContext(ctx, s.d.rebind(q), args...)
}

func (s *Store) txExec(ctx context.Context, tx *sql.Tx, q string, args ...any) (sql.Result, error) {
	return tx.ExecContext(ctx, s.d.rebind(q), args...)
}

func (s *Store) prepareTx(ctx context.Context, tx *sql.Tx, q string) (*sql.Stmt, error) {
	return tx.PrepareContext(ctx, s.d.rebind(q))
}
