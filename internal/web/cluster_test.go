// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/store"
)

func TestFeedGroupedToggleShowsRepresentativeAndMembers(t *testing.T) {
	w := newTestWeb(t)
	now := time.Now()
	if err := w.db.Record(context.Background(), []feeds.Item{
		{ID: "b", Source: "S2", Title: "B is the same development", Link: "https://x/b", Published: now},
		{ID: "c", Source: "S3", Title: "C is unrelated", Link: "https://x/c", Published: now.Add(-time.Hour)},
	}, nil, now); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := w.db.ReplaceStoryClusters(context.Background(), []store.StoryCluster{{
		ID: "story-1", RepresentativeItemID: "a", Method: "exact", Confidence: 1,
		Items: []store.StoryClusterItem{{ItemID: "a", Position: 0}, {ItemID: "b", Position: 1}},
	}}); err != nil {
		t.Fatalf("ReplaceStoryClusters: %v", err)
	}

	individual := get(t, w, "/feed")
	if strings.Contains(individual, "story-group") {
		t.Error("individual feed rendered grouped markup")
	}
	if !strings.Contains(individual, "/feed?grouped=1") {
		t.Error("individual feed has no grouped toggle")
	}

	grouped := get(t, w, "/feed?grouped=1")
	for _, want := range []string{
		`class="story-group"`,
		"1 related item on this story",
		`href="https://x/b"`,
		"B is the same development",
		"Show every item separately",
	} {
		if !strings.Contains(grouped, want) {
			t.Errorf("grouped feed missing %q", want)
		}
	}
	if strings.Count(grouped, "data-source=") != 2 {
		t.Errorf("grouped feed rendered %d rows, want 2 representatives", strings.Count(grouped, "data-source="))
	}
}

func TestFeedGroupedWithoutClustersStillRenders(t *testing.T) {
	w := newTestWeb(t)
	body := get(t, w, "/feed?grouped=1")
	if !strings.Contains(body, "story-group--single") {
		t.Errorf("grouped feed without clusters did not fall back to single rows: %s", body)
	}
}

func TestFeedGroupedFallsBackToExactDuplicates(t *testing.T) {
	w := newTestWeb(t)
	if err := w.db.Record(context.Background(), []feeds.Item{
		{ID: "b", Source: "S1", Title: "A", Link: "https://x/b"},
	}, nil, time.Now()); err != nil {
		t.Fatalf("Record: %v", err)
	}
	body := get(t, w, "/feed?grouped=1")
	if !strings.Contains(body, "1 related item on this story") {
		t.Errorf("grouped feed did not group exact duplicates: %s", body)
	}
}
