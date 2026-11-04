// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/feeds"
)

func TestDigestItemsWindowOrderingAndLimit(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	start := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, 1)
	items := []feeds.Item{
		{ID: "high", Source: "S", Title: "High", Link: "https://x/high", Published: start.Add(time.Hour)},
		{ID: "tie-b", Source: "S", Title: "Tie B", Link: "https://x/tie-b", Published: start.Add(2 * time.Hour)},
		{ID: "tie-a", Source: "S", Title: "Tie A", Link: "https://x/tie-a", Published: start.Add(2 * time.Hour)},
		{ID: "first-seen", Source: "S", Title: "First seen", Link: "https://x/first-seen"},
		{ID: "at-start", Source: "S", Title: "At start", Link: "https://x/at-start", Published: start},
		{ID: "at-end", Source: "S", Title: "At end", Link: "https://x/at-end", Published: end},
		{ID: "unscored", Source: "S", Title: "Unscored", Link: "https://x/unscored", Published: start.Add(time.Hour)},
		{ID: "outside", Source: "S", Title: "Outside", Link: "https://x/outside", Published: start.Add(-time.Second)},
	}
	if err := db.Record(ctx, items, []DigestEntry{
		{Item: items[0], Score: 8, Reason: "high"},
		{Item: items[1], Score: 7},
		{Item: items[2], Score: 7},
		{Item: items[3], Score: 6, Reason: "fallback"},
		{Item: items[4], Score: 5},
		{Item: items[5], Score: 10},
		{Item: items[7], Score: 10},
	}, end); err != nil {
		t.Fatal(err)
	}
	firstSeen := start.Add(3 * time.Hour)
	if _, err := db.exec(ctx,
		"UPDATE items SET created_at = ? WHERE id = ?", sqlTime(firstSeen), "first-seen"); err != nil {
		t.Fatal(err)
	}
	userScore := 10
	if err := db.UpdateUserState(ctx, "high", UserPatch{UserScore: &userScore}); err != nil {
		t.Fatal(err)
	}

	got, err := db.DigestItems(ctx, start, end, 3)
	if err != nil {
		t.Fatalf("DigestItems: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d items, want SQL limit 3: %+v", len(got), got)
	}
	wantIDs := []string{"high", "tie-a", "tie-b"}
	for i, want := range wantIDs {
		if got[i].Item.ID != want {
			t.Errorf("item %d ID = %q, want %q", i, got[i].Item.ID, want)
		}
	}
	// The user rating of 10 must not outrank an LLM score of 8.
	if got[0].Score != 8 {
		t.Errorf("first score = %d, want LLM score 8 despite user rating 10", got[0].Score)
	}

	got, err = db.DigestItems(ctx, start, end, 50)
	if err != nil {
		t.Fatalf("DigestItems full limit: %v", err)
	}
	if len(got) != 5 || got[3].Item.ID != "first-seen" || got[4].Item.ID != "at-start" {
		t.Fatalf("window results = %+v, want five in-period scored items including start", got)
	}
	if !got[3].Item.Published.IsZero() {
		t.Errorf("missing publication date was not preserved: %s", got[3].Item.Published)
	}
	if !got[3].EffectiveDate.Equal(firstSeen) {
		t.Errorf("effective date = %s, want first-seen %s", got[3].EffectiveDate, firstSeen)
	}
}

func TestDigestItemsRejectsInvalidBoundsAndLimits(t *testing.T) {
	db := openTestStore(t)
	now := time.Now().UTC()
	for _, tc := range []struct {
		start, end time.Time
		limit      int
	}{
		{start: now, end: now, limit: 1},
		{start: now, end: now.Add(time.Hour), limit: 0},
		{start: now, end: now.Add(time.Hour), limit: MaxDigestItems + 1},
	} {
		if _, err := db.DigestItems(context.Background(), tc.start, tc.end, tc.limit); err == nil {
			t.Errorf("DigestItems(%s, %s, %d) error = nil", tc.start, tc.end, tc.limit)
		}
	}
}
