// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeBenchmark drops body into a temp file and returns its path.
func writeBenchmark(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "golden.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

// validBody is a minimal well-formed benchmark: one control, one tagged.
const validBody = `
metadata:
  name: test
  version: 1
tags:
  keyword-bait: your vocabulary, no substance
samples:
  - id: rb001
    source: Source One
    title: A real write-up
    summary: |
      Measured, reproducible, honest about what failed.
    expected_llm_score: 9
    note: top tier
  - id: rb002
    source: Source Two
    title: Why AcmeCloud is the future
    expected_llm_score: 1
    tags: [keyword-bait]
    note: marketing
`

func TestLoad(t *testing.T) {
	ds, err := Load(writeBenchmark(t, validBody))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if ds.Metadata.Version != supportedVersion {
		t.Errorf("Version = %d, want %d", ds.Metadata.Version, supportedVersion)
	}
	if len(ds.Samples) != 2 {
		t.Fatalf("got %d samples, want 2", len(ds.Samples))
	}
	if got := ds.Samples[0].ExpectedScore(); got != 9 {
		t.Errorf("rb001 ExpectedScore() = %d, want 9", got)
	}
	if len(ds.Samples[0].Tags) != 0 {
		t.Errorf("rb001 Tags = %v, want none (control)", ds.Samples[0].Tags)
	}
	if got := ds.Samples[1].Tags; len(got) != 1 || got[0] != "keyword-bait" {
		t.Errorf("rb002 Tags = %v, want [keyword-bait]", got)
	}
	if got := ds.Tags["keyword-bait"]; got == "" {
		t.Error("tag description was not carried through")
	}
}

// A score of 0 is a legitimate verdict, so an omitted expected_llm_score must
// not be able to impersonate one. This is why the field is a pointer.
func TestLoadDistinguishesZeroFromMissing(t *testing.T) {
	zero := strings.Replace(validBody, "expected_llm_score: 9", "expected_llm_score: 0", 1)
	ds, err := Load(writeBenchmark(t, zero))
	if err != nil {
		t.Fatalf("explicit 0 should load: %v", err)
	}
	if got := ds.Samples[0].ExpectedScore(); got != 0 {
		t.Errorf("ExpectedScore() = %d, want 0", got)
	}

	missing := strings.Replace(validBody, "    expected_llm_score: 9\n", "", 1)
	if _, err := Load(writeBenchmark(t, missing)); err == nil {
		t.Fatal("omitted expected_llm_score should be an error, not a silent 0")
	}
}

func TestLoadRejects(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string // substring the error should name
	}{
		{
			name: "unknown top-level key",
			body: strings.Replace(validBody, "tags:", "tagz:", 1),
			want: "tagz",
		},
		{
			name: "unknown sample key",
			body: strings.Replace(validBody, "    note: top tier", "    reason: top tier", 1),
			want: "reason",
		},
		{
			name: "unsupported version",
			body: strings.Replace(validBody, "version: 1", "version: 2", 1),
			want: "version",
		},
		{
			name: "no samples",
			body: "metadata:\n  name: test\n  version: 1\nsamples: []\n",
			want: "no samples",
		},
		{
			name: "empty file",
			body: "",
			want: "empty",
		},
		{
			name: "missing id",
			body: strings.Replace(validBody, "  - id: rb001\n    source: Source One", "  - source: Source One", 1),
			want: "id is required",
		},
		{
			// yaml.v3 rejects these itself; asserted so the guarantee is not
			// silently lost if decoding ever changes.
			name: "duplicate key within a sample",
			body: strings.Replace(validBody, "    title: A real write-up", "    title: A\n    title: B", 1),
			want: "already defined",
		},
		{
			name: "duplicate id",
			body: strings.Replace(validBody, "- id: rb002", "- id: rb001", 1),
			want: "duplicate id",
		},
		{
			name: "missing title",
			body: strings.Replace(validBody, "    title: A real write-up\n", "", 1),
			want: "title is required",
		},
		{
			name: "score above the scale",
			body: strings.Replace(validBody, "expected_llm_score: 9", "expected_llm_score: 11", 1),
			want: "want 0-10",
		},
		{
			name: "score below the scale",
			body: strings.Replace(validBody, "expected_llm_score: 1", "expected_llm_score: -1", 1),
			want: "want 0-10",
		},
		{
			name: "tag not declared in the tags block",
			body: strings.Replace(validBody, "tags: [keyword-bait]", "tags: [keyword-bais]", 1),
			want: "not declared",
		},
		{
			name: "duplicate tag on one sample",
			body: strings.Replace(validBody, "tags: [keyword-bait]", "tags: [keyword-bait, keyword-bait]", 1),
			want: "duplicate tag",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeBenchmark(t, tt.body))
			if err == nil {
				t.Fatalf("Load() error = nil, want one naming %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Load() error = %q, want it to name %q", err, tt.want)
			}
		})
	}
}

