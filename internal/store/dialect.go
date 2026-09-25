// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
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

	// tableExists reports whether the database already holds a table, resolved
	// the way that engine resolves an unqualified name. It gates the additive DDL:
	// Postgres checks a role's rights on the schema before it ever notices
	// IF NOT EXISTS, so creating a table that is already there needs CREATE
	// anyway — which would lock a DML-only runtime role out of a database that is
	// fully set up.
	tableExists(ctx context.Context, db *sql.DB, table string) (bool, error)

	// schemas is the DDL for a new database, and additiveTables the tables added
	// since schemaVersion last moved, created only where they are missing. Each
	// entry is executed on its own.
	schemas() []string
	additiveTables() []additive

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

// additive is one table added since schemaVersion last moved: the DDL that
// creates it, and the name to look for before running that DDL. The two are kept
// together so an entry cannot claim to guard a table it does not create, which
// TestAdditiveTablesNameTheirOwnTable checks.
type additive struct {
	table string
	ddl   string
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

func (sqliteDialect) tableExists(ctx context.Context, db *sql.DB, table string) (bool, error) {
	var n int
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&n); err != nil {
		return false, fmt.Errorf("inspect database: %w", err)
	}
	return n > 0, nil
}

func (sqliteDialect) schemas() []string {
	return []string{
		schema, todoSchema, ideaSchema, ingestSchema, ingestLogSchema,
		feedFetchSchema, feedConfigSchema, profileSchema,
	}
}

func (sqliteDialect) additiveTables() []additive {
	return []additive{{table: "auth", ddl: authSchema}}
}

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

func (postgresDialect) tableExists(ctx context.Context, db *sql.DB, table string) (bool, error) {
	return pgTableExists(ctx, db, table)
}

func (postgresDialect) schemas() []string {
	return []string{
		schemaVersionPG, schemaPG, todoSchemaPG, ideaSchemaPG,
		// ingest_run_logs carries a foreign key into ingest_history.
		ingestSchemaPG, ingestLogSchemaPG, feedFetchSchemaPG, feedConfigSchemaPG,
		profileSchemaPG,
	}
}

func (postgresDialect) additiveTables() []additive {
	return []additive{{table: "auth", ddl: authSchemaPG}}
}

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

// Pool defaults for Postgres. SQLite gets none: its concurrency is already shaped
// by the WAL and busy_timeout pragmas in its DSN. A hosted Postgres caps
// connections per database, and a pooler in front of it caps them again, so the
// default stays well inside a free tier's budget; store.max_conns and
// store.conn_max_lifetime are there to go lower, not higher. ConnMaxLifetime
// matters most: poolers recycle server connections underneath us, and a handle
// held forever eventually talks to something that has gone away.
const (
	pgDefaultMaxConns        = 10
	pgDefaultMaxIdleConns    = 5
	pgDefaultConnMaxLifetime = 30 * time.Minute
	pgDefaultConnMaxIdleTime = 5 * time.Minute
)

// pgOptions are the per-open knobs of a Postgres store. Every zero value means
// "take the default", so a caller with no opinion passes the zero struct.
type pgOptions struct {
	maxConns        int
	connMaxLifetime time.Duration
	connMaxIdleTime time.Duration

	// exclusive claims the single-writer guard for the life of the Store.
	// Only `serve` asks for it: one-shot CLI commands have to keep working while
	// a server is up, and it is a second server that does the damage.
	exclusive bool
}

// openPostgres connects to dsn and brings the schema up to date. label names
// the database in errors and must never carry the password.
func openPostgres(ctx context.Context, dsn, label string, opts pgOptions) (*Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	maxConns := opts.maxConns
	if maxConns <= 0 {
		maxConns = pgDefaultMaxConns
	}
	if opts.exclusive {
		// The guard connection is held for the life of the process and is claimed
		// before any DDL, so it is counted apart from the queries: a pool sized to
		// `store.max_conns: 1` would otherwise leave initSchema waiting on a
		// connection that is never given back, and serve would hang at startup.
		maxConns++
	}
	idleConns := min(pgDefaultMaxIdleConns, maxConns)
	connMaxLifetime := orDefault(opts.connMaxLifetime, pgDefaultConnMaxLifetime)
	connMaxIdleTime := orDefault(opts.connMaxIdleTime, pgDefaultConnMaxIdleTime)
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(idleConns)
	db.SetConnMaxLifetime(connMaxLifetime)
	db.SetConnMaxIdleTime(connMaxIdleTime)

	// sql.Open is lazy, so without this the first failure would surface from
	// whichever query happened to run first rather than from opening the store.
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect to postgres at %s: %w", label, err)
	}

	d := postgresDialect{}

	// Claimed before any DDL, so a second server is refused without having
	// touched the database, and only one process is ever creating tables.
	s := &Store{db: db, d: d}
	if opts.exclusive {
		if s.writer, err = claimSingleWriter(ctx, db, label); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	if err := initSchema(ctx, db, d, label); err != nil {
		releaseWriter(s.writer)
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func orDefault(v, fallback time.Duration) time.Duration {
	if v <= 0 {
		return fallback
	}
	return v
}

// ErrStoreInUse is returned when the single-writer guard is already held, which
// means another rabbithole is serving the same store right now.
var ErrStoreInUse = errors.New("another rabbithole is already serving this store")

// singleWriter is the Postgres session advisory lock standing in for the
// single-writer rule, together with the one connection carrying it. A session
// lock belongs to a backend, so it must be taken on a connection held out of the
// pool for the life of the process; on a pooled handle it would be released the
// moment the query that took it returned.
type singleWriter struct {
	conn *sql.Conn
	key  int64
}

// claimSingleWriter refuses a second server instead of letting it flip the
// first one's running ingest to error at startup.
//
// This is a startup guard, not a lease. If something in front of the database
// drops the connection later, the lock leaves with the session and nothing
// re-takes it; what it does guarantee is that two `serve` processes cannot both
// get as far as reconciling ingest runs.
func claimSingleWriter(ctx context.Context, db *sql.DB, label string) (*singleWriter, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("reserve the single-writer guard: %w", err)
	}
	key, err := storeKey(ctx, conn)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	var held bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&held); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("claim the single-writer guard: %w", err)
	}
	if !held {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: %s — machines take turns, see docs/store.md", ErrStoreInUse, label)
	}
	return &singleWriter{conn: conn, key: key}, nil
}

// storeKey names the set of tables this store points at — the database and the
// schema, not the server — so two installs on one Postgres host, or one database
// used by two installs in two schemas, do not block each other. Only processes
// reaching the same tables collide, which is the case the guard is for.
func storeKey(ctx context.Context, conn *sql.Conn) (int64, error) {
	var database, schema string
	if err := conn.QueryRowContext(ctx,
		"SELECT current_database(), current_schema()").Scan(&database, &schema); err != nil {
		return 0, fmt.Errorf("identify the store: %w", err)
	}
	h := fnv.New64a()
	// hash.Hash.Write never returns an error.
	_, _ = h.Write([]byte(database))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(schema))
	return int64(h.Sum64()), nil
}

// releaseWriter gives the guard back. Closing the connection ends the session,
// which drops the lock anyway, so nothing is reported: the caller is on its way
// out and the close error is the one worth hearing.
func releaseWriter(w *singleWriter) {
	if w == nil {
		return
	}
	// context.Background: Close runs once the context that opened the store is gone.
	_, _ = w.conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", w.key)
	_ = w.conn.Close()
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
