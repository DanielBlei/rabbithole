// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package classify

import (
	"strings"
	"testing"

	"github.com/DanielBlei/rabbithole/internal/feeds"
)

func testItems(n int) []feeds.Item {
	items := make([]feeds.Item, n)
	for i := range items {
		items[i] = feeds.Item{
			ID:      string(rune('a' + i)),
			Title:   "Title",
			Summary: "Summary",
		}
	}
	return items
}

func TestParseClassificationsCleanJSON(t *testing.T) {
	items := testItems(2)
	raw := `{"classifications":[
		{"index":1,"keywords":["vLLM","paged attention"],"assumptions":["inference"]},
		{"index":2,"keywords":["Go"],"assumptions":["runtime","go"]}
	]}`
	got, err := ParseClassifications(raw, items)
	if err != nil {
		t.Fatalf("ParseClassifications: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("verdicts = %+v", got)
	}
	if got[0].ID != "a" || got[0].Index != 1 || len(got[0].Keywords) != 2 {
		t.Errorf("first verdict = %+v", got[0])
	}
	if len(got[1].Assumptions) != 1 || got[1].Assumptions[0] != "runtime" {
		t.Errorf("keyword-wins dedupe failed: %+v", got[1].Assumptions)
	}
}

func TestParseClassificationsProseAndCodeFence(t *testing.T) {
	raw := "Sure:\n```json\n{\"classifications\":[{\"index\":1,\"keywords\":[\"go\"],\"assumptions\":[]}]}\n```"
	got, err := ParseClassifications(raw, testItems(1))
	if err != nil {
		t.Fatalf("ParseClassifications: %v", err)
	}
	if len(got) != 1 || got[0].Keywords[0] != "go" {
		t.Fatalf("verdicts = %+v", got)
	}
}

func TestParseClassificationsRepairsTruncatedTail(t *testing.T) {
	raw := `{"classifications":[{"index":1,"keywords":["go"],"assumptions":[]},{"index":2,"keywords":["rus`
	got, err := ParseClassifications(raw, testItems(2))
	if err != nil {
		t.Fatalf("ParseClassifications: %v", err)
	}
	if len(got) != 2 || got[0].Keywords[0] != "go" {
		t.Fatalf("verdicts = %+v", got)
	}
}

func TestParseClassificationsMissingEntryFails(t *testing.T) {
	raw := `{"classifications":[{"index":1,"keywords":["go"],"assumptions":[]}]}`
	if _, err := ParseClassifications(raw, testItems(2)); err == nil ||
		!strings.Contains(err.Error(), "missing classification") {
		t.Fatalf("error = %v, want a missing-entry error", err)
	}
}

func TestParseClassificationsSingleItemToleratesIndex(t *testing.T) {
	raw := `{"classifications":[{"index":7,"keywords":["go"],"assumptions":[]}]}`
	got, err := ParseClassifications(raw, testItems(1))
	if err != nil {
		t.Fatalf("ParseClassifications: %v", err)
	}
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("verdicts = %+v", got)
	}
}

func TestParseClassificationsTrimsOverCapLists(t *testing.T) {
	var keywords []string
	for i := range MaxKeywords + 3 {
		keywords = append(keywords, "kw"+string(rune('a'+i)))
	}
	raw := `{"classifications":[{"index":1,"keywords":["` +
		strings.Join(keywords, `","`) + `"],"assumptions":[]}]}`
	got, err := ParseClassifications(raw, testItems(1))
	if err != nil {
		t.Fatalf("ParseClassifications: %v", err)
	}
	if len(got[0].Keywords) != MaxKeywords {
		t.Errorf("keywords = %d, want %d", len(got[0].Keywords), MaxKeywords)
	}
}

func TestParseClassificationsLenientListShapes(t *testing.T) {
	raw := `{"classifications":[{"index":1,"keywords":"Go","assumptions":[{"label":"runtime"},{"name":"concurrency"}]}]}`
	got, err := ParseClassifications(raw, testItems(1))
	if err != nil {
		t.Fatalf("ParseClassifications: %v", err)
	}
	if len(got[0].Keywords) != 1 || got[0].Keywords[0] != "Go" {
		t.Errorf("keywords = %v", got[0].Keywords)
	}
	if len(got[0].Assumptions) != 2 {
		t.Errorf("assumptions = %v", got[0].Assumptions)
	}
}

func TestParseClassificationsRejectsDuplicateIndex(t *testing.T) {
	raw := `{"classifications":[
		{"index":1,"keywords":["go"],"assumptions":[]},
		{"index":1,"keywords":["rust"],"assumptions":[]}
	]}`
	if _, err := ParseClassifications(raw, testItems(2)); err == nil ||
		!strings.Contains(err.Error(), "duplicate classification") {
		t.Fatalf("error = %v, want a duplicate-index error", err)
	}
}

func TestParseClassificationsRejectsInvalidLabels(t *testing.T) {
	for _, label := range []string{"", "++", "___"} {
		raw := `{"classifications":[{"index":1,"keywords":["` + label + `"],"assumptions":[]}]}`
		if _, err := ParseClassifications(raw, testItems(1)); err == nil {
			t.Errorf("label %q was accepted", label)
		}
	}
}

func TestParseClassificationsRejectsOversizeLabel(t *testing.T) {
	raw := `{"classifications":[{"index":1,"keywords":["` +
		strings.Repeat("x", MaxLabelLength+1) + `"],"assumptions":[]}]}`
	if _, err := ParseClassifications(raw, testItems(1)); err == nil {
		t.Fatal("oversize label was accepted")
	}
}

func TestParseClassificationsMissingJSONFails(t *testing.T) {
	if _, err := ParseClassifications("no json here", testItems(1)); err == nil {
		t.Fatal("prose with no JSON was accepted")
	}
}

func TestBuildUserPromptIncludesVocabularyHint(t *testing.T) {
	prompt := BuildUserPrompt(testItems(1), []string{"vllm", "kubernetes"})
	if !strings.Contains(prompt, "Vocabulary hints") ||
		!strings.Contains(prompt, "- vllm") || !strings.Contains(prompt, "- kubernetes") {
		t.Fatalf("prompt missing vocabulary hint:\n%s", prompt)
	}
	plain := BuildUserPrompt(testItems(1), nil)
	if strings.Contains(plain, "Vocabulary hints") {
		t.Fatalf("hint appeared without vocabulary:\n%s", plain)
	}
}

func TestSchemaCarriesCaps(t *testing.T) {
	schema := Schema()
	for _, want := range []string{`"maxItems": 8`, `"maxItems": 5`, `"maxLength": 64`} {
		if !strings.Contains(schema, want) {
			t.Errorf("schema missing %s:\n%s", want, schema)
		}
	}
}

func TestHeuristicReturnsEmptyVerdicts(t *testing.T) {
	got, err := NewHeuristic().Classify(t.Context(), testItems(2))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || len(got[0].Keywords) != 0 || got[0].ID != "a" {
		t.Fatalf("verdicts = %+v", got)
	}
}
