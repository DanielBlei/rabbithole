package rank

import (
	"context"
	"testing"

	"github.com/DanielBlei/ai-searcher/internal/feeds"
)

func TestSelectFiltersAndSorts(t *testing.T) {
	items := []feeds.Item{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	scores := map[string]ItemScore{
		"a": {ID: "a", Score: 4},
		"b": {ID: "b", Score: 9},
		"c": {ID: "c", Score: 7},
	}
	got := Select(items, scores, 6, 10)
	if len(got) != 2 {
		t.Fatalf("got %d results, want 2 (min score 6)", len(got))
	}
	if got[0].Item.ID != "b" || got[1].Item.ID != "c" {
		t.Errorf("wrong order: %s, %s", got[0].Item.ID, got[1].Item.ID)
	}
}

func TestSelectRespectsTopN(t *testing.T) {
	items := []feeds.Item{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	scores := map[string]ItemScore{
		"a": {ID: "a", Score: 8},
		"b": {ID: "b", Score: 9},
		"c": {ID: "c", Score: 10},
	}
	got := Select(items, scores, 1, 2)
	if len(got) != 2 {
		t.Fatalf("got %d, want topN=2", len(got))
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
