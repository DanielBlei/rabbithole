// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/feeds"
)

func briefFixture(itemID string) ItemBrief {
	score := 7
	feedScore := 4
	return ItemBrief{
		ItemID:         itemID,
		Status:         ItemBriefReady,
		Score:          &score,
		Recommendation: "read",
		Why:            "It matches the reader's interest in databases.",
		KeyPoints:      []string{"one", "two"},
		Novelty:        "new",
		Caveats:        "small sample",
		Focus:          "benchmarks",
		ReadingMinutes: 6,
		WordCount:      1100,
		Truncated:      true,
		ContentHash:    "hash",
		FinalURL:       "https://example.com/a",
		FeedScore:      &feedScore,
		ProfileID:      "p1",
		ProfileName:    "Reader",
		ProfileHash:    "ph",
		Provider:       "ollama",
		Model:          "llama:latest",
		CreatedAt:      time.Now().Add(-time.Hour),
	}
}

// recordBriefItems gives the briefs' foreign key real items to reference.
func recordBriefItems(t *testing.T, s *Store, ids ...string) {
	t.Helper()
	items := make([]feeds.Item, len(ids))
	for i, id := range ids {
		items[i] = feeds.Item{ID: id, Source: "S", Title: id, Link: "https://x/" + id}
	}
	if err := s.Record(context.Background(), items, nil, time.Now()); err != nil {
		t.Fatalf("Record items: %v", err)
	}
}

