// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// dialect.go is the translator: it owns the database handle, and its rebind
// carries '?' as a Go rune. Both checks below skip it. Open and initSchema
// work on a raw *sql.DB before a Store exists, which no pattern here matches.
var exemptFromSeamChecks = map[string]bool{"dialect.go": true}

// The Context suffix is optional on purpose: s.db.Exec(q) bypasses rebinding
// just as thoroughly as s.db.ExecContext(q) does.
//
// Known hole: aliasing the handle first (db := s.db) escapes this, as does
// reaching it from another type. Both are visible in review in a way a bare
// ExecContext call is not, which is what this exists to catch.
var rawAccess = regexp.MustCompile(`\b(?:s\.db|tx)\.(?:Query|QueryRow|Exec|Prepare)(?:Context)?\(`)

// TestNoDirectDatabaseAccess fails when a query bypasses the wrappers in
// dialect.go. A bypassed statement keeps its `?` placeholders and so works on
// SQLite and fails on Postgres, which is exactly the bug a reviewer cannot see.
func TestNoDirectDatabaseAccess(t *testing.T) {
	for _, file := range packageFiles(t) {
		if exemptFromSeamChecks[filepath.Base(file)] {
			continue
		}
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if rawAccess.MatchString(line) {
				t.Errorf("%s:%d reaches the database directly; use s.query/s.queryRow/s.exec or the tx wrappers\n\t%s",
					filepath.Base(file), i+1, strings.TrimSpace(line))
			}
		}
	}
}

// TestNoLiteralQuestionMarkInSQL guards the assumption rebind rests on: every
// `?` in a query is a placeholder, so renumbering them left to right is safe.
// A `?` inside a string literal would break that, and it is how a jsonb column
// would arrive, since `?` is an operator there.
func TestNoLiteralQuestionMarkInSQL(t *testing.T) {
	for _, file := range packageFiles(t) {
		if exemptFromSeamChecks[filepath.Base(file)] {
			continue
		}
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "//") {
				continue
			}
			if questionMarkInsideQuotes(line) {
				t.Errorf("%s:%d has a ? inside a SQL string literal, which rebind would renumber\n\t%s",
					filepath.Base(file), i+1, strings.TrimSpace(line))
			}
		}
	}
}

// questionMarkInsideQuotes reports whether a ? falls between SQL single
// quotes. A line mixing literals and placeholders, as the tag filter does, has
// every ? outside them.
func questionMarkInsideQuotes(line string) bool {
	inString := false
	for _, r := range line {
		switch {
		case r == '\'':
			inString = !inString
		case r == '?' && inString:
			return true
		}
	}
	return false
}

// TestAdditiveTablesNameTheirOwnTable holds the additive DDL to its existence
// check: initSchema skips the create when a table of that name is already there,
// so an entry naming the wrong table would quietly never be created. The DDL is a
// compile-time constant, so both engines are checked whatever this run uses.
func TestAdditiveTablesNameTheirOwnTable(t *testing.T) {
	firstTable := regexp.MustCompile(`CREATE TABLE IF NOT EXISTS (\w+)`)
	for _, d := range []dialect{sqliteDialect{}, postgresDialect{}} {
		for _, a := range d.additiveTables() {
			got := firstTable.FindStringSubmatch(a.ddl)
			if got == nil {
				t.Errorf("%s: additive DDL for %q creates no table\n%s", d.name(), a.table, a.ddl)
				continue
			}
			if got[1] != a.table {
				t.Errorf("%s: additive entry for %q creates %q", d.name(), a.table, got[1])
			}
		}
	}
}

