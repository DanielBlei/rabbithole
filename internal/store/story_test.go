// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/feeds"
)

func TestStoryClusterRoundTripAndRebuildPreservesItems(t *testing.T) {
	db := openTestStore(t)
	ctx := t.Context()
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	blog := []feeds.Item{
		{ID: "a", Source: "S1", Title: "First item", Link: "https://example.com/a", Published: now},
		{ID: "b", Source: "S2", Title: "Second item", Link: "https://example.com/b", Published: now.Add(-time.Hour)},
		{ID: "c", Source: "S3", Title: "Third item", Link: "https://example.com/c", Published: now.Add(-2 * time.Hour)},
	}
	if err := db.Record(ctx, blog, nil, now); err != nil {
		t.Fatalf("Record: %v", err)
	}

	clusters := []StoryCluster{
		{
			ID: "story-1", RepresentativeItemID: "a", Method: "exact", Confidence: 1,
			Items: []StoryClusterItem{{ItemID: "a", Position: 0}, {ItemID: "b", Position: 1}},
		},
		{
			ID: "story-2", RepresentativeItemID: "c", Method: "verified", Confidence: 0.9,
			Items: []StoryClusterItem{{ItemID: "c", Position: 0}},
		},
	}
	if err := db.ReplaceStoryClusters(ctx, clusters); err != nil {
		t.Fatalf("ReplaceStoryClusters: %v", err)
	}

	got, err := db.StoryClusters(ctx)
	if err != nil {
		t.Fatalf("StoryClusters: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("clusters = %d, want 2", len(got))
	}
	if got[0].ID != "story-1" || got[0].Method != "exact" || len(got[0].Items) != 2 {
		t.Errorf("story-1 round trip = %+v", got[0])
	}
	if got[0].CreatedAt.IsZero() || got[0].UpdatedAt.IsZero() {
		t.Error("cluster timestamps were not stored")
	}
	if got[0].Items[1] != (StoryClusterItem{ItemID: "b", Position: 1}) {
		t.Errorf("member order did not round trip: %+v", got[0].Items)
	}

	byItem, err := db.StoryClustersForItems(ctx, []string{"b"})
	if err != nil {
		t.Fatalf("StoryClustersForItems: %v", err)
	}
	if len(byItem) != 1 || byItem[0].ID != "story-1" {
		t.Errorf("clusters for item b = %+v, want story-1", byItem)
	}

	before, err := db.Get(ctx, "a")
	if err != nil {
		t.Fatalf("Get before rebuild: %v", err)
	}
	rebuild := []StoryCluster{{
		ID: "story-3", RepresentativeItemID: "a", Method: "conservative", Confidence: 0.8,
		Items: []StoryClusterItem{{ItemID: "a", Position: 0}, {ItemID: "c", Position: 1}},
	}}
	if err := db.ReplaceStoryClusters(ctx, rebuild); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	after, err := db.Get(ctx, "a")
	if err != nil {
		t.Fatalf("Get after rebuild: %v", err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Errorf("rebuild changed item data:\n before %+v\n after  %+v", before, after)
	}
	got, err = db.StoryClusters(ctx)
	if err != nil {
		t.Fatalf("StoryClusters after rebuild: %v", err)
	}
	if len(got) != 1 || got[0].ID != "story-3" {
		t.Errorf("old clusters survived rebuild: %+v", got)
	}
}

func TestStoryClustersRejectInvalidMetadata(t *testing.T) {
	db := openTestStore(t)
	err := db.ReplaceStoryClusters(t.Context(), []StoryCluster{{
		ID: "story-bad", RepresentativeItemID: "missing",
		Items: []StoryClusterItem{{ItemID: "a", Position: 0}},
	}})
	if err == nil || !strings.Contains(err.Error(), "representative") {
		t.Fatalf("invalid cluster error = %v, want representative error", err)
	}
}
