// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package rank

import (
	"context"
	"sync"
	"testing"

	"github.com/DanielBlei/rabbithole/internal/feeds"
)

// ScoreGroups chunks each group separately, so a batch must never contain
// items from two groups even though all batches share one pool.
func TestScoreGroupsKeepsBatchesWithinGroups(t *testing.T) {
	noBackoff(t)
	groups := [][]feeds.Item{
		{
			{ID: "a1", Source: "A", Title: "a1"},
			{ID: "a2", Source: "A", Title: "a2"},
			{ID: "a3", Source: "A", Title: "a3"},
		},
		{
			{ID: "b1", Source: "B", Title: "b1"},
		},
	}
	var (
		mu    sync.Mutex
		spans int
	)
	scorer := &stubScorer{fn: func(items []feeds.Item) ([]ItemScore, error) {
		source := items[0].Source
		for _, item := range items {
			if item.Source != source {
				mu.Lock()
				spans++
				mu.Unlock()
			}
		}
		scores := make([]ItemScore, 0, len(items))
		for _, item := range items {
			scores = append(scores, ItemScore{ID: item.ID, Score: 6, Reason: "Relevant to the reader."})
		}
		return scores, nil
	}}

	scores := ScoreGroups(context.Background(), scorer, "profile", groups, 2, 2)
	if len(scores) != 4 {
		t.Fatalf("scores = %d, want 4", len(scores))
	}
	if spans != 0 {
		t.Errorf("%d batches mixed items from different groups", spans)
	}
}

// The richer callback reports a monotonic total, per-group completion, and
// pool occupancy without changing the older OnBatch counts.
func TestScoreGroupsReportsProgressAndPoolFields(t *testing.T) {
	noBackoff(t)
	groups := [][]feeds.Item{
		{{ID: "a1", Source: "A"}, {ID: "a2", Source: "A"}, {ID: "a3", Source: "A"}},
		{{ID: "b1", Source: "B"}},
	}
	scorer := &stubScorer{fn: func(items []feeds.Item) ([]ItemScore, error) {
		scores := make([]ItemScore, 0, len(items))
		for _, item := range items {
			scores = append(scores, ItemScore{ID: item.ID, Score: 6, Reason: "Relevant to the reader."})
		}
		return scores, nil
	}}

	var (
		progress []BatchProgress
		legacy   []int
	)
	ScoreGroups(context.Background(), scorer, "profile", groups, 2, 2,
		OnBatch(func(done, total, _ int) { legacy = append(legacy, done) }),
		OnBatchProgress(func(p BatchProgress) { progress = append(progress, p) }),
	)

	if len(progress) != 3 || len(legacy) != 3 {
		t.Fatalf("callbacks = %d progress / %d legacy, want 3 each", len(progress), len(legacy))
	}
	last := progress[len(progress)-1]
	if last.Done != 3 || last.Total != 3 {
		t.Errorf("final progress done/total = %d/%d, want 3/3", last.Done, last.Total)
	}
	// Completion order is not guaranteed, so check each group's final state.
	groupFinal := map[int]BatchProgress{}
	for _, p := range progress {
		groupFinal[p.Group] = p
	}
	if got := groupFinal[0]; got.GroupTotal != 2 || got.GroupDone != 2 {
		t.Errorf("group 0 final progress = %d/%d, want 2/2", got.GroupDone, got.GroupTotal)
	}
	if got := groupFinal[1]; got.GroupTotal != 1 || got.GroupDone != 1 {
		t.Errorf("group 1 final progress = %d/%d, want 1/1", got.GroupDone, got.GroupTotal)
	}
	for i, p := range progress {
		if p.Done != i+1 {
			t.Errorf("progress %d done = %d, want %d", i, p.Done, i+1)
		}
		if p.Peak < p.InFlight || p.InFlight < 0 {
			t.Errorf("progress %d in_flight/peak = %d/%d, want 0 <= in_flight <= peak", i, p.InFlight, p.Peak)
		}
		if p.Peak > 2 {
			t.Errorf("progress %d peak = %d, want <= 2", i, p.Peak)
		}
	}
}
