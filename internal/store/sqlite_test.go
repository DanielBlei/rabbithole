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

func TestRecordIgnoresDuplicateLink(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	// Different IDs (e.g. distinct guids) can still resolve to the same link.
	// The UNIQUE constraint must keep UpdateUserState's WHERE link = ? targeting
	// exactly one row.
	first := feeds.Item{ID: "a", Source: "S", Title: "A", Link: "https://x/dup"}
	second := feeds.Item{ID: "b", Source: "S", Title: "B", Link: "https://x/dup"}
	if err := db.Record(ctx, []feeds.Item{first}, nil, time.Now()); err != nil {
		t.Fatalf("first Record: %v", err)
	}
	if err := db.Record(ctx, []feeds.Item{second}, nil, time.Now()); err != nil {
		t.Fatalf("second Record: %v", err)
	}

	var count int
	if err := db.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM items WHERE link = ?", "https://x/dup").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("want 1 row for duplicate link, got %d", count)
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

func TestUpdateUserStateErrors(t *testing.T) {
	readStatus := StatusRead
	badStatus := "archived"
	tests := []struct {
		name      string
		seedItem  bool
		link      string
		patch     UserPatch
		wantErrIs error // checked via errors.Is when non-nil
		wantErr   bool  // checked via plain non-nil when wantErrIs is nil
	}{
		{
			name:      "not found",
			link:      "https://x/missing",
			patch:     UserPatch{Status: &readStatus},
			wantErrIs: ErrItemNotFound,
		},
		{
			name:     "invalid status",
			seedItem: true,
			link:     "https://x/a",
			patch:    UserPatch{Status: &badStatus},
			wantErr:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, err := Open(filepath.Join(t.TempDir(), "test.db"))
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer func() { _ = db.Close() }()
			ctx := context.Background()

			if tt.seedItem {
				items := []feeds.Item{{ID: "a", Source: "S", Title: "A", Link: tt.link}}
				if err := db.Record(ctx, items, nil, time.Now()); err != nil {
					t.Fatalf("Record: %v", err)
				}
			}

			err = db.UpdateUserState(ctx, tt.link, tt.patch)
			if tt.wantErrIs != nil {
				if !errors.Is(err, tt.wantErrIs) {
					t.Errorf("UpdateUserState() error = %v, want errors.Is(_, %v)", err, tt.wantErrIs)
				}
				return
			}
			if tt.wantErr && err == nil {
				t.Error("UpdateUserState() error = nil, want non-nil")
			}
		})
	}
}
