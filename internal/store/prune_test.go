package store

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/feeds"
)

// seedPrunable records four items across two sources and backdates them one
// through four days, so source filters and time bounds each have something to
// separate. Record stamps created_at with time.Now(), so ages are set directly;
// the write goes through sqlTime or it won't compare against the bounds.
func seedPrunable(t *testing.T, db *Store, now time.Time) {
	t.Helper()
	ctx := context.Background()

	items := []feeds.Item{
		{ID: "a", Source: "S1", Title: "A", Link: "https://x/a"},
		{ID: "b", Source: "S1", Title: "B", Link: "https://x/b"},
		{ID: "c", Source: "S2", Title: "C", Link: "https://x/c"},
		{ID: "d", Source: "S2", Title: "D", Link: "https://x/d"},
	}
	if err := db.Record(ctx, items, nil, now); err != nil {
		t.Fatalf("Record: %v", err)
	}
	for id, age := range map[string]time.Duration{
		"a": 4 * 24 * time.Hour,
		"b": 3 * 24 * time.Hour,
		"c": 2 * 24 * time.Hour,
		"d": 1 * 24 * time.Hour,
	} {
		if _, err := db.db.ExecContext(ctx,
			"UPDATE items SET created_at = ? WHERE id = ?", sqlTime(now.Add(-age)), id); err != nil {
			t.Fatalf("backdate %s: %v", id, err)
		}
	}
}

// remainingIDs is every item id still in the store, sorted, for asserting what a
// prune left behind.
func remainingIDs(t *testing.T, db *Store) []string {
	t.Helper()
	rows, err := db.db.QueryContext(context.Background(), "SELECT id FROM items ORDER BY id")
	if err != nil {
		t.Fatalf("query ids: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan id: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return ids
}

func TestPruneItemsRefusesUnboundedFilter(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name   string
		filter PruneFilter
	}{
		{
			name:   "no source and no bounds would select everything",
			filter: PruneFilter{},
		},
		{
			name:   "include-saved alone is not a selection",
			filter: PruneFilter{IncludeSaved: true},
		},
		{
			name:   "before is not after since, so the window is empty",
			filter: PruneFilter{After: now.Add(-time.Hour), Before: now.Add(-2 * time.Hour)},
		},
		{
			name:   "all contradicts a source",
			filter: PruneFilter{All: true, Source: "S1"},
		},
		{
			name:   "all contradicts a date bound",
			filter: PruneFilter{All: true, Before: now},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openTestStore(t)
			seedPrunable(t, db, now)

			if _, err := db.PruneItems(context.Background(), tt.filter); !errors.Is(err, ErrInvalidFilter) {
				t.Errorf("PruneItems() error = %v, want ErrInvalidFilter", err)
			}
			if got := remainingIDs(t, db); len(got) != 4 {
				t.Errorf("items after refused prune = %v, want all 4 intact", got)
			}
			if _, err := db.PrunePreview(context.Background(), tt.filter, 10); !errors.Is(err, ErrInvalidFilter) {
				t.Errorf("PrunePreview() error = %v, want ErrInvalidFilter", err)
			}
		})
	}
}

func TestPruneItems(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name        string
		filter      PruneFilter
		wantDeleted int
		wantRemain  []string
	}{
		{
			name:        "source only takes that feed and leaves the other",
			filter:      PruneFilter{Source: "S1"},
			wantDeleted: 2,
			wantRemain:  []string{"c", "d"},
		},
		{
			name:        "source matching nothing is a no-op",
			filter:      PruneFilter{Source: "nope"},
			wantDeleted: 0,
			wantRemain:  []string{"a", "b", "c", "d"},
		},
		{
			name:        "before takes everything older than the cutoff",
			filter:      PruneFilter{Before: now.Add(-36 * time.Hour)},
			wantDeleted: 3,
			wantRemain:  []string{"d"},
		},
		{
			name:        "since takes everything newer than the cutoff",
			filter:      PruneFilter{After: now.Add(-36 * time.Hour)},
			wantDeleted: 1,
			wantRemain:  []string{"a", "b", "c"},
		},
		{
			name:        "since and before bound a window",
			filter:      PruneFilter{After: now.Add(-84 * time.Hour), Before: now.Add(-36 * time.Hour)},
			wantDeleted: 2,
			wantRemain:  []string{"a", "d"},
		},
		{
			name:        "source and before intersect",
			filter:      PruneFilter{Source: "S1", Before: now.Add(-84 * time.Hour)},
			wantDeleted: 1,
			wantRemain:  []string{"b", "c", "d"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openTestStore(t)
			seedPrunable(t, db, now)
			ctx := context.Background()

			// The preview must agree with the delete that follows it, or the
			// dry-run is lying about what it is about to do.
			preview, err := db.PrunePreview(ctx, tt.filter, 10)
			if err != nil {
				t.Fatalf("PrunePreview: %v", err)
			}
			if preview.Deleted != tt.wantDeleted {
				t.Errorf("PrunePreview().Deleted = %d, want %d", preview.Deleted, tt.wantDeleted)
			}
			if len(preview.Sample) != tt.wantDeleted {
				t.Errorf("len(PrunePreview().Sample) = %d, want %d", len(preview.Sample), tt.wantDeleted)
			}

			result, err := db.PruneItems(ctx, tt.filter)
			if err != nil {
				t.Fatalf("PruneItems: %v", err)
			}
			if result.Deleted != tt.wantDeleted {
				t.Errorf("PruneItems().Deleted = %d, want %d", result.Deleted, tt.wantDeleted)
			}
			if got := remainingIDs(t, db); !slices.Equal(got, tt.wantRemain) {
				t.Errorf("remaining items = %v, want %v", got, tt.wantRemain)
			}
		})
	}
}