func TestLoadStripsBOM(t *testing.T) {
	if _, err := Load(writeBenchmark(t, "\xEF\xBB\xBF"+validBody)); err != nil {
		t.Fatalf("Load() with a BOM error = %v", err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("Load() of a missing file error = nil, want non-nil")
	}
}

func TestItemsCarryOnlyWhatTheModelSees(t *testing.T) {
	ds, err := Load(writeBenchmark(t, validBody))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	items := ds.Items()
	if len(items) != len(ds.Samples) {
		t.Fatalf("got %d items, want %d", len(items), len(ds.Samples))
	}
	got := items[0]
	if got.ID != "rb001" || got.Source != "Source One" || got.Title != "A real write-up" {
		t.Errorf("item = %+v, want id/source/title carried", got)
	}
	if !strings.Contains(got.Summary, "reproducible") {
		t.Errorf("Summary = %q, want the sample's summary", got.Summary)
	}
	// The label must not reach the model: it is the answer being tested for.
	if !got.Published.IsZero() || got.Link != "" || got.Tags != nil {
		t.Errorf("item = %+v, want no fields beyond what BuildUserPrompt reads", got)
	}
}

// The shipped example is the file users copy, so a test parses it here to stop
// it drifting away from the loader, and checks the properties that make a
// fixture worth running rather than only the ones that make it parse.
func TestLoadShippedExample(t *testing.T) {
	const path = "../../configs/golden.example.yaml"
	ds, err := Load(path)
	if err != nil {
		t.Fatalf("Load(%s) error = %v", path, err)
	}

	// Below this the report prints its own volatility warning, so a shipped
	// example smaller than it demonstrates the caveat rather than the tool.
	if len(ds.Samples) < smallFixture {
		t.Errorf("shipped example has %d samples, want at least %d", len(ds.Samples), smallFixture)
	}

	used := make(map[Tag]int)
	byScore := make(map[int]int)
	for _, s := range ds.Samples {
		for _, tag := range s.Tags {
			used[tag]++
		}
		if s.Note == "" {
			t.Errorf("%s has no note; the report prints it beside a failure", s.ID)
		}
		byScore[s.ExpectedScore()]++
	}

	// Every score must appear. A fixture that skips values cannot tell a model
	// using the whole range from one collapsing it onto the few labels present,
	// which is the failure the score-spread warning exists to catch.
	for score := MinScore; score <= MaxScore; score++ {
		if byScore[score] == 0 {
			t.Errorf("no sample labelled %d; the fixture must exercise the whole scale", score)
		}
	}

	// Nothing here checks how many labels sit exactly on the high-signal mark.
	// Labels on it do make precision and recall jumpy, but the answer to that is
	// to say so in the report, not to push an author off the mark they believe:
	// a fixture is only worth running if every label is the one its author meant.
	var wanted int
	for _, s := range ds.Samples {
		if s.ExpectedScore() >= HighSignalThreshold {
			wanted++
		}
	}
	if wanted == 0 {
		t.Fatal("no sample reaches the high-signal mark; that section would measure nothing")
	}

	for tag := range ds.Tags {
		if used[tag] == 0 {
			t.Errorf("tag %q is declared but no sample uses it", tag)
		} else if used[tag] < MinTagSamples {
			t.Errorf("tag %q has %d samples, want at least %d; below that its MAE is one "+
				"article rather than a property of the group", tag, used[tag], MinTagSamples)
		}
	}
}