func TestItemBriefRoundTrip(t *testing.T) {
	s := openTestStore(t)
	recordBriefItems(t, s, "a")
	want := briefFixture("a")
	if err := s.SaveItemBrief(t.Context(), want); err != nil {
		t.Fatalf("SaveItemBrief: %v", err)
	}
	got, err := s.ItemBrief(t.Context(), "a")
	if err != nil {
		t.Fatalf("ItemBrief: %v", err)
	}
	if got.Status != ItemBriefReady || got.Score == nil || *got.Score != 7 ||
		got.FeedScore == nil || *got.FeedScore != 4 ||
		got.Recommendation != "read" || got.Why != want.Why ||
		len(got.KeyPoints) != 2 || got.KeyPoints[1] != "two" ||
		got.ReadingMinutes != 6 || got.WordCount != 1100 || !got.Truncated ||
		got.ContentHash != "hash" || got.FinalURL != want.FinalURL ||
		got.ProfileID != "p1" || got.ProfileName != "Reader" || got.ProfileHash != "ph" ||
		got.Provider != "ollama" || got.Model != "llama:latest" {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}

func TestItemBriefReplace(t *testing.T) {
	s := openTestStore(t)
	recordBriefItems(t, s, "a")
	first := briefFixture("a")
	if err := s.SaveItemBrief(t.Context(), first); err != nil {
		t.Fatalf("save first: %v", err)
	}
	second := briefFixture("a")
	second.Status = ItemBriefFailed
	second.ErrorCode = "timeout"
	second.Score = nil
	second.KeyPoints = nil
	if err := s.SaveItemBrief(t.Context(), second); err != nil {
		t.Fatalf("save second: %v", err)
	}
	got, err := s.ItemBrief(t.Context(), "a")
	if err != nil {
		t.Fatalf("ItemBrief: %v", err)
	}
	if got.Status != ItemBriefFailed || got.ErrorCode != "timeout" || got.Score != nil {
		t.Fatalf("replaced brief = %+v", got)
	}
	if got.KeyPoints == nil || len(got.KeyPoints) != 0 {
		t.Errorf("key points = %#v, want empty non-nil", got.KeyPoints)
	}
}

func TestItemBriefNotFound(t *testing.T) {
	s := openTestStore(t)
	_, err := s.ItemBrief(t.Context(), "missing")
	if !errors.Is(err, ErrItemBriefNotFound) {
		t.Fatalf("err = %v, want ErrItemBriefNotFound", err)
	}
}

// TestSaveItemBriefRequiresLiveItem pins the foreign key: a brief cannot exist
// without its item, which is what stops an in-flight dive from recreating one
// after a prune.
func TestSaveItemBriefRequiresLiveItem(t *testing.T) {
	s := openTestStore(t)
	err := s.SaveItemBrief(t.Context(), briefFixture("missing"))
	if err == nil {
		t.Fatal("SaveItemBrief accepted a brief for a missing item")
	}
	if _, err := s.ItemBrief(t.Context(), "missing"); !errors.Is(err, ErrItemBriefNotFound) {
		t.Fatalf("brief row = %v, want ErrItemBriefNotFound", err)
	}
}

// TestPruneThenInflightSaveCannotRecreateBrief is the prune-versus-save race:
// a dive that started before the prune saves after it commits. The write must
// fail, and re-ingesting the same stable ID must not expose the old brief.
func TestPruneThenInflightSaveCannotRecreateBrief(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	recordBriefItems(t, s, "a")
	if err := s.SaveItemBrief(ctx, briefFixture("a")); err != nil {
		t.Fatalf("SaveItemBrief before prune: %v", err)
	}
	if _, err := s.PruneItems(ctx, PruneFilter{All: true}); err != nil {
		t.Fatalf("PruneItems: %v", err)
	}

	if err := s.SaveItemBrief(ctx, briefFixture("a")); err == nil {
		t.Fatal("in-flight save recreated a brief for a pruned item")
	}
	if _, err := s.ItemBrief(ctx, "a"); !errors.Is(err, ErrItemBriefNotFound) {
		t.Fatalf("brief after in-flight save = %v, want ErrItemBriefNotFound", err)
	}

	// Re-ingesting the same stable ID is a new record; it must not inherit the
	// brief from the deleted one.
	recordBriefItems(t, s, "a")
	if _, err := s.ItemBrief(ctx, "a"); !errors.Is(err, ErrItemBriefNotFound) {
		t.Fatalf("re-ingested item exposed stale brief: %v", err)
	}
}

func TestItemBriefsByItemIDs(t *testing.T) {
	s := openTestStore(t)
	recordBriefItems(t, s, "a", "b", "c")
	for _, id := range []string{"a", "b", "c"} {
		if err := s.SaveItemBrief(t.Context(), briefFixture(id)); err != nil {
			t.Fatalf("save %s: %v", id, err)
		}
	}
	got, err := s.ItemBriefsByItemIDs(t.Context(), []string{"a", "c", "missing"})
	if err != nil {
		t.Fatalf("ItemBriefsByItemIDs: %v", err)
	}
	if len(got) != 2 || got["a"].ItemID != "a" || got["c"].ItemID != "c" {
		t.Fatalf("batch = %+v", got)
	}
	empty, err := s.ItemBriefsByItemIDs(t.Context(), nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty batch = %v, %v", empty, err)
	}
}

func TestPruneCascadesItemBriefs(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.Record(ctx, []feeds.Item{
		{ID: "a", Source: "S", Title: "A", Link: "https://x/a", Published: now},
		{ID: "b", Source: "S", Title: "B", Link: "https://x/b", Published: now},
	}, nil, now); err != nil {
		t.Fatalf("Record: %v", err)
	}
	for _, id := range []string{"a", "b"} {
		if err := s.SaveItemBrief(ctx, briefFixture(id)); err != nil {
			t.Fatalf("save brief %s: %v", id, err)
		}
	}
	keep := true
	if err := s.UpdateUserState(ctx, "b", UserPatch{Bookmarked: &keep}); err != nil {
		t.Fatalf("bookmark: %v", err)
	}
	if _, err := s.PruneItems(ctx, PruneFilter{All: true}); err != nil {
		t.Fatalf("PruneItems: %v", err)
	}
	if _, err := s.ItemBrief(ctx, "a"); !errors.Is(err, ErrItemBriefNotFound) {
		t.Errorf("brief for pruned item remains: %v", err)
	}
	if _, err := s.ItemBrief(ctx, "b"); err != nil {
		t.Errorf("brief for saved item was pruned: %v", err)
	}
}
