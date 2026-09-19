// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

// testPostgresEnv holds a DSN when the suite should run against Postgres as
// well as SQLite. `make test-pg` sets it from scripts/dev-postgres.sh.
const testPostgresEnv = "RABBITHOLE_TEST_POSTGRES"

// testPGSchema is the Postgres schema the suite owns. Created once by TestMain
// and dropped at the end, it is the counterpart to SQLite's t.TempDir(): it
// keeps a test run from colliding with whatever else lives in the database.
const testPGSchema = "rabbithole_test_suite"

// testPGDSN is the DSN with search_path pointed at testPGSchema, empty when
// the suite is running on SQLite alone.
var testPGDSN string

func TestMain(m *testing.M) {
	dsn := os.Getenv(testPostgresEnv)
	if dsn == "" {
		os.Exit(m.Run())
	}
	scoped, cleanup, err := setupTestSchema(dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "postgres setup: %v\n", err)
		os.Exit(1)
	}
	testPGDSN = scoped
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// setupTestSchema gives the run its own Postgres schema and returns a DSN
// scoped to it. search_path travels as a connect parameter rather than a SET,
// which would land on one pooled connection and miss the rest.
func setupTestSchema(dsn string) (string, func(), error) {
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		return "", nil, fmt.Errorf("open: %w", err)
	}
	defer func() { _ = admin.Close() }()

	ctx := context.Background()
	if _, err := admin.ExecContext(ctx, "DROP SCHEMA IF EXISTS "+testPGSchema+" CASCADE"); err != nil {
		return "", nil, fmt.Errorf("drop stale schema: %w", err)
	}
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+testPGSchema); err != nil {
		return "", nil, fmt.Errorf("create schema: %w", err)
	}

	parsed, err := url.Parse(dsn)
	if err != nil {
		return "", nil, fmt.Errorf("parse dsn: %w", err)
	}
	q := parsed.Query()
	q.Set("search_path", testPGSchema)
	parsed.RawQuery = q.Encode()

	cleanup := func() {
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			return
		}
		defer func() { _ = db.Close() }()
		_, _ = db.ExecContext(context.Background(), "DROP SCHEMA IF EXISTS "+testPGSchema+" CASCADE")
	}
	return parsed.String(), cleanup, nil
}

// openTestStore opens a throwaway store, closed on cleanup. Every test and
// benchmark in this package goes through it, so pointing the suite at a second
// engine is one change here rather than one per test.
//
// On Postgres the tables are shared for the whole run and emptied per test,
// which is the same isolation a fresh SQLite file gives but without paying for
// the DDL each time. Safe because no test in this package runs in parallel.
func openTestStore(t testing.TB) *Store {
	t.Helper()
	if testPGDSN != "" {
		db, err := openPostgres(t.Context(), testPGDSN, "test")
		if err != nil {
			t.Fatalf("openPostgres: %v", err)
		}
		truncateAll(t, db)
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// truncateAll empties every table so a test starts from the same blank slate a
// fresh SQLite file gives. RESTART IDENTITY matters: tests assert on generated
// ids, which would otherwise keep climbing across the run.
func truncateAll(t testing.TB, s *Store) {
	t.Helper()
	const tables = "items, todos, ideas, ingest_history, ingest_run_logs, feed_fetches, feeds, " +
		"feed_defaults, profiles, profile_state, profile_imports, auth"
	if _, err := s.db.ExecContext(t.Context(),
		"TRUNCATE "+tables+" RESTART IDENTITY CASCADE"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
}
