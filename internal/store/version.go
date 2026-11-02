// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"fmt"
)

// storeVersionSchema holds one row: how many feed and item write transactions
// have committed to this database. It lives in the database, so every process
// reading it sees the same count, and each write bumps it in the same
// transaction, so a reader never sees the write without the new count.
const storeVersionSchema = `
CREATE TABLE IF NOT EXISTS store_version (
	id      INTEGER PRIMARY KEY CHECK (id = 1),
	version INTEGER NOT NULL
)`

const storeVersionSchemaPG = `
CREATE TABLE IF NOT EXISTS store_version (
	id      INTEGER PRIMARY KEY CHECK (id = 1),
	version BIGINT NOT NULL
)`

const (
	sqlBumpVersion = `INSERT INTO store_version (id, version) VALUES (1, 1)
		ON CONFLICT (id) DO UPDATE SET version = store_version.version + 1`
	sqlReadVersion = `SELECT COALESCE((SELECT version FROM store_version WHERE id = 1), 0)`
)

// bumpVersion records one feed or item write inside tx.
func (s *Store) bumpVersion(ctx context.Context, tx *sql.Tx) error {
	if _, err := s.txExec(ctx, tx, sqlBumpVersion); err != nil {
		return fmt.Errorf("record store version: %w", err)
	}
	return nil
}

// Version returns how many feed and item write transactions have committed to
// this database, by any process. It changes exactly when something a cached
// Sources result is drawn from has been written.
func (s *Store) Version(ctx context.Context) (int64, error) {
	var v int64
	if err := s.queryRow(ctx, sqlReadVersion).Scan(&v); err != nil {
		return 0, fmt.Errorf("read store version: %w", err)
	}
	return v, nil
}
