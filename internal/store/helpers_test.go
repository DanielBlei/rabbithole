// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// testPostgresEnv holds a DSN when the suite should run against Postgres as
// well as SQLite. `make test-pg` sets it from scripts/dev-postgres.sh.
const testPostgresEnv = "RABBITHOLE_TEST_POSTGRES"

// testPGSchemaName is the Postgres schema a single run owns, named per run
// rather than per suite. It is the counterpart to SQLite's t.TempDir(): it keeps
// a run from colliding with whatever else lives in the database. A fixed name
// would have the suite `DROP SCHEMA … CASCADE` a name it did not create, which
// is exactly what two runs against one server — or a leftover from an interrupted
// run — do to each other.
func testPGSchemaName() string {
	var raw [4]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// The pid alone still separates two live runs; the random half only
		// covers two runs that reuse a pid after one of them dies.
		return "rabbithole_test_" + strconv.Itoa(os.Getpid())
	}
	return "rabbithole_test_" + strconv.Itoa(os.Getpid()) + "_" + hex.EncodeToString(raw[:])
}

// testPGDSN is the DSN with search_path pointed at this run's schema, empty when
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

// setupTestSchema gives this run its own Postgres schema and returns a DSN
// scoped to it, along with a cleanup that drops only what this run made. The
// name is quoted rather than interpolated, since it is an identifier and DDL
// takes no bound parameters. search_path travels as a connect parameter rather
// than a SET, which would land on one pooled connection and miss the rest.
func setupTestSchema(dsn string) (string, func(), error) {
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		return "", nil, fmt.Errorf("open: %w", err)
	}
	defer func() { _ = admin.Close() }()

	name := testPGSchemaName()
	quoted := pq(name)

	ctx := context.Background()
	// Nothing should own a name this random, but a run that was killed mid-flight
	// can leave its schema behind, and this is the moment to collect our own.
	if _, err := admin.ExecContext(ctx, "DROP SCHEMA IF EXISTS "+quoted+" CASCADE"); err != nil {
		return "", nil, fmt.Errorf("drop stale schema: %w", err)
	}
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+quoted); err != nil {
		return "", nil, fmt.Errorf("create schema: %w", err)
	}

	parsed, err := url.Parse(dsn)
	if err != nil {
		return "", nil, fmt.Errorf("parse dsn: %w", err)
	}
	q := parsed.Query()
	q.Set("search_path", name)
	parsed.RawQuery = q.Encode()

	cleanup := func() {
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			return
		}
		defer func() { _ = db.Close() }()
		_, _ = db.ExecContext(context.Background(), "DROP SCHEMA IF EXISTS "+quoted+" CASCADE")
	}
	return parsed.String(), cleanup, nil
}

// onPostgres reports whether this run is pointed at Postgres. The behaviour the
// two engines cannot share — the single-writer guard, for one — is pinned here
// rather than skipped silently, so a passing SQLite run cannot be mistaken for
// coverage of it.
func onPostgres() bool { return testPGDSN != "" }

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
		db, err := openPostgres(t.Context(), testPGDSN, "test", pgOptions{})
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
//
// The list is read from the catalog rather than written down, so a table added
// later is cleared too. A hardcoded list would leave the new table's rows
// bleeding between tests on the Postgres run only, with nothing failing.
func truncateAll(t testing.TB, s *Store) {
	t.Helper()
	ctx := t.Context()
	rows, err := s.db.QueryContext(ctx,
		`SELECT tablename FROM pg_tables WHERE schemaname = current_schema()`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		// schema_version is the store's own bookkeeping, not test data;
		// emptying it would make the next open think the database is new.
		if name != "schema_version" {
			tables = append(tables, pq(name))
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list tables: %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if len(tables) == 0 {
		return
	}
	if _, err := s.db.ExecContext(ctx,
		"TRUNCATE "+strings.Join(tables, ", ")+" RESTART IDENTITY CASCADE"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

// pq quotes an identifier read back from the catalog, so a table name is never
// pasted into SQL raw.
func pq(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }
