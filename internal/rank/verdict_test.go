// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package rank

import (
	"context"
	"strings"
	"testing"

	"github.com/DanielBlei/rabbithole/internal/feeds"
)

func TestDegenerateReason(t *testing.T) {
	for _, tc := range []struct {
		reason string
		junk   bool
	}{
		// Signatures seen from a thinking model under guided decoding.
		{":", true},
		{":0" + strings.Repeat("}]", 40), true},
		{"}]} качество։ Wait, I must respond with valid JSON only.", true},
		{"", true},
		{`Covers it {"index":2`, true},
		{"Off topic.", false},
		{"本地推理", false},
		{"Directly about LLM serving on AMD GPUs, with benchmarks.", false},
		{"Mentions arrays like a[0] but nothing else.", false},
	} {
		if got := degenerateReason(tc.reason) != ""; got != tc.junk {
			t.Errorf("degenerateReason(%q) junk = %v, want %v", tc.reason, got, tc.junk)
		}
	}
}

func TestScoreAllReasksJunkVerdict(t *testing.T) {
	noBackoff(t)
	calls := 0
	scorer := &stubScorer{fn: func(batch []feeds.Item) ([]ItemScore, error) {
		calls++
		reason := ":"
		if calls == 2 {
			reason = "Relevant to the reader."
		}
		return []ItemScore{{ID: batch[0].ID, Score: 0, Reason: reason}}, nil
	}}
	got := ScoreAll(context.Background(), scorer, "profile", []feeds.Item{{ID: "a", Title: "A"}}, 1, 1)
	if got["a"].Reason != "Relevant to the reader." || calls != 2 {
		t.Fatalf("got %+v after %d calls, want the second, real verdict", got, calls)
	}
}

func TestScoreAllRetriesOnlyTheJunkBatchEntry(t *testing.T) {
	noBackoff(t)
	var calls [][]string
	scorer := &stubScorer{fn: func(batch []feeds.Item) ([]ItemScore, error) {
		var ids []string
		for _, it := range batch {
			ids = append(ids, it.ID)
		}
		calls = append(calls, ids)
		if len(batch) > 1 {
			return []ItemScore{
				{ID: "a", Score: 7, Reason: "Relevant to the reader."},
				{ID: "b", Score: 0, Reason: ":0}]}]"},
			}, nil
		}
		return []ItemScore{{ID: batch[0].ID, Score: 4, Reason: "Loosely related."}}, nil
	}}
	items := []feeds.Item{{ID: "a", Title: "A"}, {ID: "b", Title: "B"}}
	got := ScoreAll(context.Background(), scorer, "profile", items, 2, 1)
	if got["a"].Score != 7 || got["b"].Reason != "Loosely related." {
		t.Fatalf("got %+v, want a kept from the batch and b re-asked", got)
	}
	if len(calls) != 2 || len(calls[1]) != 1 || calls[1][0] != "b" {
		t.Fatalf("calls = %v, want the batch then b alone", calls)
	}
}