// TestPruneItemsAll covers the one filter that empties the feed. The saved-item
// guard still applies to it, so --all alone leaves your bookmarks; clearing the
// table outright is --all with IncludeSaved, the `make clean-feeds` reset.
func TestPruneItemsAll(t *testing.T) {
	now := time.Now()
	bookmark := true

	t.Run("all keeps saved items", func(t *testing.T) {
		db := openTestStore(t)
		seedPrunable(t, db, now)
		ctx := context.Background()
		if err := db.UpdateUserState(ctx, "https://x/a", UserPatch{Bookmarked: &bookmark}); err != nil {
			t.Fatalf("UpdateUserState: %v", err)
		}

		result, err := db.PruneItems(ctx, PruneFilter{All: true})
		if err != nil {
			t.Fatalf("PruneItems: %v", err)
		}
		if result.Deleted != 3 || result.Kept != 1 {
			t.Errorf("PruneItems() = {Deleted:%d Kept:%d}, want {Deleted:3 Kept:1}", result.Deleted, result.Kept)
		}
		if got := remainingIDs(t, db); !slices.Equal(got, []string{"a"}) {
			t.Errorf("remaining items = %v, want [a]", got)
		}
	})

	t.Run("all with include-saved empties the table", func(t *testing.T) {
		db := openTestStore(t)
		seedPrunable(t, db, now)
		ctx := context.Background()
		if err := db.UpdateUserState(ctx, "https://x/a", UserPatch{Bookmarked: &bookmark}); err != nil {
			t.Fatalf("UpdateUserState: %v", err)
		}

		result, err := db.PruneItems(ctx, PruneFilter{All: true, IncludeSaved: true})
		if err != nil {
			t.Fatalf("PruneItems: %v", err)
		}
		if result.Deleted != 4 || result.Kept != 0 {
			t.Errorf("PruneItems() = {Deleted:%d Kept:%d}, want {Deleted:4 Kept:0}", result.Deleted, result.Kept)
		}
		if got := remainingIDs(t, db); len(got) != 0 {
			t.Errorf("remaining items = %v, want none", got)
		}
	})

	t.Run("preview agrees and deletes nothing", func(t *testing.T) {
		db := openTestStore(t)
		seedPrunable(t, db, now)

		preview, err := db.PrunePreview(context.Background(), PruneFilter{All: true, IncludeSaved: true}, 2)
		if err != nil {
			t.Fatalf("PrunePreview: %v", err)
		}
		if preview.Deleted != 4 {
			t.Errorf("PrunePreview().Deleted = %d, want 4", preview.Deleted)
		}
		if got := remainingIDs(t, db); len(got) != 4 {
			t.Errorf("items after preview = %v, want all 4 intact", got)
		}
	})
}

// TestPruneItemsBounds pins the inclusive-low/exclusive-high asymmetry the
// bounds inherit from ListFilter, and with it the fixed-width sqlTime layout
// SQLite compares as text.
func TestPruneItemsBounds(t *testing.T) {
	now := time.Now()
	cutoff := now.Add(-48 * time.Hour)

	tests := []struct {
		name       string
		filter     PruneFilter
		wantRemain []string
	}{
		{
			// c sits exactly on the cutoff; "< before" is exclusive, so it stays.
			name:       "before is exclusive at the boundary",
			filter:     PruneFilter{Before: cutoff},
			wantRemain: []string{"c", "d"},
		},
		{
			// c sits exactly on the cutoff; ">= after" is inclusive, so it goes.
			name:       "since is inclusive at the boundary",
			filter:     PruneFilter{After: cutoff},
			wantRemain: []string{"a", "b"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openTestStore(t)
			seedPrunable(t, db, now)
			ctx := context.Background()

			// Land c exactly on the cutoff and b one nanosecond older than it.
			for id, at := range map[string]time.Time{
				"c": cutoff,
				"b": cutoff.Add(-time.Nanosecond),
			} {
				if _, err := db.db.ExecContext(ctx,
					"UPDATE items SET created_at = ? WHERE id = ?", sqlTime(at), id); err != nil {
					t.Fatalf("backdate %s: %v", id, err)
				}
			}

			if _, err := db.PruneItems(ctx, tt.filter); err != nil {
				t.Fatalf("PruneItems: %v", err)
			}
			if got := remainingIDs(t, db); !slices.Equal(got, tt.wantRemain) {
				t.Errorf("remaining items = %v, want %v", got, tt.wantRemain)
			}
		})
	}
}

