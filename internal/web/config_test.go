// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"path/filepath"
	"testing"

	"github.com/DanielBlei/rabbithole/internal/store"
)

func configTestWeb(t *testing.T) (*Web, *store.Store) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return New(db, db.ConfigSource(), testIngestManager(t, db)), db
}
