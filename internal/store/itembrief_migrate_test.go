// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/feeds"
)

// legacyItemBriefSchema is the item_briefs DDL exactly as published at
// 417e76f: every column present, no foreign key. The migration test replaces
// the current table with this definition rather than merely dropping one
// column, so the fixture matches the published schema.
const legacyItemBriefSchema = `
CREATE TABLE IF NOT EXISTS item_briefs (
	item_id         TEXT PRIMARY KEY,
	status          TEXT NOT NULL,
	error_code      TEXT NOT NULL DEFAULT '',
	error_detail    TEXT NOT NULL DEFAULT '',
	score           INTEGER,
	recommendation  TEXT NOT NULL DEFAULT '',
	why             TEXT NOT NULL DEFAULT '',
	key_points      TEXT NOT NULL DEFAULT '[]',
	novelty         TEXT NOT NULL DEFAULT '',
	caveats         TEXT NOT NULL DEFAULT '',
	focus           TEXT NOT NULL DEFAULT '',
	reading_minutes INTEGER NOT NULL DEFAULT 0,
	word_count      INTEGER NOT NULL DEFAULT 0,
	truncated       BOOLEAN NOT NULL DEFAULT 0,
	content_hash    TEXT NOT NULL DEFAULT '',
	final_url       TEXT NOT NULL DEFAULT '',
	feed_score      INTEGER,
	profile_id      TEXT NOT NULL DEFAULT '',
	profile_name    TEXT NOT NULL DEFAULT '',
	profile_hash    TEXT NOT NULL DEFAULT '',
	provider        TEXT NOT NULL DEFAULT '',
	model           TEXT NOT NULL DEFAULT '',
	created_at      TIMESTAMP NOT NULL,
	updated_at      TIMESTAMP NOT NULL
);
`

var errItemBriefMigrationInjected = errors.New("injected item briefs migration failure")

// migrationTarget owns one database that can be closed and reopened, on either
// engine. SQLite gets a file path; Postgres gets the suite's schema.
type migrationTarget struct {
	store *Store
	path  string
}