// TestPruneItemsPublishedAtWins covers the itemDate fallback: an item is judged
// on the date its feed published, and only on when we first saw it when the feed
// published no date at all.
func TestPruneItemsPublishedAtWins(t *testing.T) {
	now := time.Now()
	db := openTestStore(t)
	ctx := context.Background()

	items := []feeds.Item{
		// Ingested today, but published two years ago: old by item date.
		{ID: "backfilled", Source: "S1", Title: "Old", Link: "https://x/old", Published: now.AddDate(-2, 0, 0)},
		// No publish date, ingested 90 days ago: old by the created_at fallback.
		{ID: "undated", Source: "S1", Title: "Undated", Link: "https://x/undated"},
		// Published today: must survive.
		{ID: "fresh", Source: "S1", Title: "Fresh", Link: "https://x/fresh", Published: now},
	}
	if err := db.Record(ctx, items, nil, now); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if _, err := db.db.ExecContext(ctx,
		"UPDATE items SET created_at = ? WHERE id = 'undated'", sqlTime(now.AddDate(0, 0, -90))); err != nil {
		t.Fatalf("backdate undated: %v", err)
	}

	result, err := db.PruneItems(ctx, PruneFilter{Before: now.AddDate(0, 0, -30)})
	if err != nil {
		t.Fatalf("PruneItems: %v", err)
	}
	if result.Deleted != 2 {
		t.Errorf("PruneItems().Deleted = %d, want 2", result.Deleted)
	}
	if got := remainingIDs(t, db); !slices.Equal(got, []string{"fresh"}) {
		t.Errorf("remaining items = %v, want [fresh]", got)
	}
}

// TestPruneItemsKeepsSaved covers the default guard: anything you bookmarked,
// rated or annotated survives a prune that would otherwise take it, and is
// reported rather than silently skipped.
func TestPruneItemsKeepsSaved(t *testing.T) {
	now := time.Now()
	score := 8
	note := "worth revisiting"
	bookmark := true
	read := StatusRead

	tests := []struct {
		name  string
		patch UserPatch
		keeps bool
	}{
		{name: "bookmarked", patch: UserPatch{Bookmarked: &bookmark}, keeps: true},
		{name: "rated", patch: UserPatch{UserScore: &score}, keeps: true},
		{name: "noted", patch: UserPatch{UserNote: &note}, keeps: true},
		// Marking something read is the normal path through the feed, not a
		// signal to keep it — that is exactly what you want pruned.
		{name: "read is not saved state", patch: UserPatch{Status: &read}, keeps: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openTestStore(t)
			seedPrunable(t, db, now)
			ctx := context.Background()

			if err := db.UpdateUserState(ctx, "https://x/a", tt.patch); err != nil {
				t.Fatalf("UpdateUserState: %v", err)
			}

			result, err := db.PruneItems(ctx, PruneFilter{Source: "S1"})
			if err != nil {
				t.Fatalf("PruneItems: %v", err)
			}
			wantDeleted, wantKept, wantRemain := 1, 1, []string{"a", "c", "d"}
			if !tt.keeps {
				wantDeleted, wantKept, wantRemain = 2, 0, []string{"c", "d"}
			}
			if result.Deleted != wantDeleted || result.Kept != wantKept {
				t.Errorf("PruneItems() = {Deleted:%d Kept:%d}, want {Deleted:%d Kept:%d}",
					result.Deleted, result.Kept, wantDeleted, wantKept)
			}
			if got := remainingIDs(t, db); !slices.Equal(got, wantRemain) {
				t.Errorf("remaining items = %v, want %v", got, wantRemain)
			}

			if !tt.keeps {
				return
			}
			// --include-saved lifts the guard and takes the rest.
			result, err = db.PruneItems(ctx, PruneFilter{Source: "S1", IncludeSaved: true})
			if err != nil {
				t.Fatalf("PruneItems include-saved: %v", err)
			}
			if result.Deleted != 1 || result.Kept != 0 {
				t.Errorf("PruneItems(IncludeSaved) = {Deleted:%d Kept:%d}, want {Deleted:1 Kept:0}",
					result.Deleted, result.Kept)
			}
			if got := remainingIDs(t, db); !slices.Equal(got, []string{"c", "d"}) {
				t.Errorf("remaining items = %v, want [c d]", got)
			}
		})
	}
}

