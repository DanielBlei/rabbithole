// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// libraryHiddenSchema holds the Library entries the user hid, keyed on the
// library URL key. Hiding only drops an entry from the Library view and the
// Overview suggestions: the remote library still lists it, and a hidden entry
// that was added stays added. Plain TEXT and ON CONFLICT DO NOTHING, so the
// table ports to Postgres as is.
const libraryHiddenSchema = `
CREATE TABLE IF NOT EXISTS library_hidden (
	url_key   TEXT PRIMARY KEY,
	hidden_at TIMESTAMP NOT NULL
);
`

const libraryHiddenSchemaPG = `
CREATE TABLE IF NOT EXISTS library_hidden (
	url_key   TEXT PRIMARY KEY,
	hidden_at TIMESTAMPTZ NOT NULL
);
`

const (
	sqlLibraryHiddenKeys = `SELECT url_key FROM library_hidden`
	sqlLibraryHide       = `INSERT INTO library_hidden (url_key, hidden_at) VALUES (?, ?) ON CONFLICT (url_key) DO NOTHING`
	sqlLibraryUnhide     = `DELETE FROM library_hidden WHERE url_key = ?`
)

// HiddenLibrary returns the URL keys of the hidden Library entries.
func (s *Store) HiddenLibrary(ctx context.Context) (map[string]bool, error) {
	rows, err := s.query(ctx, sqlLibraryHiddenKeys)
	if err != nil {
		return nil, fmt.Errorf("read hidden library entries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("scan hidden library entry: %w", err)
		}
		out[key] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read hidden library entries: %w", err)
	}
	return out, nil
}

// HideLibrary hides the entries with the given URL keys. Hiding one twice keeps
// the first time.
func (s *Store) HideLibrary(ctx context.Context, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	now := time.Now()
	return s.inTx(ctx, func(tx *sql.Tx) error {
		for _, key := range keys {
			if _, err := s.txExec(ctx, tx, sqlLibraryHide, key, sqlTime(now)); err != nil {
				return fmt.Errorf("hide library entry: %w", err)
			}
		}
		return nil
	})
}

// UnhideLibrary shows the entries with the given URL keys again.
func (s *Store) UnhideLibrary(ctx context.Context, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		for _, key := range keys {
			if _, err := s.txExec(ctx, tx, sqlLibraryUnhide, key); err != nil {
				return fmt.Errorf("unhide library entry: %w", err)
			}
		}
		return nil
	})
}
