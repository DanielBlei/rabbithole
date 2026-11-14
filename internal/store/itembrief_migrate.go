// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// itemBriefMigrationFault is a test seam. It runs inside an open migration
// transaction so a test can prove that a failure rolls the migration back. It
// is nil in production.
var itemBriefMigrationFault func(engine string) error

// migrateItemBriefs gives an item_briefs table created by the published PR #73
// schema (which had no foreign key) the constraint added in Phase 1. It is
// idempotent: a table that already has the item -> items(id) cascade
// constraint is left untouched.
func migrateItemBriefs(ctx context.Context, db *sql.DB, d dialect) error {
	exists, err := d.tableExists(ctx, db, "item_briefs")
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	hasFK, err := itemBriefsHaveForeignKey(ctx, db, d)
	if err != nil {
		return err
	}
	if hasFK {
		return nil
	}
	switch d.name() {
	case "sqlite":
		return migrateItemBriefsSQLite(ctx, db)
	case "postgres":
		return migrateItemBriefsPostgres(ctx, db)
	default:
		return fmt.Errorf("item briefs migration: unsupported engine %q", d.name())
	}
}

// itemBriefsHaveForeignKey reports whether item_briefs already carries the
// item_id -> items(id) ON DELETE CASCADE constraint.
func itemBriefsHaveForeignKey(ctx context.Context, db *sql.DB, d dialect) (bool, error) {
	switch d.name() {
	case "sqlite":
		rows, err := db.QueryContext(ctx, "PRAGMA foreign_key_list(item_briefs)")
		if err != nil {
			return false, fmt.Errorf("inspect item_briefs foreign keys: %w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var (
				id, seq                                int
				table, from, to, onUpdate, onDelete, m string
			)
			if err := rows.Scan(&id, &seq, &table, &from, &to, &onUpdate, &onDelete, &m); err != nil {
				return false, fmt.Errorf("scan item_briefs foreign keys: %w", err)
			}
			// The target column matters: a foreign key to items(link) is not
			// the item_id -> items(id) constraint this store relies on.
			if strings.EqualFold(table, "items") &&
				strings.EqualFold(from, "item_id") &&
				strings.EqualFold(to, "id") &&
				strings.EqualFold(onDelete, "CASCADE") {
				return true, nil
			}
		}
		return false, rows.Err()
	case "postgres":
		var n int
		err := db.QueryRowContext(ctx, `
			SELECT COUNT(*)
			FROM information_schema.table_constraints tc
			JOIN information_schema.referential_constraints rc
			  ON rc.constraint_name = tc.constraint_name
			 AND rc.constraint_schema = tc.constraint_schema
			JOIN information_schema.key_column_usage kcu
			  ON kcu.constraint_name = tc.constraint_name
			 AND kcu.constraint_schema = tc.constraint_schema
			JOIN information_schema.constraint_column_usage ccu
			  ON ccu.constraint_name = tc.constraint_name
			 AND ccu.constraint_schema = tc.constraint_schema
			WHERE tc.constraint_type = 'FOREIGN KEY'
			  AND tc.table_schema = current_schema()
			  AND tc.table_name = 'item_briefs'
			  AND kcu.column_name = 'item_id'
			  AND ccu.table_name = 'items'
			  AND ccu.column_name = 'id'
			  AND rc.delete_rule = 'CASCADE'`).Scan(&n)
		if err != nil {
			return false, fmt.Errorf("inspect item_briefs foreign keys: %w", err)
		}
		return n > 0, nil
	default:
		return false, fmt.Errorf("item briefs migration: unsupported engine %q", d.name())
	}
}

// itemBriefRebuildDDL is the current item_briefs schema under another table
// name, used by SQLite's table rebuild.
func itemBriefRebuildDDL(table string) string {
	return strings.Replace(itemBriefSchema,
		"CREATE TABLE IF NOT EXISTS item_briefs",
		"CREATE TABLE IF NOT EXISTS "+table, 1)
}

// migrateItemBriefsSQLite rebuilds the table, because SQLite cannot add a
// foreign key in place. Rows whose item no longer exists are dropped; valid
// rows are copied verbatim. Everything runs in one transaction, so an
// interrupted migration leaves the legacy table exactly as it was.
func migrateItemBriefsSQLite(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin item briefs migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, stmt := range []string{
		"DROP TABLE IF EXISTS item_briefs_new",
		itemBriefRebuildDDL("item_briefs_new"),
		"INSERT INTO item_briefs_new (" + itemBriefColumns + ") SELECT " + itemBriefColumns +
			" FROM item_briefs WHERE item_id IN (SELECT id FROM items)",
		"DROP TABLE item_briefs",
		"ALTER TABLE item_briefs_new RENAME TO item_briefs",
	} {
		if _, err := execRawTx(ctx, tx, stmt); err != nil {
			return fmt.Errorf("migrate item_briefs (sqlite): %w", err)
		}
	}
	if itemBriefMigrationFault != nil {
		if err := itemBriefMigrationFault("sqlite"); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit item briefs migration: %w", err)
	}
	return nil
}

// migrateItemBriefsPostgres deletes orphan rows, then adds the foreign key in
// place. The transaction makes the delete and constraint addition atomic: a
// failure restores both the rows and the legacy schema.
func migrateItemBriefsPostgres(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin item briefs migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := execRawTx(ctx, tx,
		`DELETE FROM item_briefs
		 WHERE NOT EXISTS (SELECT 1 FROM items WHERE items.id = item_briefs.item_id)`); err != nil {
		return fmt.Errorf("remove orphan item briefs: %w", err)
	}
	if _, err := execRawTx(ctx, tx,
		`ALTER TABLE item_briefs
		 ADD CONSTRAINT item_briefs_item_id_items_fkey
		 FOREIGN KEY (item_id) REFERENCES items(id) ON DELETE CASCADE`); err != nil {
		return fmt.Errorf("add item_briefs foreign key: %w", err)
	}
	if itemBriefMigrationFault != nil {
		if err := itemBriefMigrationFault("postgres"); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit item briefs migration: %w", err)
	}
	return nil
}