// TestSingleWriterGuardRefusesASecondServer is the one thing about this store
// that cannot be left to convention: a second server reaching the same database
// marks the first one's running ingest as failed at startup, doubles the model
// spend of a run, and halves the login rate limiter. Postgres-only, because
// SQLite has nothing to lock on.
func TestSingleWriterGuardRefusesASecondServer(t *testing.T) {
	if !onPostgres() {
		t.Skip("the guard is a Postgres advisory lock; run the suite under RABBITHOLE_TEST_POSTGRES")
	}
	ctx := t.Context()

	serving, err := openPostgres(ctx, testPGDSN, "test", pgOptions{exclusive: true})
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	t.Cleanup(func() { _ = serving.Close() })

	_, err = openPostgres(ctx, testPGDSN, "test", pgOptions{exclusive: true})
	if !errors.Is(err, ErrStoreInUse) {
		t.Fatalf("second open = %v, want ErrStoreInUse", err)
	}

	// The guard has to be given up, not just noticed: after the server exits —
	// crash included, since the lock dies with its session — the next one starts.
	if err := serving.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	restarted, err := openPostgres(ctx, testPGDSN, "test", pgOptions{exclusive: true})
	if err != nil {
		t.Fatalf("open after the holder closed: %v", err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
}

// TestOpenFromDoesNotClaimTheGuard pins the other half of the rule: a one-shot
// CLI command must still run while a server holds the guard, or `items list`
// would fail on the machine that is serving the UI.
func TestOpenFromDoesNotClaimTheGuard(t *testing.T) {
	if !onPostgres() {
		t.Skip("the guard is a Postgres advisory lock; run the suite under RABBITHOLE_TEST_POSTGRES")
	}
	ctx := t.Context()

	serving, err := openPostgres(ctx, testPGDSN, "test", pgOptions{exclusive: true})
	if err != nil {
		t.Fatalf("serve open: %v", err)
	}
	defer func() { _ = serving.Close() }()

	cli, err := openPostgres(ctx, testPGDSN, "test", pgOptions{})
	if err != nil {
		t.Fatalf("a plain open must not need the guard: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
}

// TestExclusiveOpenKeepsRoomForQueries pins the pool against the guard. The
// guard connection is taken before any DDL and held for the life of the Store,
// so a pool of exactly `store.max_conns` would have nothing left for the queries
// — at 1, serve would hang in initSchema waiting for a connection that is never
// returned. Postgres-only, because SQLite has no pool to run out of.
func TestExclusiveOpenKeepsRoomForQueries(t *testing.T) {
	if !onPostgres() {
		t.Skip("the guard is a Postgres advisory lock; run the suite under RABBITHOLE_TEST_POSTGRES")
	}
	ctx := t.Context()

	serving, err := openPostgres(ctx, testPGDSN, "test", pgOptions{exclusive: true, maxConns: 1})
	if err != nil {
		t.Fatalf("open exclusively with max_conns=1: %v", err)
	}
	t.Cleanup(func() { _ = serving.Close() })

	// Opening already ran the DDL, so this only proves a second connection was
	// available while the guard held one. The deadline is the test: a pool that
	// cannot grow waits rather than failing.
	queryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := serving.Count(queryCtx, ListFilter{}); err != nil {
		t.Fatalf("query while the guard holds a connection: %v", err)
	}
}

// packageFiles lists the package's non-test Go sources.
func packageFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var files []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
			files = append(files, name)
		}
	}
	if len(files) == 0 {
		t.Fatal("no package sources found")
	}
	return files
}

// TestScannedTimesAreUTC guards a split that is invisible until it bites:
// SQLite returns UTC because the stored text ends in Z, while pgx returns the
// process's local zone whatever the server's timezone is set to. The instant
// matches either way, so nothing looks wrong, but callers bucket by day
// (internal/web's startOfDay, the Maze's completed-by-date grouping) and a row
// written at 23:30 UTC lands on a different date depending on the engine and
// the machine that opened it.
func TestScannedTimesAreUTC(t *testing.T) {
	db := openTestStore(t)
	ctx := t.Context()
	// Late enough that any positive offset rolls it into the next day.
	when := time.Date(2026, 9, 19, 23, 30, 0, 0, time.UTC)

	todo, err := db.AddTodo(ctx, "tz", "", nil, nil)
	if err != nil {
		t.Fatalf("AddTodo: %v", err)
	}
	if _, err := db.exec(ctx, "UPDATE todos SET completed_at = ?, done = TRUE WHERE id = ?",
		sqlTime(when), todo.ID); err != nil {
		t.Fatalf("stamp completed_at: %v", err)
	}
	done := true
	got, err := db.ListTodos(ctx, TodoFilter{Done: &done})
	if err != nil {
		t.Fatalf("ListTodos: %v", err)
	}
	completed := *got[0].CompletedAt
	if completed.Location() != time.UTC {
		t.Errorf("CompletedAt location = %v, want UTC on every engine", completed.Location())
	}
	if !completed.Equal(when) {
		t.Errorf("CompletedAt = %s, want %s", completed.Format(time.RFC3339Nano), when.Format(time.RFC3339Nano))
	}
	if y, m, d := completed.Date(); y != 2026 || m != time.September || d != 19 {
		t.Errorf("CompletedAt falls on %04d-%02d-%02d, want 2026-09-19", y, m, d)
	}
}

// TestCompletedTodosSortNullsLast pins the other default the engines disagree
// on: SQLite sorts NULL last under DESC, Postgres sorts it first. A task
// completed before completed_at was stamped would otherwise head the completed
// list on one engine and tail it on the other.
func TestCompletedTodosSortNullsLast(t *testing.T) {
	db := openTestStore(t)
	ctx := t.Context()

	for _, title := range []string{"older", "newer", "undated"} {
		if _, err := db.AddTodo(ctx, title, "", nil, nil); err != nil {
			t.Fatalf("AddTodo(%s): %v", title, err)
		}
	}
	stamp := map[string]any{
		"older":   sqlTime(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)),
		"newer":   sqlTime(time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)),
		"undated": nil,
	}
	for title, at := range stamp {
		if _, err := db.exec(ctx,
			"UPDATE todos SET done = TRUE, completed_at = ? WHERE title = ?", at, title); err != nil {
			t.Fatalf("stamp %s: %v", title, err)
		}
	}

	done := true
	got, err := db.ListTodos(ctx, TodoFilter{Done: &done})
	if err != nil {
		t.Fatalf("ListTodos: %v", err)
	}
	var order []string
	for _, todo := range got {
		order = append(order, todo.Title)
	}
	want := []string{"newer", "older", "undated"}
	if !slices.Equal(order, want) {
		t.Errorf("completed order = %v, want %v", order, want)
	}
}
