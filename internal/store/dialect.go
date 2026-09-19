// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"fmt"
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