func newMigrationTarget(t *testing.T) *migrationTarget {
	t.Helper()
	if onPostgres() {
		return &migrationTarget{store: openTestStore(t)}
	}
	path := filepath.Join(t.TempDir(), "legacy.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return &migrationTarget{store: s, path: path}
}

func (m *migrationTarget) open(t *testing.T) (*Store, error) {
	t.Helper()
	if onPostgres() {
		s, err := openPostgres(t.Context(), testPGDSN, "test", pgOptions{})
		if err == nil {
			t.Cleanup(func() { _ = s.Close() })
		}
		return s, err
	}
	s, err := Open(m.path)
	if err == nil {
		t.Cleanup(func() { _ = s.Close() })
	}
	return s, err
}

// raw opens the same database without running initSchema, for inspecting the
// table exactly as a failed migration left it.
func (m *migrationTarget) raw(t *testing.T) (*sql.DB, dialect) {
	t.Helper()
	if onPostgres() {
		db, err := sql.Open("pgx", testPGDSN)
		if err != nil {
			t.Fatalf("open raw postgres: %v", err)
		}
		return db, postgresDialect{}
	}
	db, err := sql.Open("sqlite", dsn(m.path))
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	return db, sqliteDialect{}
}

// makeLegacyItemBriefs turns a fresh item_briefs table into the published
// 417e76f shape: same columns, no foreign key.
func makeLegacyItemBriefs(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	switch s.d.name() {
	case "sqlite":
		for _, stmt := range []string{"DROP TABLE item_briefs", legacyItemBriefSchema} {
			if _, err := s.db.ExecContext(ctx, stmt); err != nil {
				t.Fatalf("legacy sqlite schema: %v", err)
			}
		}
	case "postgres":
		var name string
		err := s.db.QueryRowContext(ctx, `
			SELECT c.conname
			FROM pg_constraint c
			JOIN pg_class t ON t.oid = c.conrelid
			JOIN pg_namespace n ON n.oid = t.relnamespace
			WHERE n.nspname = current_schema()
			  AND t.relname = 'item_briefs'
			  AND c.contype = 'f'`).Scan(&name)
		switch {
		case err == nil:
			if _, err := s.db.ExecContext(ctx, "ALTER TABLE item_briefs DROP CONSTRAINT "+pq(name)); err != nil {
				t.Fatalf("drop legacy postgres foreign key: %v", err)
			}
		case errors.Is(err, sql.ErrNoRows):
		default:
			t.Fatalf("find postgres foreign key: %v", err)
		}
	default:
		t.Fatalf("unsupported engine %q", s.d.name())
	}
	has, err := itemBriefsHaveForeignKey(ctx, s.db, s.d)
	if err != nil {
		t.Fatalf("inspect legacy fixture: %v", err)
	}
	if has {
		t.Fatal("legacy fixture still has the item_briefs foreign key")
	}
}

// seedMigratableBriefs writes two valid briefs and returns their stored form,
// including timestamps, so the post-migration comparison covers every column.
func seedMigratableBriefs(t *testing.T, s *Store) (ItemBrief, ItemBrief) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	if err := s.Record(ctx, []feeds.Item{
		{ID: "a", Source: "S", Title: "A", Link: "https://x/a", Published: now},
		{ID: "keep", Source: "S", Title: "Keep", Link: "https://x/keep", Published: now},
	}, nil, now); err != nil {
		t.Fatalf("Record: %v", err)
	}
	at := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	for _, id := range []string{"a", "keep"} {
		b := briefFixture(id)
		b.CreatedAt = at
		if err := s.SaveItemBrief(ctx, b); err != nil {
			t.Fatalf("SaveItemBrief %s: %v", id, err)
		}
	}
	storedA, err := s.ItemBrief(ctx, "a")
	if err != nil {
		t.Fatalf("ItemBrief a: %v", err)
	}
	storedKeep, err := s.ItemBrief(ctx, "keep")
	if err != nil {
		t.Fatalf("ItemBrief keep: %v", err)
	}
	return storedA, storedKeep
}

func insertOrphanBrief(t *testing.T, s *Store, id string) {
	t.Helper()
	now := sqlTime(time.Now().UTC())
	_, err := s.db.ExecContext(context.Background(),
		s.d.rebind(`INSERT INTO item_briefs (item_id, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?)`), id, ItemBriefReady, now, now)
	if err != nil {
		t.Fatalf("insert orphan brief: %v", err)
	}
}

func scanBriefRaw(ctx context.Context, db *sql.DB, d dialect, id string) (ItemBrief, error) {
	row := db.QueryRowContext(ctx,
		d.rebind(`SELECT `+itemBriefColumns+` FROM item_briefs WHERE item_id = ?`), id)
	return scanItemBrief(row)
}

