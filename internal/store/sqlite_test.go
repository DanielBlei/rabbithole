package store

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strconv"
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

func TestRecordPersistsScoresWithoutDigesting(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	items := []feeds.Item{
		{ID: "a", Source: "S", Title: "A", Link: "https://x/a"},
		{ID: "b", Source: "S", Title: "B", Link: "https://x/b"},
		{ID: "c", Source: "S", Title: "C", Link: "https://x/c"},
	}
	// a made the digest; b was scored but fell below the cut; c never scored.
	scored := []DigestEntry{
		{Item: items[0], Score: 9, Reason: "good", Model: "m", Digested: true},
		{Item: items[1], Score: 4, Reason: "meh", Model: "m"},
	}
	if err := db.Record(ctx, items, scored, time.Now()); err != nil {
		t.Fatalf("Record: %v", err)
	}

	type row struct {
		score     *int
		digestDay *string
	}
	get := func(id string) row {
		var r row
		if err := db.db.QueryRowContext(ctx,
			"SELECT llm_score, digested_on FROM items WHERE id = ?", id).
			Scan(&r.score, &r.digestDay); err != nil {
			t.Fatalf("query %s: %v", id, err)
		}
		return r
	}

	// b: scored but not digested — keeps its score, no digest date.
	b := get("b")
	if b.score == nil || *b.score != 4 {
		t.Errorf("item b llm_score = %v, want 4 (score must persist even when not digested)", b.score)
	}
	if b.digestDay != nil {
		t.Errorf("item b digested_on = %v, want NULL (it didn't make the digest)", *b.digestDay)
	}
	// a: digested — has both score and digest date.
	a := get("a")
	if a.score == nil || *a.score != 9 || a.digestDay == nil {
		t.Errorf("item a = %+v, want score 9 and a digest date", a)
	}
	// c: never scored — seen-only.
	if c := get("c"); c.score != nil {
		t.Errorf("item c llm_score = %v, want NULL (never scored)", *c.score)
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
	if err := db.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM items WHERE link = ?", "https://x/dup").
		Scan(&count); err != nil {
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
	if err := db.UpdateUserState(
		ctx,
		"https://x/a",
		UserPatch{Status: &status, UserScore: &score, UserNote: &note},
	); err != nil {
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

func TestUpdateUserStateByID(t *testing.T) {
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

	// "a" is the item's id, not its link — UpdateUserState must resolve either.
	status := StatusRead
	if err := db.UpdateUserState(ctx, "a", UserPatch{Status: &status}); err != nil {
		t.Fatalf("UpdateUserState by id: %v", err)
	}

	var got string
	row := db.db.QueryRowContext(ctx, "SELECT status FROM items WHERE link = ?", "https://x/a")
	if err := row.Scan(&got); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got != StatusRead {
		t.Errorf("got status=%q, want %q", got, StatusRead)
	}
}

func TestList(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	items := []feeds.Item{
		{ID: "a", Source: "S1", Title: "A", Link: "https://x/a"},
		{ID: "b", Source: "S1", Title: "B", Link: "https://x/b"},
		{ID: "c", Source: "S2", Title: "C", Link: "https://x/c"},
		{ID: "d", Source: "S2", Title: "D", Link: "https://x/d"},
	}
	digested := []DigestEntry{
		{Item: items[0], Score: 5, Reason: "ok"},
		{Item: items[1], Score: 9, Reason: "great"},
	}
	if err := db.Record(ctx, items, digested, time.Now()); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// d: a user_score that outranks every llm_score, plus status=read.
	// c: left seen-only, no score at all — must sort last.
	userScore := 10
	readStatus := StatusRead
	if err := db.UpdateUserState(
		ctx,
		"https://x/d",
		UserPatch{UserScore: &userScore, Status: &readStatus},
	); err != nil {
		t.Fatalf("UpdateUserState: %v", err)
	}

	// Spread created_at across distinct days (oldest a -> newest d) so
	// After/Before windows and SortByLatest have something to distinguish.
	// Record always stamps created_at with time.Now(), so this is set
	// directly — there's no public API for backdating it.
	now := time.Now()
	for id, age := range map[string]time.Duration{
		"a": 4 * 24 * time.Hour,
		"b": 3 * 24 * time.Hour,
		"c": 2 * 24 * time.Hour,
		"d": 1 * 24 * time.Hour,
	} {
		if _, err := db.db.ExecContext(
			ctx,
			"UPDATE items SET created_at = ? WHERE id = ?",
			now.Add(-age),
			id,
		); err != nil {
			t.Fatalf("backdate %s: %v", id, err)
		}
	}

	tests := []struct {
		name    string
		filter  ListFilter
		wantIDs []string
		wantErr bool
	}{
		{
			name:    "no filter, best score first, nulls last",
			filter:  ListFilter{},
			wantIDs: []string{"d", "b", "a", "c"},
		},
		{
			name:    "status filter",
			filter:  ListFilter{Status: StatusRead},
			wantIDs: []string{"d"},
		},
		{
			name:    "source filter, still best score first within it",
			filter:  ListFilter{Source: "S1"},
			wantIDs: []string{"b", "a"},
		},
		{
			name:    "limit caps results",
			filter:  ListFilter{Limit: 2},
			wantIDs: []string{"d", "b"},
		},
		{
			name:    "zero limit falls back to default, not zero rows",
			filter:  ListFilter{Limit: 0},
			wantIDs: []string{"d", "b", "a", "c"},
		},
		{
			name:    "invalid status filter",
			filter:  ListFilter{Status: "archived"},
			wantErr: true,
		},
		{
			name:    "after filter, recent items only",
			filter:  ListFilter{After: now.Add(-60 * time.Hour)}, // > 2.5 days ago
			wantIDs: []string{"d", "c"},
		},
		{
			name:    "before filter, older items only",
			filter:  ListFilter{Before: now.Add(-60 * time.Hour)},
			wantIDs: []string{"b", "a"},
		},
		{
			name:    "after and before, bounded window",
			filter:  ListFilter{After: now.Add(-84 * time.Hour), Before: now.Add(-36 * time.Hour)},
			wantIDs: []string{"b", "c"},
		},
		{
			name:    "sort by latest, newest first regardless of score",
			filter:  ListFilter{SortBy: SortByLatest},
			wantIDs: []string{"d", "c", "b", "a"},
		},
		{
			name:    "sort by oldest, oldest first regardless of score",
			filter:  ListFilter{SortBy: SortByOldest},
			wantIDs: []string{"a", "b", "c", "d"},
		},
		{
			name:    "filter by multiple statuses, excludes the read item",
			filter:  ListFilter{Statuses: []string{StatusUnread, StatusSkipped}, SortBy: SortByOldest},
			wantIDs: []string{"a", "b", "c"},
		},
		{
			name:    "invalid status in set",
			filter:  ListFilter{Statuses: []string{StatusUnread, "bogus"}},
			wantErr: true,
		},
		{
			name:    "invalid sort filter",
			filter:  ListFilter{SortBy: "newest"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows, err := db.List(ctx, tt.filter)
			if tt.wantErr {
				if err == nil {
					t.Fatal("List() error = nil, want non-nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			gotIDs := make([]string, len(rows))
			for i, r := range rows {
				gotIDs[i] = r.ID
			}
			if !slices.Equal(gotIDs, tt.wantIDs) {
				t.Errorf("List() ids = %v, want %v", gotIDs, tt.wantIDs)
			}
		})
	}
}

func TestListLimitClamp(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	items := make([]feeds.Item, maxListLimit+5)
	for i := range items {
		id := strconv.Itoa(i)
		items[i] = feeds.Item{ID: id, Source: "S", Title: id, Link: "https://x/" + id}
	}
	if err := db.Record(ctx, items, nil, time.Now()); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// A caller asking for more than maxListLimit is capped, not honored.
	rows, err := db.List(ctx, ListFilter{Limit: maxListLimit * 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != maxListLimit {
		t.Errorf("got %d rows, want clamp to %d", len(rows), maxListLimit)
	}

	// Count is not bound by the list limit — it returns the true total.
	n, err := db.Count(ctx, ListFilter{})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != len(items) {
		t.Errorf("Count() = %d, want %d (uncapped)", n, len(items))
	}
}

func TestCount(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	items := []feeds.Item{
		{ID: "a", Source: "S1", Title: "A", Link: "https://x/a"},
		{ID: "b", Source: "S1", Title: "B", Link: "https://x/b"},
		{ID: "c", Source: "S2", Title: "C", Link: "https://x/c"},
		{ID: "d", Source: "S2", Title: "D", Link: "https://x/d"},
	}
	if err := db.Record(ctx, items, nil, time.Now()); err != nil {
		t.Fatalf("Record: %v", err)
	}
	readStatus := StatusRead
	if err := db.UpdateUserState(ctx, "https://x/d", UserPatch{Status: &readStatus}); err != nil {
		t.Fatalf("UpdateUserState: %v", err)
	}

	tests := []struct {
		name    string
		filter  ListFilter
		want    int
		wantErr bool
	}{
		{name: "all items, status-agnostic", filter: ListFilter{}, want: 4},
		{name: "filter by source", filter: ListFilter{Source: "S1"}, want: 2},
		{name: "filter by status read", filter: ListFilter{Status: StatusRead}, want: 1},
		{name: "filter by status unread", filter: ListFilter{Status: StatusUnread}, want: 3},
		{name: "multi-status set", filter: ListFilter{Statuses: []string{StatusRead, StatusUnread}}, want: 4},
		{name: "invalid status", filter: ListFilter{Status: "bogus"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := db.Count(ctx, tt.filter)
			if tt.wantErr {
				if err == nil {
					t.Fatal("Count() error = nil, want non-nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("Count: %v", err)
			}
			if got != tt.want {
				t.Errorf("Count() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestSources(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	items := []feeds.Item{
		{ID: "a", Source: "S1", Title: "A", Link: "https://x/a"},
		{ID: "b", Source: "S1", Title: "B", Link: "https://x/b"},
		{ID: "c", Source: "S2", Title: "C", Link: "https://x/c"},
	}
	if err := db.Record(ctx, items, nil, time.Now()); err != nil {
		t.Fatalf("Record: %v", err)
	}

	counts, err := db.Sources(ctx)
	if err != nil {
		t.Fatalf("Sources: %v", err)
	}
	want := []SourceCount{{Source: "S1", Count: 2}, {Source: "S2", Count: 1}}
	if !slices.Equal(counts, want) {
		t.Errorf("Sources() = %+v, want %+v", counts, want)
	}
}
