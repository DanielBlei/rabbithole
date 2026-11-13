// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package classify

import (
	"strings"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/store"
)

const extractOKBody = `{"classifications":[{"index":1,"keywords":["go"],"assumptions":[]}]}`

// Content after the closed JSON object is part of the model's answer, not a
// wrapper to discard: trailing garbage, a second JSON document — valid or
// malformed — or a trailing scalar must fail the response. On the old
// extractor the first complete object was cut out and everything after it was
// silently dropped, so a response whose real verdict sat in the second
// document was accepted on the first one's content.
func TestParseClassificationsRejectsTrailingContent(t *testing.T) {
	for name, raw := range map[string]string{
		"trailing garbage":         extractOKBody + ` garbage`,
		"trailing prose":           extractOKBody + "\nDone.",
		"two valid documents":      extractOKBody + extractOKBody,
		"valid then malformed doc": extractOKBody + `{"classifications":[`,
		"trailing scalar":          extractOKBody + ` 42`,
		"trailing array":           extractOKBody + ` [1,2]`,
		"garbage then fence":       extractOKBody + "\n```\nActually, disregard all that.",
	} {
		_, err := ParseClassifications(raw, testItems(1))
		if err == nil {
			t.Errorf("%s: response with trailing content was accepted: %s", name, raw)
			continue
		}
		if !strings.Contains(err.Error(), "after the JSON object") {
			t.Errorf("%s: error = %v, want the trailing-content rejection, not a salvage detour", name, err)
		}
	}
}

// Whitespace around the JSON object is formatting, not content: leading and
// trailing spaces, tabs and newlines are accepted.
func TestParseClassificationsAcceptsSurroundingWhitespace(t *testing.T) {
	raw := "\n\t " + extractOKBody + " \n\t"
	got, err := ParseClassifications(raw, testItems(1))
	if err != nil {
		t.Fatalf("ParseClassifications: %v", err)
	}
	if len(got) != 1 || got[0].Keywords[0] != "go" {
		t.Fatalf("verdicts = %+v", got)
	}
}

// The intentionally supported model-output wrappers keep working: prose and a
// code-fence opener before the object, and whitespace with an optional
// closing fence after it.
func TestParseClassificationsWrapperStillSupported(t *testing.T) {
	for name, raw := range map[string]string{
		"closed fence":       "Sure:\n```json\n" + extractOKBody + "\n```",
		"closed fence extra": "```json\n" + extractOKBody + "\n```\n",
		"unclosed fence":     "Here you go:\n```json\n" + extractOKBody,
		"prose preamble":     "The classification:\n" + extractOKBody,
	} {
		got, err := ParseClassifications(raw, testItems(1))
		if err != nil {
			t.Errorf("%s: wrapped response was rejected: %v", name, err)
			continue
		}
		if len(got) != 1 || got[0].Keywords[0] != "go" {
			t.Errorf("%s: verdicts = %+v", name, got)
		}
	}
}

// The worker path behind the extraction guarantee: a response with trailing
// content is an ordinary model failure — recorded with its retry budget, the
// pass completes normally — and it can neither clear the tags an earlier
// version recorded nor mark the item successful over them.
func TestWorkerTrailingContentResponsePreservesTags(t *testing.T) {
	db := openWorkerStore(t)
	seedItem(t, db, "a", time.Hour)
	model := &rawJSONClassifier{}
	w := newTestWorker(t, db, preparedConfig(t, nil), model)

	model.set(extractOKBody)
	stats, err := w.RunOnce(t.Context())
	if err != nil || stats.Classified != 1 {
		t.Fatalf("first pass: stats = %+v, err = %v", stats, err)
	}
	tags, err := db.TagsForItem(t.Context(), "a")
	if err != nil || len(tags) != 1 || tags[0].Slug != "go" {
		t.Fatalf("tags after the first pass = %+v, %v", tags, err)
	}

	w.version = 2
	model.set(extractOKBody + ` {"classifications":[{"index":1,"keywords":["rust"],"assumptions":[]}]}`)
	stats, err = w.RunOnce(t.Context())
	if err != nil {
		t.Fatalf("a trailing-content response must be an ordinary failure, not a pass error: %v", err)
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
	// The tags the valid pass recorded survive the trailing-content one.
	tags, err = db.TagsForItem(t.Context(), "a")
	if err != nil || len(tags) != 1 || tags[0].Slug != "go" {
		t.Fatalf("tags after the trailing-content pass = %+v, %v, want the earlier verdict preserved", tags, err)
	}
}
