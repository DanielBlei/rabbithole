package rank

import (
	"testing"

	"github.com/DanielBlei/ai-searcher/internal/feeds"
)

func testItems() []feeds.Item {
	return []feeds.Item{
		{ID: "a", Title: "Scaling vLLM"},
		{ID: "b", Title: "Intro to Python"},
	}
}

func TestParseScoresPlainJSON(t *testing.T) {
	raw := `{"scores":[{"index":1,"score":9,"reason":"deep inference work"},{"index":2,"score":2,"reason":"beginner"}]}`
	got, err := ParseScores(raw, testItems())
	if err != nil {
		t.Fatalf("ParseScores: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d scores, want 2", len(got))
	}
	byID := map[string]ItemScore{}
	for _, s := range got {
		byID[s.ID] = s
	}
	if byID["a"].Score != 9 || byID["a"].Reason != "deep inference work" {
		t.Errorf("item a = %+v", byID["a"])
	}
	if byID["b"].Score != 2 {
		t.Errorf("item b score = %d, want 2", byID["b"].Score)
	}
}

func TestParseScoresWithFencesAndProse(t *testing.T) {
	raw := "Sure! Here are the scores:\n```json\n{\"scores\":[{\"index\":1,\"score\":12,\"reason\":\"x\"}]}\n```\nHope that helps."
	got, err := ParseScores(raw, testItems())
	if err != nil {
		t.Fatalf("ParseScores: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d scores, want 1", len(got))
	}
	if got[0].Score != 10 {
		t.Errorf("score = %d, want clamped to 10", got[0].Score)
	}
}

func TestParseScoresIgnoresOutOfRangeIndex(t *testing.T) {
	raw := `{"scores":[{"index":99,"score":5,"reason":"x"},{"index":1,"score":7,"reason":"y"}]}`
	got, err := ParseScores(raw, testItems())
	if err != nil {
		t.Fatalf("ParseScores: %v", err)
	}
	if len(got) != 1 || got[0].ID != "a" {
		t.Errorf("expected only valid index mapped, got %+v", got)
	}
}

func TestParseScoresErrorsOnGarbage(t *testing.T) {
	if _, err := ParseScores("no json here", testItems()); err == nil {
		t.Error("expected error for response with no JSON")
	}
}
