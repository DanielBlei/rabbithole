// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/feedback"
	"github.com/DanielBlei/rabbithole/internal/feeds"
)

func TestRecentFeedbackIsBoundedFilteredAndOrdered(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, time.October, 22, 12, 0, 0, 0, time.UTC)
	date := func(daysAgo int) time.Time { return now.Add(-time.Duration(daysAgo) * 24 * time.Hour) }

	type itemSpec struct {
		id        string
		score     *int
		published time.Time
		firstSeen time.Time
		summary   string
		note      *string
	}
	score := func(value int) *int { return &value }
	note := "keep this note"
	specs := []itemSpec{
		{id: "p-newest", score: score(10), published: date(1), firstSeen: date(10), note: &note},
		{id: "p-second", score: score(10), published: date(2), firstSeen: now, summary: "has summary"},
		{id: "p-undated-new", score: score(10), firstSeen: date(3)},
		{id: "p-undated-old", score: score(10), firstSeen: date(4)},
		{id: "p-tie-b", score: score(10), published: date(5), firstSeen: now},
		{id: "p-tie-a", score: score(10), published: date(5), firstSeen: now},
		{id: "p-oldest", score: score(10), published: date(6), firstSeen: now},
		{id: "n-newest", score: score(0), published: date(1), firstSeen: date(10)},
		{id: "n-second", score: score(0), published: date(2), firstSeen: now},
		{id: "n-undated-new", score: score(0), firstSeen: date(3)},
		{id: "n-undated-old", score: score(0), firstSeen: date(4)},
		{id: "n-tie-b", score: score(0), published: date(5), firstSeen: now},
		{id: "n-tie-a", score: score(0), published: date(5), firstSeen: now},
		{id: "n-oldest", score: score(0), published: date(6), firstSeen: now},
		{id: "intermediate-low", score: score(1), published: date(1), firstSeen: now},
		{id: "intermediate-high", score: score(9), published: date(1), firstSeen: now},
		{id: "unrated", published: date(1), firstSeen: now},
	}
	items := make([]feeds.Item, 0, len(specs))
	for _, spec := range specs {
		items = append(items, feeds.Item{
			ID: spec.id, Title: "Title " + spec.id, Source: "Source " + spec.id,
			Link: "https://example.test/" + spec.id, Summary: spec.summary,
			Published: spec.published,
		})
	}
	if err := db.Record(ctx, items, nil, now); err != nil {
		t.Fatalf("Record: %v", err)
	}
	for _, spec := range specs {
		if spec.firstSeen.IsZero() {
			continue
		}
		_, err := db.exec(ctx,
			"UPDATE items SET created_at = ? WHERE id = ?",
			sqlTime(spec.firstSeen), spec.id,
		)
		if err != nil {
			t.Fatalf("set first-seen date for %s: %v", spec.id, err)
		}
		if spec.score != nil {
			patch := UserPatch{UserScore: spec.score, UserNote: spec.note}
			if err := db.UpdateUserState(ctx, spec.id, patch); err != nil {
				t.Fatalf("rate %s: %v", spec.id, err)
			}
		}
	}

	got, err := db.RecentFeedback(ctx)
	if err != nil {
		t.Fatalf("RecentFeedback: %v", err)
	}
	selected := feedback.Select(got)
	wantPositive := []string{"p-newest", "p-second", "p-undated-new", "p-undated-old", "p-tie-a"}
	wantNegative := []string{"n-newest", "n-second", "n-undated-new", "n-undated-old", "n-tie-a"}
	if gotIDs := feedbackIDs(selected.Positive); !reflect.DeepEqual(gotIDs, wantPositive) {
		t.Errorf("positive IDs = %v, want %v", gotIDs, wantPositive)
	}
	if gotIDs := feedbackIDs(selected.Negative); !reflect.DeepEqual(gotIDs, wantNegative) {
		t.Errorf("negative IDs = %v, want %v", gotIDs, wantNegative)
	}
	if len(got) != 2*feedback.MaxExamples {
		t.Fatalf("RecentFeedback returned %d rows, want at most %d", len(got), 2*feedback.MaxExamples)
	}

	byID := make(map[string]feedback.Example, len(got))
	for _, example := range got {
		byID[example.ItemID] = example
	}
	newest := byID["p-newest"]
	if newest.Title != "Title p-newest" || newest.Source != "Source p-newest" || newest.Summary != "" {
		t.Errorf("stored item fields = %+v", newest)
	}
	if newest.UserNote == nil || *newest.UserNote != note {
		t.Errorf("user note = %v, want %q", newest.UserNote, note)
	}
	if newest.PublishedAt == nil || !newest.PublishedAt.Equal(date(1)) || !newest.FirstSeenAt.Equal(date(10)) {
		t.Errorf("dates = published %v, first seen %v", newest.PublishedAt, newest.FirstSeenAt)
	}
	if byID["p-second"].UserNote != nil {
		t.Errorf("missing note should remain nil; got %v", byID["p-second"].UserNote)
	}
}

func feedbackIDs(examples []feedback.Example) []string {
	ids := make([]string, len(examples))
	for i, example := range examples {
		ids[i] = example.ItemID
	}
	return ids
}
