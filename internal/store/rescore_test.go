// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/feeds"
)

func TestRecentScoredItemsWindowAndLegacyProvenance(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	items := []feeds.Item{
		{
			ID: "recent", Source: "S", Title: "Recent", Link: "https://x/recent",
			Summary: "recent summary", Published: now.Add(-time.Hour),
		},
		{
			ID: "old", Source: "S", Title: "Old", Link: "https://x/old",
			Published: now.Add(-8 * 24 * time.Hour),
		},
		{
			ID: "unscored", Source: "S", Title: "Unscored", Link: "https://x/unscored",
			Published: now.Add(-time.Hour),
		},
		{ID: "undated", Source: "S", Title: "Undated", Link: "https://x/undated"},
	}
	if err := db.Record(ctx, items, []DigestEntry{
		{Item: items[0], Score: 8, Reason: "recent"},
		{Item: items[1], Score: 7, Reason: "old"},
		{Item: items[3], Score: 6, Reason: "legacy"},
	}, now); err != nil {
		t.Fatal(err)
	}
	// Simulate an old first-seen time for the undated row. Its NULL profile
	// provenance is intentional: legacy scored rows must still be candidates.
	if _, err := db.db.ExecContext(ctx,
		"UPDATE items SET created_at = ?, updated_at = ? WHERE id = ?",
		sqlTime(now.Add(-2*time.Hour)), sqlTime(now.Add(-2*time.Hour)), "undated",
	); err != nil {
		t.Fatal(err)
	}

	got, err := db.RecentScoredItems(ctx, now.Add(-7*24*time.Hour))
	if err != nil {
		t.Fatalf("RecentScoredItems: %v", err)
	}
	if len(got) != 2 || got[0].ID != "recent" || got[1].ID != "undated" {
		t.Fatalf("candidates = %+v, want recent then undated", got)
	}
	if got[0].Summary != "recent summary" {
		t.Errorf("stored summary lost: %+v", got[0])
	}
}

func TestReplaceItemScoresPreservesUserAndUnrelatedState(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	item := feeds.Item{
		ID: "a", Source: "S", Title: "A", Link: "https://x/a",
		Summary: "summary", Published: now.Add(-time.Hour), Tags: []string{"AI"},
	}
	if err := db.Record(ctx, []feeds.Item{item}, []DigestEntry{{
		Item: item, Score: 3, Reason: "old", Model: "old-model",
		ProfileID: "old-profile", ProfileName: "Old", ProfileHash: "old-hash",
		Digested: true,
	}}, now.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	status := StatusRead
	rating := 10
	note := "keep me"
	bookmark := true
	if err := db.UpdateUserState(ctx, item.ID, UserPatch{
		Status: &status, UserScore: &rating, UserNote: &note, Bookmarked: &bookmark,
	}); err != nil {
		t.Fatal(err)
	}
	var oldDigest string
	if err := db.db.QueryRowContext(ctx, "SELECT digested_on FROM items WHERE id = ?", item.ID).
		Scan(&oldDigest); err != nil {
		t.Fatal(err)
	}

	updated, err := db.ReplaceItemScores(ctx, []ScoreReplacement{{
		ItemID: item.ID, Score: 9, Reason: "new", Model: "new-model",
		ProfileID: "new-profile", ProfileName: "New", ProfileHash: "new-hash",
	}})
	if err != nil || updated != 1 {
		t.Fatalf("ReplaceItemScores = %d, %v", updated, err)
	}
	row, err := db.Get(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.LLMScore == nil || *row.LLMScore != 9 ||
		row.LLMScoreReason == nil || *row.LLMScoreReason != "new" ||
		row.LLMScoreModel == nil || *row.LLMScoreModel != "new-model" ||
		row.LLMProfileID == nil || *row.LLMProfileID != "new-profile" ||
		row.LLMProfileName == nil || *row.LLMProfileName != "New" ||
		row.LLMProfileHash == nil || *row.LLMProfileHash != "new-hash" {
		t.Fatalf("replacement score/provenance = %+v", row)
	}
	if row.Status != StatusRead || row.UserScore == nil || *row.UserScore != rating ||
		row.UserNote == nil || *row.UserNote != note || !row.Bookmarked ||
		len(row.Tags) != 1 || row.Tags[0] != "AI" {
		t.Fatalf("user/unrelated state changed: %+v", row)
	}
	var digest string
	if err := db.db.QueryRowContext(ctx, "SELECT digested_on FROM items WHERE id = ?", item.ID).
		Scan(&digest); err != nil {
		t.Fatal(err)
	}
	if digest != oldDigest {
		t.Errorf("digested_on changed from %q to %q", oldDigest, digest)
	}
}
