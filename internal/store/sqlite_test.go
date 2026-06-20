package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/DanielBlei/ai-searcher/internal/feeds"
)

func TestRecordAndSeenRoundTrip(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	items := []feeds.Item{
		{ID: "a", Source: "S", Title: "A", Link: "https://x/a"},
		{ID: "b", Source: "S", Title: "B", Link: "https://x/b"},
	}
	digested := []DigestEntry{{Item: items[0], Score: 9, Reason: "good"}}

	if err := db.Record(ctx, items, digested, time.Now()); err != nil {
		t.Fatalf("Record: %v", err)
	}

	seen, err := db.Seen(ctx, []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("Seen: %v", err)
	}
	if !seen["a"] || !seen["b"] {
		t.Errorf("recorded items should be seen: %+v", seen)
	}
	if seen["c"] {
		t.Error("unrecorded item should not be seen")
	}
}

func TestRecordIsIdempotent(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	items := []feeds.Item{{ID: "a", Source: "S", Title: "A", Link: "https://x/a"}}
	if err := db.Record(ctx, items, nil, time.Now()); err != nil {
		t.Fatalf("first Record: %v", err)
	}
	// Re-recording the same item must not error (INSERT OR IGNORE).
	if err := db.Record(ctx, items, nil, time.Now()); err != nil {
		t.Fatalf("second Record: %v", err)
	}
}

func TestUpdateUserState(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	items := []feeds.Item{{ID: "a", Source: "S", Title: "A", Link: "https://x/a"}}
	if err := db.Record(ctx, items, nil, time.Now()); err != nil {
		t.Fatalf("Record: %v", err)
	}

	status := StatusRead
	score := 8
	note := "worth a reread"
	if err := db.UpdateUserState(ctx, "https://x/a", UserPatch{Status: &status, UserScore: &score, UserNote: &note}); err != nil {
		t.Fatalf("UpdateUserState: %v", err)
	}

	var gotStatus, gotNote string
	var gotScore int
	row := db.db.QueryRowContext(ctx, "SELECT status, user_score, user_note FROM items WHERE link = ?", "https://x/a")
	if err := row.Scan(&gotStatus, &gotScore, &gotNote); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if gotStatus != StatusRead || gotScore != 8 || gotNote != note {
		t.Errorf("got status=%q score=%d note=%q", gotStatus, gotScore, gotNote)
	}
}

func TestUpdateUserStateNotFound(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	status := StatusRead
	err = db.UpdateUserState(context.Background(), "https://x/missing", UserPatch{Status: &status})
	if !errors.Is(err, ErrItemNotFound) {
		t.Errorf("expected ErrItemNotFound, got %v", err)
	}
}

func TestUpdateUserStateInvalidStatus(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	bad := "archived"
	if err := db.UpdateUserState(context.Background(), "https://x/a", UserPatch{Status: &bad}); err == nil {
		t.Error("expected error for invalid status")
	}
}
