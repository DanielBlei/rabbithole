// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func okIcon(id string, data []byte) FeedIcon {
	return FeedIcon{
		FeedID: id, FeedURL: "https://" + id + ".example/feed", SiteHost: id + ".example",
		Status: IconStatusOK, Mime: "image/png", Bytes: data, ETag: "e-" + id,
		FetchedAt: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC),
	}
}

func TestFeedIconUpsertInsertThenUpdate(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()

	if err := s.UpsertFeedIcons(ctx, nil); err != nil {
		t.Fatalf("empty upsert: %v", err)
	}
	if meta, err := s.FeedIconMeta(ctx); err != nil || len(meta) != 0 {
		t.Fatalf("meta after empty upsert = %v, %v; want empty", meta, err)
	}

	if err := s.UpsertFeedIcons(ctx, []FeedIcon{okIcon("a", []byte{1, 2, 3}), okIcon("b", []byte{4})}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := s.FeedIconBytes(ctx, "a")
	if err != nil || !bytes.Equal(got.Bytes, []byte{1, 2, 3}) || got.ETag != "e-a" {
		t.Fatalf("FeedIconBytes(a) = %+v, %v", got, err)
	}

	// An update replaces the row, and a lookup that finds nothing drops the bytes.
	none := FeedIcon{
		FeedID: "a", FeedURL: "https://new.example/feed", SiteHost: "new.example",
		Status: IconStatusNone, Error: "no icon", FetchedAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
		Bytes: []byte{9, 9},
	}
	if err := s.UpsertFeedIcons(ctx, []FeedIcon{none}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := s.FeedIconBytes(ctx, "a"); !errors.Is(err, ErrFeedIconNotFound) {
		t.Errorf("FeedIconBytes after none = %v, want ErrFeedIconNotFound", err)
	}
	meta, err := s.FeedIconMeta(ctx)
	if err != nil || len(meta) != 2 {
		t.Fatalf("meta = %v, %v; want 2 rows", meta, err)
	}
	a := meta["a"]
	if a.Status != IconStatusNone || a.FeedURL != "https://new.example/feed" || a.SiteHost != "new.example" ||
		a.Error != "no icon" || a.Mime != "" || a.ETag != "" || a.Dark {
		t.Errorf("meta[a] = %+v", a)
	}
	var n int
	err = s.queryRow(ctx,
		"SELECT COUNT(*) FROM feed_icons WHERE feed_id = ? AND bytes IS NULL", "a").Scan(&n)
	if err != nil || n != 1 {
		t.Errorf("bytes of a none row: count %d, err %v; want NULL", n, err)
	}
	if b, err := s.FeedIconBytes(ctx, "b"); err != nil || !bytes.Equal(b.Bytes, []byte{4}) {
		t.Errorf("b untouched: %+v, %v", b, err)
	}
}

func TestFeedIconBytesRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()

	raw := make([]byte, 60<<10)
	for i := range raw {
		raw[i] = byte(i % 251)
	}
	raw[0], raw[1], raw[2], raw[len(raw)-1] = 0x00, 0xFF, 0x00, 0xFF
	if err := s.UpsertFeedIcons(ctx, []FeedIcon{okIcon("big", raw)}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := s.FeedIconBytes(ctx, "big")
	if err != nil {
		t.Fatalf("FeedIconBytes: %v", err)
	}
	if !bytes.Equal(got.Bytes, raw) {
		t.Errorf("bytes differ after round trip (got %d, want %d)", len(got.Bytes), len(raw))
	}
	if got.Mime != "image/png" || got.FeedID != "big" || got.SiteHost != "big.example" {
		t.Errorf("fields = %+v", got)
	}
}

func TestFeedIconMetaHasNoBytes(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()

	if err := s.UpsertFeedIcons(ctx, []FeedIcon{okIcon("a", []byte{1, 2, 3})}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	meta, err := s.FeedIconMeta(ctx)
	if err != nil {
		t.Fatalf("FeedIconMeta: %v", err)
	}
	ic, ok := meta["a"]
	if !ok || ic.FeedID != "a" {
		t.Fatalf("meta = %v; want key a", meta)
	}
	if ic.Bytes != nil {
		t.Errorf("meta bytes = %v, want nil", ic.Bytes)
	}
	if ic.ETag != "e-a" || ic.Status != IconStatusOK {
		t.Errorf("meta[a] = %+v", ic)
	}
}

func TestFeedIconBytesNotFound(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()

	if _, err := s.FeedIconBytes(ctx, "missing"); !errors.Is(err, ErrFeedIconNotFound) {
		t.Errorf("missing = %v, want ErrFeedIconNotFound", err)
	}
	for _, status := range []string{IconStatusNone, IconStatusError} {
		ic := FeedIcon{FeedID: status, Status: status, FetchedAt: time.Now()}
		if err := s.UpsertFeedIcons(ctx, []FeedIcon{ic}); err != nil {
			t.Fatalf("upsert %s: %v", status, err)
		}
		if _, err := s.FeedIconBytes(ctx, status); !errors.Is(err, ErrFeedIconNotFound) {
			t.Errorf("status %s = %v, want ErrFeedIconNotFound", status, err)
		}
	}
}

func TestFeedIconDarkAndFetchedAtRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()

	zone := time.FixedZone("x", 5*3600+1800)
	when := time.Date(2026, 10, 30, 17, 45, 12, 0, zone)
	dark := okIcon("dark", []byte{1})
	dark.Dark, dark.FetchedAt = true, when
	light := okIcon("light", []byte{2})
	if err := s.UpsertFeedIcons(ctx, []FeedIcon{dark, light}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	meta, err := s.FeedIconMeta(ctx)
	if err != nil {
		t.Fatalf("FeedIconMeta: %v", err)
	}
	if !meta["dark"].Dark || meta["light"].Dark {
		t.Errorf("dark flags = %v, %v; want true, false", meta["dark"].Dark, meta["light"].Dark)
	}
	got := meta["dark"].FetchedAt
	if !got.Equal(when) || got.Location() != time.UTC {
		t.Errorf("fetched_at = %v (%v), want %v in UTC", got, got.Location(), when.UTC())
	}
	full, err := s.FeedIconBytes(ctx, "dark")
	if err != nil || !full.Dark || !full.FetchedAt.Equal(when) || full.FetchedAt.Location() != time.UTC {
		t.Errorf("FeedIconBytes = %+v, %v", full, err)
	}
}

func TestFeedIconTableCreatedOnExistingDatabase(t *testing.T) {
	if onPostgres() {
		t.Skip("simulates an upgrade by reopening a SQLite file")
	}
	path := filepath.Join(t.TempDir(), "old.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.exec(t.Context(), "DROP TABLE feed_icons"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if meta, err := s.FeedIconMeta(t.Context()); err != nil || len(meta) != 0 {
		t.Fatalf("meta after upgrade = %v, %v; want empty table", meta, err)
	}
	if err := s.UpsertFeedIcons(t.Context(), []FeedIcon{okIcon("a", []byte{1})}); err != nil {
		t.Fatalf("upsert after upgrade: %v", err)
	}
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != schemaVersion {
		t.Errorf("user_version = %d, %v; want %d", version, err, schemaVersion)
	}
}
