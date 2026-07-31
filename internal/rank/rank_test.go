// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package rank

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/DanielBlei/rabbithole/internal/feeds"
)

// stubScorer scores via fn, ignoring profile. Validate always succeeds.
type stubScorer struct {
	fn func(items []feeds.Item) ([]ItemScore, error)
}

func (s *stubScorer) Score(_ context.Context, _ string, items []feeds.Item) ([]ItemScore, error) {
	return s.fn(items)
}

func (s *stubScorer) Validate(context.Context) error { return nil }

func TestSelectKeepsScoredAndSorts(t *testing.T) {
	items := []feeds.Item{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}}
	scores := map[string]ItemScore{
		"a": {ID: "a", Score: 4},
		"b": {ID: "b", Score: 9},
		"c": {ID: "c", Score: 7},
		// "d" failed to score and must be dropped.
	}
	got := Select(items, scores)
	if len(got) != 3 {
		t.Fatalf("got %d results, want 3 (every scored item)", len(got))
	}
	if got[0].Item.ID != "b" || got[1].Item.ID != "c" || got[2].Item.ID != "a" {
		t.Errorf("wrong order: %s, %s, %s", got[0].Item.ID, got[1].Item.ID, got[2].Item.ID)
	}
}

func TestScoreAllRetriesItemsMissingFromPartialResponse(t *testing.T) {
	items := []feeds.Item{{ID: "a", Title: "a"}, {ID: "b", Title: "b"}, {ID: "c", Title: "c"}}
	var calls [][]string
	scorer := &stubScorer{fn: func(batch []feeds.Item) ([]ItemScore, error) {
		ids := make([]string, len(batch))
		for i, it := range batch {
			ids[i] = it.ID
		}
		calls = append(calls, ids)
		// Only ever score the first item of whatever batch is sent, mimicking a
		// small model that returns one verdict for a multi-item batch.
		first := batch[0]
		return []ItemScore{{ID: first.ID, Score: 5, Reason: "x"}}, nil
	}}

	got := ScoreAll(context.Background(), scorer, "profile", items, 3, 4)

	if len(got) != len(items) {
		t.Fatalf(
			"got %d scores, want %d (all items should eventually be scored): calls=%v",
			len(got),
			len(items),
			calls,
		)
	}
	for _, it := range items {
		if _, ok := got[it.ID]; !ok {
			t.Errorf("item %q missing from scores", it.ID)
		}
	}
}

func TestScoreBatchSkipsItemModelNeverScores(t *testing.T) {
	noBackoff(t)
	items := []feeds.Item{{ID: "a", Title: "a"}, {ID: "b", Title: "b"}}
	var aCalls int
	scorer := &stubScorer{fn: func(batch []feeds.Item) ([]ItemScore, error) {
		for _, it := range batch {
			if it.ID == "a" {
				aCalls++
				return nil, errors.New("model refuses to score item a")
			}
		}
		return []ItemScore{{ID: batch[0].ID, Score: 5}}, nil
	}}

	got := scoreBatch(context.Background(), scorer, "profile", items)

	if len(got) != 1 || got[0].ID != "b" {
		t.Fatalf("got %+v, want only item b scored", got)
	}
	// The batch call counts once, then scoreOne retries item a up to retryAttempts.
	if want := 1 + retryAttempts; aCalls != want {
		t.Errorf("item a scored %d times, want %d (1 batch + %d retries)", aCalls, want, retryAttempts)
	}
}

func TestScoreOneRetriesTransientFailure(t *testing.T) {
	noBackoff(t)
	item := feeds.Item{ID: "a", Title: "a"}
	var calls int
	scorer := &stubScorer{fn: func(batch []feeds.Item) ([]ItemScore, error) {
		calls++
		if calls < retryAttempts {
			return nil, errors.New("transient timeout")
		}
		return []ItemScore{{ID: batch[0].ID, Score: 7, Reason: "ok"}}, nil
	}}

	got := scoreOne(context.Background(), scorer, "profile", item)

	if len(got) != 1 || got[0].ID != "a" || got[0].Score != 7 {
		t.Fatalf("got %+v, want item a recovered with score 7", got)
	}
	if calls != retryAttempts {
		t.Errorf("scored %d times, want %d (recovered on final attempt)", calls, retryAttempts)
	}
}

// noBackoff zeros the per-item retry backoff for the duration of a test so
// retries don't sleep, restoring it afterward.
func noBackoff(t *testing.T) {
	t.Helper()
	prev := retryBackoff
	retryBackoff = 0
	t.Cleanup(func() { retryBackoff = prev })
}

func TestFailureVerbDistinguishesTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	<-ctx.Done()

	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "timeout", err: fmt.Errorf("ollama chat: %w", ctx.Err()), want: "scoring timed out"},
		{name: "generic error", err: errors.New("no valid scores mapped from response"), want: "scoring failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := failureVerb(tt.err); got != tt.want {
				t.Errorf("failureVerb() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHeuristicScoresOverlap(t *testing.T) {
	h := NewHeuristic()
	profile := "I care about vLLM inference, RAG retrieval and Kubernetes serving."
	items := []feeds.Item{
		{ID: "match", Title: "vLLM inference on Kubernetes", Summary: "RAG retrieval tips"},
		{ID: "none", Title: "Sourdough baking guide", Summary: "flour and water"},
	}
	scores, err := h.Score(context.Background(), profile, items)
	if err != nil {
		t.Fatalf("heuristic score: %v", err)
	}
	byID := map[string]ItemScore{}
	for _, s := range scores {
		byID[s.ID] = s
	}
	if byID["match"].Score == 0 {
		t.Error("expected non-zero score for overlapping item")
	}
	if byID["none"].Score != 0 {
		t.Errorf("expected zero score for unrelated item, got %d", byID["none"].Score)
	}
	if byID["match"].Score <= byID["none"].Score {
		t.Error("matching item should outscore unrelated item")
	}
}
