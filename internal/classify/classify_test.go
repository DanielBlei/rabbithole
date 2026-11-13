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

// A response cut off mid-label must fail, not store the truncated prefix as a
// tag: the incomplete entry is discarded, which fails the one-entry-per-item
// check and sends the item back through the retry path.
func TestParseClassificationsRejectsTruncatedLabel(t *testing.T) {
	raw := `{"classifications":[{"index":1,"keywords":["go"],"assumptions":[]},{"index":2,"keywords":["rus`
	_, err := ParseClassifications(raw, testItems(2))
	if err == nil || !strings.Contains(err.Error(), "missing classification") {
		t.Fatalf("error = %v, want a missing-entry error for the truncated item", err)
	}
}

// The same rule holds when the cut lands mid-key or mid-array: a bracket the
// model never closed must not repair into a valid empty list.
func TestParseClassificationsRejectsStructuralTruncation(t *testing.T) {
	for name, raw := range map[string]string{
		"cut inside keywords array": `{"classifications":[{"index":1,"keywords":[`,
		"cut after keywords colon":  `{"classifications":[{"index":1,"keywords":`,
		"cut inside a key":          `{"classifications":[{"index":1,"keywords":["go"],"assum`,
	} {
		_, err := ParseClassifications(raw, testItems(1))
		if err == nil {
			t.Errorf("%s: truncated response was accepted: %s", name, raw)
		}
	}
	// With two items the complete first entry survives, and only the truncated
	// second one is reported missing.
	raw := `{"classifications":[{"index":1,"keywords":["go"],"assumptions":[]},{"index":2,"keywords":[`
	_, err := ParseClassifications(raw, testItems(2))
	if err == nil || !strings.Contains(err.Error(), "missing classification for item 2") {
		t.Fatalf("error = %v, want item 2 reported missing", err)
	}
}

// Entries that closed before the cut are still salvageable — the response
// fails only because the truncated tail leaves an item unclassified, and the
// error names that item so the caller can retry it alone.
func TestParseClassificationsSalvagesCompleteEntriesOnly(t *testing.T) {
	raw := `{"classifications":[{"index":1,"keywords":["go"],"assumptions":[]},{"index":2,"keywords":["rust`
	_, err := ParseClassifications(raw, testItems(2))
	if err == nil || !strings.Contains(err.Error(), "missing classification for item 2") {
		t.Fatalf("error = %v, want item 2 reported missing", err)
	}
}

// A `{` inside a label must not confuse the structural cut: the salvaged
// prefix still keeps the entry the label belongs to.
func TestParseClassificationsToleratesBraceInsideLabel(t *testing.T) {
	raw := `{"classifications":[{"index":1,"keywords":["a{b"],"assumptions":[]},{"index":2,"keywords":["rus`
	_, err := ParseClassifications(raw, testItems(2))
	if err == nil || !strings.Contains(err.Error(), "missing classification") {
		t.Fatalf("error = %v, want a missing-entry error", err)
	}
}

// Both list fields are required. An entry without them is malformed model
// output, not an empty classification: storing it would mark the item ok with
// fabricated-empty data the worker would never revisit.
func TestParseClassificationsRejectsMissingListFields(t *testing.T) {
	for name, raw := range map[string]string{
		"no lists at all":     `{"classifications":[{"index":1}]}`,
		"missing keywords":    `{"classifications":[{"index":1,"assumptions":["gpu"]}]}`,
		"missing assumptions": `{"classifications":[{"index":1,"keywords":["go"]}]}`,
		"missing index":       `{"classifications":[{"keywords":["go"],"assumptions":[]}]}`,
	} {
		if _, err := ParseClassifications(raw, testItems(1)); err == nil {
			t.Errorf("%s: entry with missing fields was accepted", name)
		}
	}
}

// Field values that are not usable lists are rejected, not silently emptied.
func TestParseClassificationsRejectsMalformedListValues(t *testing.T) {
	for name, raw := range map[string]string{
		"number":         `{"classifications":[{"index":1,"keywords":42,"assumptions":[]}]}`,
		"null":           `{"classifications":[{"index":1,"keywords":null,"assumptions":[]}]}`,
		"object":         `{"classifications":[{"index":1,"keywords":{},"assumptions":[]}]}`,
		"nested array":   `{"classifications":[{"index":1,"keywords":[["go"]],"assumptions":[]}]}`,
		"number element": `{"classifications":[{"index":1,"keywords":[42],"assumptions":[]}]}`,
		"bare element":   `{"classifications":[{"index":1,"keywords":[{}],"assumptions":[]}]}`,
		"bad index":      `{"classifications":[{"index":"one","keywords":["go"],"assumptions":[]}]}`,
	} {
		if _, err := ParseClassifications(raw, testItems(1)); err == nil {
			t.Errorf("%s: malformed list value was accepted", name)
		}
	}
}

// Explicitly empty arrays stay valid: "no keywords" is an answer the model may
// legitimately give, unlike a missing or malformed field.
func TestParseClassificationsAcceptsExplicitEmptyArrays(t *testing.T) {
	raw := `{"classifications":[{"index":1,"keywords":[],"assumptions":[]}]}`
	got, err := ParseClassifications(raw, testItems(1))
	if err != nil {
		t.Fatalf("ParseClassifications: %v", err)
	}
	if len(got[0].Keywords) != 0 || len(got[0].Assumptions) != 0 {
		t.Fatalf("verdict = %+v, want empty lists", got[0])
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