func optionalIntEqual(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func assertItemBriefEqual(t *testing.T, got, want ItemBrief) {
	t.Helper()
	if got.ItemID != want.ItemID || got.Status != want.Status ||
		got.ErrorCode != want.ErrorCode || got.ErrorDetail != want.ErrorDetail ||
		got.Recommendation != want.Recommendation || got.Why != want.Why ||
		got.Novelty != want.Novelty || got.Caveats != want.Caveats || got.Focus != want.Focus ||
		got.ReadingMinutes != want.ReadingMinutes || got.WordCount != want.WordCount ||
		got.Truncated != want.Truncated || got.ContentHash != want.ContentHash ||
		got.FinalURL != want.FinalURL || got.ProfileID != want.ProfileID ||
		got.ProfileName != want.ProfileName || got.ProfileHash != want.ProfileHash ||
		got.Provider != want.Provider || got.Model != want.Model ||
		!slices.Equal(got.KeyPoints, want.KeyPoints) ||
		!optionalIntEqual(got.Score, want.Score) ||
		!optionalIntEqual(got.FeedScore, want.FeedScore) ||
		!got.CreatedAt.Equal(want.CreatedAt) ||
		!got.UpdatedAt.Equal(want.UpdatedAt) {
		t.Fatalf("migrated brief changed:\n got=%+v\nwant=%+v", got, want)
	}
}

func assertMigrated(t *testing.T, s *Store, wantA, wantKeep ItemBrief) {
	t.Helper()
	ctx := context.Background()
	has, err := itemBriefsHaveForeignKey(ctx, s.db, s.d)
	if err != nil {
		t.Fatalf("inspect migrated table: %v", err)
	}
	if !has {
		t.Fatal("migration did not add the item_briefs foreign key")
	}
	gotA, err := s.ItemBrief(ctx, "a")
	if err != nil {
		t.Fatalf("ItemBrief a: %v", err)
	}
	assertItemBriefEqual(t, gotA, wantA)
	gotKeep, err := s.ItemBrief(ctx, "keep")
	if err != nil {
		t.Fatalf("ItemBrief keep: %v", err)
	}
	assertItemBriefEqual(t, gotKeep, wantKeep)
	if _, err := s.ItemBrief(ctx, "gone"); !errors.Is(err, ErrItemBriefNotFound) {
		t.Fatalf("orphan brief survived migration: %v", err)
	}
}

// TestMigrateLegacyItemBriefs is the Blocker 2 regression: a database created
// with the published 417e76f schema upgrades on reopen, preserves valid briefs
// and every column, removes orphans, and enforces the key from then on.
func TestMigrateLegacyItemBriefs(t *testing.T) {
	ctx := context.Background()
	target := newMigrationTarget(t)
	makeLegacyItemBriefs(t, target.store)
	wantA, wantKeep := seedMigratableBriefs(t, target.store)
	insertOrphanBrief(t, target.store, "gone")
	if _, err := target.store.ItemBrief(ctx, "gone"); err != nil {
		t.Fatalf("orphan seed missing: %v", err)
	}
	if err := target.store.Close(); err != nil {
		t.Fatalf("close legacy store: %v", err)
	}

	migrated, err := target.open(t)
	if err != nil {
		t.Fatalf("reopen with migration: %v", err)
	}
	assertMigrated(t, migrated, wantA, wantKeep)
	if err := migrated.Close(); err != nil {
		t.Fatalf("close migrated store: %v", err)
	}

	// A second startup must be a no-op that keeps every value.
	again, err := target.open(t)
	if err != nil {
		t.Fatalf("second reopen: %v", err)
	}
	assertMigrated(t, again, wantA, wantKeep)

	// The guard is now enforced: a save after prune cannot recreate a brief.
	if _, err := again.PruneItems(ctx, PruneFilter{All: true}); err != nil {
		t.Fatalf("PruneItems: %v", err)
	}
	if err := again.SaveItemBrief(ctx, briefFixture("a")); err == nil {
		t.Fatal("save after prune succeeded on the migrated schema")
	}
	if _, err := again.ItemBrief(ctx, "a"); !errors.Is(err, ErrItemBriefNotFound) {
		t.Fatalf("brief after prune = %v, want ErrItemBriefNotFound", err)
	}
	recordBriefItems(t, again, "a")
	if _, err := again.ItemBrief(ctx, "a"); !errors.Is(err, ErrItemBriefNotFound) {
		t.Fatalf("re-ingested item exposed a stale brief: %v", err)
	}
}

// TestConcurrentPruneSaveCannotLeaveOrphan exercises the Postgres write-skew
// ordering directly: the item row is deleted in an uncommitted transaction
// while a save is in flight. The foreign key blocks the insert until the prune
// commits, then rejects it.
func TestConcurrentPruneSaveCannotLeaveOrphan(t *testing.T) {
	if !onPostgres() {
		t.Skip("write-skew ordering is exercised on Postgres")
	}
	ctx := context.Background()
	s := openTestStore(t)
	recordBriefItems(t, s, "a")
	if err := s.SaveItemBrief(ctx, briefFixture("a")); err != nil {
		t.Fatalf("SaveItemBrief: %v", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin prune: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "DELETE FROM item_briefs WHERE item_id = 'a'"); err != nil {
		t.Fatalf("delete brief: %v", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM items WHERE id = 'a'"); err != nil {
		t.Fatalf("delete item: %v", err)
	}

	saveErr := make(chan error, 1)
	go func() { saveErr <- s.SaveItemBrief(ctx, briefFixture("a")) }()
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit prune: %v", err)
	}
	if err := <-saveErr; err == nil {
		t.Fatal("in-flight save recreated a brief while its item was being pruned")
	}
	if _, err := s.ItemBrief(ctx, "a"); !errors.Is(err, ErrItemBriefNotFound) {
		t.Fatalf("orphan brief exists after concurrent prune/save: %v", err)
	}
}

// TestItemBriefMigrationRollsBackOnFailure injects a failure after the DDL but
// before commit, then inspects the raw database to prove the legacy table and
// its rows survived intact.
func TestItemBriefMigrationRollsBackOnFailure(t *testing.T) {
	ctx := context.Background()
	target := newMigrationTarget(t)
	makeLegacyItemBriefs(t, target.store)
	wantA, _ := seedMigratableBriefs(t, target.store)
	insertOrphanBrief(t, target.store, "gone")
	if err := target.store.Close(); err != nil {
		t.Fatalf("close legacy store: %v", err)
	}

	itemBriefMigrationFault = func(string) error { return errItemBriefMigrationInjected }
	t.Cleanup(func() { itemBriefMigrationFault = nil })
	if _, err := target.open(t); err == nil {
		t.Fatal("migration succeeded despite the injected fault")
	}

	raw, d := target.raw(t)
	defer func() { _ = raw.Close() }()
	has, err := itemBriefsHaveForeignKey(ctx, raw, d)
	if err != nil {
		t.Fatalf("inspect rolled-back table: %v", err)
	}
	if has {
		t.Fatal("failed migration left the new foreign key behind")
	}
	gotA, err := scanBriefRaw(ctx, raw, d, "a")
	if err != nil {
		t.Fatalf("valid brief lost on rollback: %v", err)
	}
	assertItemBriefEqual(t, gotA, wantA)
	if _, err := scanBriefRaw(ctx, raw, d, "gone"); err != nil {
		t.Fatalf("orphan row lost on rollback: %v", err)
	}

	itemBriefMigrationFault = nil
	migrated, err := target.open(t)
	if err != nil {
		t.Fatalf("retry after rollback: %v", err)
	}
	has, err = itemBriefsHaveForeignKey(ctx, migrated.db, migrated.d)
	if err != nil {
		t.Fatalf("inspect retried migration: %v", err)
	}
	if !has {
		t.Fatal("retry did not add the foreign key")
	}
	gotA, err = migrated.ItemBrief(ctx, "a")
	if err != nil {
		t.Fatalf("ItemBrief a after retry: %v", err)
	}
	assertItemBriefEqual(t, gotA, wantA)
	if _, err := migrated.ItemBrief(ctx, "gone"); !errors.Is(err, ErrItemBriefNotFound) {
		t.Fatalf("orphan survived retry: %v", err)
	}
}

// TestMigrateWrongTargetForeignKeySQLite pins the SQLite detector's target
// column check. A legacy table whose foreign key points at items(link) is not
// the item_id -> items(id) constraint; accepting it would leave a schema where
// an item's link can masquerade as an ItemID. The fixture is populated with
// foreign keys off only to build that deliberately malformed table; the store
// that reopens it enforces them normally.
func TestMigrateWrongTargetForeignKeySQLite(t *testing.T) {
	if onPostgres() {
		t.Skip("SQLite foreign-key target detection")
	}
	ctx := context.Background()
	target := newMigrationTarget(t)
	wantA, wantKeep := seedMigratableBriefs(t, target.store)
	if err := target.store.Close(); err != nil {
		t.Fatalf("close seeded store: %v", err)
	}

	// Rebuild item_briefs with FOREIGN KEY (item_id) REFERENCES items(link).
	// The rows are copied on one connection with foreign keys off, because the
	// malformed constraint cannot accept the valid item-ID rows; the reopened
	// store uses its normal connection and enforcement.
	raw, err := sql.Open("sqlite", dsn(target.path))
	if err != nil {
		t.Fatalf("open fixture connection: %v", err)
	}
	raw.SetMaxOpenConns(1)
	conn, err := raw.Conn(ctx)
	if err != nil {
		_ = raw.Close()
		t.Fatalf("fixture connection: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		_ = conn.Close()
		_ = raw.Close()
		t.Fatalf("fixture foreign_keys: %v", err)
	}
	wrong := strings.Replace(itemBriefSchema, "REFERENCES items(id)", "REFERENCES items(link)", 1)
	if wrong == itemBriefSchema {
		_ = conn.Close()
		_ = raw.Close()
		t.Fatal("could not build the wrong-target DDL")
	}
	for _, stmt := range []string{
		"ALTER TABLE item_briefs RENAME TO item_briefs_old",
		wrong,
		"INSERT INTO item_briefs (" + itemBriefColumns + ") SELECT " + itemBriefColumns +
			" FROM item_briefs_old",
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			_ = conn.Close()
			_ = raw.Close()
			t.Fatalf("build wrong-target fixture: %v", err)
		}
	}
	now := sqlTime(time.Now().UTC())
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO item_briefs (item_id, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?)`, "gone", ItemBriefReady, now, now); err != nil {
		_ = conn.Close()
		_ = raw.Close()
		t.Fatalf("insert wrong-target orphan: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "DROP TABLE item_briefs_old"); err != nil {
		_ = conn.Close()
		_ = raw.Close()
		t.Fatalf("drop legacy table: %v", err)
	}
	if err := conn.Close(); err != nil {
		_ = raw.Close()
		t.Fatalf("close fixture connection: %v", err)
	}

	// The detector must reject a foreign key aimed at items(link).
	has, err := itemBriefsHaveForeignKey(ctx, raw, sqliteDialect{})
	if err != nil {
		_ = raw.Close()
		t.Fatalf("inspect wrong-target table: %v", err)
	}
	if has {
		_ = raw.Close()
		t.Fatal("detector accepted a foreign key targeting items(link)")
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close fixture database: %v", err)
	}

	migrated, err := target.open(t)
	if err != nil {
		t.Fatalf("reopen with migration: %v", err)
	}
	assertMigrated(t, migrated, wantA, wantKeep)

	// The item's link is not an ItemID, so the restored items(id) constraint
	// must reject it. Before the fix the items(link) constraint accepted it.
	link := "https://x/a"
	if err := migrated.SaveItemBrief(ctx, briefFixture(link)); err == nil {
		t.Fatal("a brief with an item link as ItemID satisfied the rebuilt foreign key")
	}
	if _, err := migrated.ItemBrief(ctx, link); !errors.Is(err, ErrItemBriefNotFound) {
		t.Fatalf("link-keyed brief row = %v, want ErrItemBriefNotFound", err)
	}

	if err := migrated.Close(); err != nil {
		t.Fatalf("close migrated store: %v", err)
	}
	again, err := target.open(t)
	if err != nil {
		t.Fatalf("second reopen: %v", err)
	}
	assertMigrated(t, again, wantA, wantKeep)

	if _, err := again.PruneItems(ctx, PruneFilter{All: true}); err != nil {
		t.Fatalf("PruneItems: %v", err)
	}
	if err := again.SaveItemBrief(ctx, briefFixture("a")); err == nil {
		t.Fatal("save after prune succeeded on the rebuilt schema")
	}
	if _, err := again.ItemBrief(ctx, "a"); !errors.Is(err, ErrItemBriefNotFound) {
		t.Fatalf("brief after prune = %v, want ErrItemBriefNotFound", err)
	}
}
