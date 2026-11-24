// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package classify

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// A complete, balanced response is validated strictly: an invalid value in a
// list field fails the whole response with the validation error. It must never
// reach the truncation salvage, whose bracket-closing repair could rewrite the
// invalid member into a valid empty array — the exact shape the review
// reproduced, where `assumptions:[{}]` was accepted as "no assumptions", the
// item's tags were cleared, and the item was marked ok. On the old parser each
// of these payloads was either accepted outright or emptied by the repair.
func TestParseClassificationsCompleteInvalidNotRewritten(t *testing.T) {
	for name, raw := range map[string]string{
		"invalid object in final assumptions": `{"classifications":[{"index":1,"keywords":[],"assumptions":[{}]}]}`,
		"invalid object in final keywords":    `{"classifications":[{"index":1,"keywords":[{}],"assumptions":[]}]}`,
		"invalid member after a valid one":    `{"classifications":[{"index":1,"keywords":[],"assumptions":[{"label":"gpu"},{}]}]}`,
		"nested array member":                 `{"classifications":[{"index":1,"keywords":[],"assumptions":[[{}]]}]}`,
		"number member in final list":         `{"classifications":[{"index":1,"keywords":[],"assumptions":[7]}]}`,
		"object without a label field":        `{"classifications":[{"index":1,"keywords":[],"assumptions":[{"note":"x"}]}]}`,
	} {
		_, err := ParseClassifications(raw, testItems(1))
		if err == nil {
			t.Errorf("%s: complete response with an invalid list member was accepted: %s", name, raw)
			continue
		}
		// The validation error itself must surface: a salvage detour would
		// report something else (or nothing) after rewriting the value.
		if !strings.Contains(err.Error(), "is not a list of labels") {
			t.Errorf("%s: error = %v, want the list validation error, not a salvage rewrite", name, err)
		}
	}
}

// Completeness detection is string- and escape-aware: braces, brackets and
// escaped quotes inside labels must not make a complete response look
// truncated (which would send it through the rewriting salvage), nor hide an
// invalid final member in a response that is genuinely complete.
func TestParseClassificationsEscapedBracesDoNotFooCompleteness(t *testing.T) {
	valid := `{"classifications":[{"index":1,"keywords":["a {b} \"q\" \\ z"],"assumptions":["c {d} \"e\""]}]}`
	got, err := ParseClassifications(valid, testItems(1))
	if err != nil {
		t.Fatalf("complete response with braces and escapes in labels was rejected: %v", err)
	}
	if len(got[0].Keywords) != 1 || len(got[0].Assumptions) != 1 {
		t.Fatalf("verdict = %+v, want both labels kept", got[0])
	}

	invalid := `{"classifications":[{"index":1,"keywords":["a {b} \"q\" \\ z"],"assumptions":[{}]}]}`
	_, err = ParseClassifications(invalid, testItems(1))
	if err == nil || !strings.Contains(err.Error(), "assumptions is not a list of labels") {
		t.Fatalf("error = %v, want the validation error for the invalid final member", err)
	}
}

// In a genuinely truncated response, an entry that closed but holds an invalid
// list member is discarded whole — at an entry boundary — never emptied and
// accepted, and a member cut mid-label-object cannot be invented into a
// verdict. The response fails either way and the item is retried.
func TestParseClassificationsTruncatedInvalidEntryDroppedWhole(t *testing.T) {
	for name, raw := range map[string]string{
		"invalid entry before the cut": `{"classifications":[{"index":1,"keywords":["go"],"assumptions":[{}]},{"index":2,"keywords":["rus`,
		"cut inside a label object":    `{"classifications":[{"index":1,"keywords":["go"],"assumptions":[{"label":"kube`,
		"cut inside a nested member":   `{"classifications":[{"index":1,"keywords":["go"],"assumptions":[[{"label":"x"`,
	} {
		if _, err := ParseClassifications(raw, testItems(2)); err == nil {
			t.Errorf("%s: truncated response with an invalid entry was accepted: %s", name, raw)
		}
	}
	// The discriminating case for the entry-boundary cut: a single item whose
	// closed entry is invalid. The old parser cut inside the entry, closed
	// the list empty and accepted the verdict; the entry must now be dropped
	// whole, leaving no classifications at all.
	single := `{"classifications":[{"index":1,"keywords":["go"],"assumptions":[{}]}`
	if _, err := ParseClassifications(single, testItems(1)); err == nil {
		t.Fatal("truncated response whose only closed entry is invalid was accepted")
	}
	// The control: a truncated response whose closed entries are valid still
	// salvages them, and fails only on the item the cut took away.
	salvageable := `{"classifications":[{"index":1,"keywords":["go"],"assumptions":[]},{"index":2,"keywords":["rus`
	_, err := ParseClassifications(salvageable, testItems(2))
	if err == nil || !strings.Contains(err.Error(), "missing classification for item 2") {
		t.Fatalf("error = %v, want item 2 reported missing while item 1 is salvaged", err)
	}
}