// TestPrunePreviewSample checks the preview caps and orders its rows, and that
// it never deletes anything.
func TestPrunePreviewSample(t *testing.T) {
	now := time.Now()
	db := openTestStore(t)
	seedPrunable(t, db, now)
	ctx := context.Background()

	preview, err := db.PrunePreview(ctx, PruneFilter{Before: now}, 2)
	if err != nil {
		t.Fatalf("PrunePreview: %v", err)
	}
	if preview.Deleted != 4 {
		t.Errorf("PrunePreview().Deleted = %d, want 4", preview.Deleted)
	}
	if len(preview.Sample) != 2 {
		t.Fatalf("len(PrunePreview().Sample) = %d, want 2", len(preview.Sample))
	}
	// Newest first: d (1 day) then c (2 days).
	if preview.Sample[0].ID != "d" || preview.Sample[1].ID != "c" {
		t.Errorf("sample ids = [%s %s], want [d c]", preview.Sample[0].ID, preview.Sample[1].ID)
	}
	if got := remainingIDs(t, db); len(got) != 4 {
		t.Errorf("items after preview = %v, want all 4 intact", got)
	}
}

// TestPruneItemsLeavesOtherTables is the guarantee the feature exists for: a
// prune reaches the feed and nothing else. Nothing references items, so the Maze
// board and the ingest history are untouched.
func TestPruneItemsLeavesOtherTables(t *testing.T) {
	now := time.Now()
	db := openTestStore(t)
	seedPrunable(t, db, now)
	ctx := context.Background()

	if _, err := db.AddTodo(ctx, "ship prune", "", nil, nil); err != nil {
		t.Fatalf("AddTodo: %v", err)
	}
	if _, err := db.AddIdea(ctx, "prune by tag next", ""); err != nil {
		t.Fatalf("AddIdea: %v", err)
	}

	if _, err := db.PruneItems(ctx, PruneFilter{Before: now}); err != nil {
		t.Fatalf("PruneItems: %v", err)
	}

	todos, err := db.ListTodos(ctx, TodoFilter{})
	if err != nil {
		t.Fatalf("ListTodos: %v", err)
	}
	if len(todos) != 1 {
		t.Errorf("todos after prune = %d, want 1", len(todos))
	}
	ideas, err := db.ListIdeas(ctx)
	if err != nil {
		t.Fatalf("ListIdeas: %v", err)
	}
	if len(ideas) != 1 {
		t.Errorf("ideas after prune = %d, want 1", len(ideas))
	}
}

// TestPruneItemsThenReRecord documents the cost of pruning inside the ingest
// window: the link comes back on the next run, unscored and stripped of the
// state you had put on it.
func TestPruneItemsThenReRecord(t *testing.T) {
	now := time.Now()
	db := openTestStore(t)
	ctx := context.Background()

	item := feeds.Item{ID: "a", Source: "S1", Title: "A", Link: "https://x/a"}
	if err := db.Record(ctx, []feeds.Item{item}, []DigestEntry{{Item: item, Score: 7, Reason: "ok"}}, now); err != nil {
		t.Fatalf("Record: %v", err)
	}
	scored, err := db.ScoredLinks(ctx, []string{"https://x/a"})
	if err != nil {
		t.Fatalf("ScoredLinks: %v", err)
	}
	if !scored["https://x/a"] {
		t.Fatal("ScoredLinks: want the recorded link reported as scored")
	}

	if _, err := db.PruneItems(ctx, PruneFilter{Source: "S1"}); err != nil {
		t.Fatalf("PruneItems: %v", err)
	}

	// Gone from the dedup set, so the next ingest fetches and re-scores it.
	scored, err = db.ScoredLinks(ctx, []string{"https://x/a"})
	if err != nil {
		t.Fatalf("ScoredLinks after prune: %v", err)
	}
	if scored["https://x/a"] {
		t.Error("ScoredLinks after prune: want the pruned link no longer scored")
	}

	if err := db.Record(ctx, []feeds.Item{item}, nil, now); err != nil {
		t.Fatalf("Record after prune: %v", err)
	}
	row, err := db.Get(ctx, "https://x/a")
	if err != nil {
		t.Fatalf("Get after re-record: %v", err)
	}
	if row.LLMScore != nil {
		t.Errorf("re-recorded item LLMScore = %d, want nil (rescored from scratch)", *row.LLMScore)
	}
}
