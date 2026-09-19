// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"path/filepath"
	"testing"
)

// openTestStore opens a throwaway store, closed on cleanup. Every test and
// benchmark in this package goes through it, so pointing the suite at a second
// engine is one change here rather than one per test.
func openTestStore(t testing.TB) *Store {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