// rawJSONClassifier answers through the real parser, the way an inference
// backend turns a model response into verdicts.
type rawJSONClassifier struct {
	mu  sync.Mutex
	raw string
}

func (c *rawJSONClassifier) set(raw string) {
	c.mu.Lock()
	c.raw = raw
	c.mu.Unlock()
}

func (c *rawJSONClassifier) Validate(context.Context) error { return nil }

func (c *rawJSONClassifier) Classify(_ context.Context, items []feeds.Item) ([]ItemVerdict, error) {
	c.mu.Lock()
	raw := c.raw
	c.mu.Unlock()
	return ParseClassifications(raw, items)
}

// The worker path behind the parser guarantee: a complete-but-malformed model
// response is an ordinary model failure — recorded with its retry budget, the
// pass and the command complete normally — and it can never clear the tags a
// previous version recorded nor mark the item successful over them.
func TestWorkerInvalidCompleteResponsePreservesTags(t *testing.T) {
	db := openWorkerStore(t)
	seedItem(t, db, "a", time.Hour)
	model := &rawJSONClassifier{}
	w := newTestWorker(t, db, preparedConfig(t, nil), model)

	model.set(`{"classifications":[{"index":1,"keywords":["go"],"assumptions":[]}]}`)
	stats, err := w.RunOnce(t.Context())
	if err != nil || stats.Classified != 1 {
		t.Fatalf("first pass: stats = %+v, err = %v", stats, err)
	}
	tags, err := db.TagsForItem(t.Context(), "a")
	if err != nil || len(tags) != 1 || tags[0].Slug != "go" {
		t.Fatalf("tags after the first pass = %+v, %v", tags, err)
	}

	// A new classifier version re-opens the item; the model answers with the
	// complete-but-malformed payload from the review.
	w.version = 2
	model.set(`{"classifications":[{"index":1,"keywords":[],"assumptions":[{}]}]}`)
	stats, err = w.RunOnce(t.Context())
	if err != nil {
		t.Fatalf("a malformed model response must be an ordinary failure, not a pass error: %v", err)
	}
	if stats.Classified != 0 || stats.Failed != 1 {
		t.Fatalf("stats = %+v, want one ordinary model failure", stats)
	}
	state, ok, err := db.ClassificationState(t.Context(), "a")
	if err != nil || !ok {
		t.Fatalf("state = %+v, %v, %v", state, ok, err)
	}
	if state.ClassifierVersion != 2 || state.Attempts != 1 || state.Status != store.ClassificationFailed {
		t.Fatalf("state = %+v, want a v2 failure with one attempt spent", state)
	}
	if state.NextAttemptAt == nil || !state.NextAttemptAt.After(time.Now()) {
		t.Errorf("next attempt = %v, want a future retry", state.NextAttemptAt)
	}
	// The tags the valid pass recorded survive the malformed one untouched.
	tags, err = db.TagsForItem(t.Context(), "a")
	if err != nil || len(tags) != 1 || tags[0].Slug != "go" {
		t.Fatalf("tags after the malformed pass = %+v, %v, want the earlier verdict preserved", tags, err)
	}
}
