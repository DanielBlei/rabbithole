// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/feeds"
)

// Version moves for feed and item writes and holds still for reads, so a cache
// keyed on it recomputes only when there is something new to read.
func TestVersionMovesOnFeedAndItemWrites(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "version.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := t.Context()

	step := func(name string, want bool, f func()) {
		t.Helper()
		before := version(t, db)
		f()
		if moved := version(t, db) != before; moved != want {
			t.Errorf("%s: moved = %v, want %v", name, moved, want)
		}
	}

	step("read", false, func() {
		if _, err := db.FeedsDoc(ctx); err != nil {
			t.Fatal(err)
		}
	})
	step("seed feeds", true, func() {
		if _, err := db.SeedFeeds(
			ctx,
			config.FeedsDoc{Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}}},
		); err != nil {
			t.Fatal(err)
		}
	})
	step("record items", true, func() {
		if err := db.Record(
			ctx,
			[]feeds.Item{{ID: "a", Source: "Alpha", Title: "A", Link: "https://x/a"}},
			nil,
			time.Now(),
		); err != nil {
			t.Fatal(err)
		}
	})
	step("read items", false, func() {
		if _, err := db.SourceStats(ctx, time.Now(), SourceStatsOpts{}); err != nil {
			t.Fatal(err)
		}
	})
}

// The count is stored in the database, so a reopened store continues from it.
func TestVersionSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reopen.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Record(
		t.Context(),
		[]feeds.Item{{ID: "a", Source: "Alpha", Title: "A", Link: "https://x/a"}},
		nil,
		time.Now(),
	); err != nil {
		t.Fatal(err)
	}
	before := version(t, db)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if got := version(t, db); got != before {
		t.Errorf("version after reopen = %d, want %d", got, before)
	}
}

func version(t *testing.T, db *Store) int64 {
	t.Helper()
	v, err := db.Version(t.Context())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	return v
}
