// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// update rewrites the golden files instead of comparing against them:
// go test ./internal/eval -update
var update = flag.Bool("update", false, "rewrite the golden report files")

// fixedReport is a report with every volatile field pinned, so the golden files
// only change when the rendering does.
func fixedReport(t *testing.T) *Report {
	t.Helper()
	outcomes := []Outcome{
		scored("rb001", 9, 4, "on-target"),
		scored("rb002", 7, 6, "relevant"),
		scored("rb003", 1, 2, "keyword-bait"),
		scored("rb004", 4, 4, "shallow"),
		unscored("rb005", 8, "on-target"),
	}
	outcomes[0].Reason = "Covers local model serving in depth, with numbers."
	outcomes[0].Sample.Title = "Running Qwen3-30B locally with llama.cpp on a single 3090"
	outcomes[0].Sample.Note = "Two profile lines at once: open-source models and practical write-ups."
	outcomes[2].Sample.Title = "10 AI tools that will completely change how you work"

	rep := Summarize(&Results{
		Runs:       [][]Outcome{outcomes, outcomes},
		RunSeconds: []float64{31.5, 30.25},
		Info: RunInfo{
			Benchmark:      "rabbithole-relevance",
			StartedAt:      time.Date(2026, 8, 15, 21, 4, 0, 0, time.UTC),
			Provider:       "ollama",
			Model:          "qwen3.5:4b",
			Think:          true,
			BatchSize:      8,
			MaxParallel:    2,
			Repeats:        2,
			ProfileHash:    "sha256:4f2a9c1b8e3d0000000000000000000000000000000000000000000000000000",
			PromptHash:     "sha256:9a1c33bd0e120000000000000000000000000000000000000000000000000000",
			DatasetHash:    "sha256:71b2c9ff41aa0000000000000000000000000000000000000000000000000000",
			ElapsedSeconds: 61.75,
			DatasetSamples: 12,
			Limit:          5,
		},
	}, &Dataset{Tags: map[Tag]string{
		"on-target":    "what you most want to read",
		"relevant":     "worth reading, not a highlight",
		"keyword-bait": "no substance",
		"shallow":      "maybe not a match, but honest about it",
	}})
	return rep
}

func render(t *testing.T, rep *Report, opt RenderOptions) string {
	t.Helper()
	var b strings.Builder
	if err := rep.Write(&b, opt); err != nil {
		t.Fatalf("Write(%q) error = %v", opt.Format, err)
	}
	return b.String()
}

// TestGoldenReports pins the exact bytes of each renderer. Substring assertions
// cannot catch a ragged column, and the alignment is the point of the text
// report, so the whole output is the fixture.
func TestGoldenReports(t *testing.T) {
	tests := []struct {
		name string
		file string
		opt  RenderOptions
	}{
		{name: "text", file: "report.text.golden", opt: RenderOptions{Format: FormatText}},
		{name: "text with why", file: "report.why.text.golden", opt: RenderOptions{Format: FormatText, ShowWhy: true}},
		{name: "markdown", file: "report.md.golden", opt: RenderOptions{Format: FormatMarkdown}},
		{name: "json", file: "report.json.golden", opt: RenderOptions{Format: FormatJSON}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := render(t, fixedReport(t), tt.opt)
			path := filepath.Join("testdata", tt.file)

			if *update {
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatalf("write golden: %v", err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden (run with -update to create it): %v", err)
			}
			if got != string(want) {
				t.Errorf("%s does not match %s; run with -update to accept\n--- got ---\n%s", tt.name, path, got)
			}
		})
	}
}

// TestTextFitsTheWidth is what turns the column budgets in text.go into a
// constraint. The fixture uses values longer than anything the golden file has,
// because the caps only matter when something overflows them.
func TestTextFitsTheWidth(t *testing.T) {
	longTitle := strings.Repeat("a very long article title that keeps going ", 4)
	outcomes := []Outcome{
		scored("an-unusually-long-sample-identifier", 9, 2, "a-very-long-tag-name-indeed"),
		scored("rb002", 3, 3, "a-very-long-tag-name-indeed"),
		unscored("rb003", 10, "a-very-long-tag-name-indeed"),
	}
	outcomes[0].Sample.Title = longTitle
	outcomes[0].Reason = strings.Repeat("the model explained itself at length ", 6)
	outcomes[0].Sample.Note = strings.Repeat("and so did the golden note ", 8)

	rep := Summarize(&Results{
		Runs:       [][]Outcome{outcomes, outcomes},
		RunSeconds: []float64{1, 2},
		Info: RunInfo{
			Benchmark:      strings.Repeat("a-long-benchmark-name-", 5),
			Model:          strings.Repeat("a-long-model-name-", 5),
			Provider:       "ollama",
			Repeats:        2,
			ElapsedSeconds: 3,
			DatasetSamples: 3,
		},
	}, &Dataset{Tags: map[Tag]string{
		"a-very-long-tag-name-indeed": strings.Repeat("and a long description too ", 4),
	}})

	got := render(t, rep, RenderOptions{Format: FormatText, ShowWhy: true})
	for i, line := range strings.Split(got, "\n") {
		if n := utf8.RuneCountInString(line); n > textWidth {
			t.Errorf("line %d is %d columns, want at most %d:\n%s", i+1, n, textWidth, line)
		}
	}
}

func TestTextCarriesNoMarkup(t *testing.T) {
	rep := fixedReport(t)
	// A constant scorer is the case whose warning wording came from the
	// Markdown renderer, so it is the one most likely to leak styling.
	constant := Summarize(&Results{Runs: [][]Outcome{constantScorer()}}, &Dataset{})

	for _, tt := range []struct {
		name string
		rep  *Report
	}{
		{name: "clean run", rep: rep},
		{name: "constant scorer warning", rep: constant},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := render(t, tt.rep, RenderOptions{Format: FormatText, ShowWhy: true})
			if strings.Contains(got, "**") {
				t.Error("text output carries Markdown emphasis")
			}
			for _, line := range strings.Split(got, "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "|") {
					t.Errorf("text output carries a Markdown table row: %q", line)
				}
				if strings.HasPrefix(strings.TrimSpace(line), "> ") {
					t.Errorf("text output carries a Markdown blockquote: %q", line)
				}
			}
		})
	}
}

func TestShowWhyControlsTheReasoning(t *testing.T) {
	for _, format := range []Format{FormatText, FormatMarkdown} {
		t.Run(string(format), func(t *testing.T) {
			rep := fixedReport(t)
			off := render(t, rep, RenderOptions{Format: format})
			on := render(t, rep, RenderOptions{Format: format, ShowWhy: true})

			const reason = "Covers local model serving in depth"
			if strings.Contains(off, reason) {
				t.Error("the reason is printed without --show-why")
			}
			if !strings.Contains(on, reason) {
				t.Error("the reason is missing with --show-why")
			}
			// Every sample is listed either way; only the reasoning is optional.
			for _, id := range []string{"rb001", "rb004", "rb005"} {
				if !strings.Contains(off, id) {
					t.Errorf("sample %s missing from the default report", id)
				}
			}
		})
	}
}

// A run that only covers part of the golden file must not read like a full one.
func TestPartialRunSaysSo(t *testing.T) {
	got := render(t, fixedReport(t), RenderOptions{Format: FormatText})
	if !strings.Contains(got, "5 of 12 samples (--limit 5)") {
		t.Errorf("a limited run does not say how much it covered:\n%s", got)
	}
}

func TestJSONShape(t *testing.T) {
	var round map[string]any
	if err := json.Unmarshal([]byte(render(t, fixedReport(t), RenderOptions{Format: FormatJSON})), &round); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	for _, key := range []string{
		"schema_version", "config", "timing", "results", "by_tag", "samples",
		"warnings", "higher_is_better",
	} {
		if _, ok := round[key]; !ok {
			t.Errorf("JSON is missing %q", key)
		}
	}
	// The metric descriptions are wording for a reader. Emitting them would put
	// a second copy of every value beside results, with nothing consuming it.
	if _, ok := round["metrics"]; ok {
		t.Error(`JSON carries "metrics", which the renderers own instead`)
	}
	if got := round["schema_version"].(float64); got != schemaVersion {
		t.Errorf("schema_version = %v, want %d", got, schemaVersion)
	}

	config := round["config"].(map[string]any)
	if got := config["dataset_samples"].(float64); got != 12 {
		t.Errorf("dataset_samples = %v, want 12", got)
	}
	if got := config["limit"].(float64); got != 5 {
		t.Errorf("limit = %v, want 5", got)
	}
	if got := config["elapsed_seconds"].(float64); got == 0 {
		t.Error("elapsed_seconds = 0, want the run's wall clock")
	}

	// The JSON is the whole record regardless of what the reports chose to show.
	samples := round["samples"].([]any)
	if len(samples) != 5 {
		t.Fatalf("samples = %d, want all 5 including the unanswered one", len(samples))
	}
	var withReason int
	for _, s := range samples {
		if _, ok := s.(map[string]any)["llm_score_reason"]; ok {
			withReason++
		}
	}
	if withReason == 0 {
		t.Error("no sample carries its reason; --show-why must not affect the JSON")
	}
}

// A metric rendered with no definition would print its key as its label, and a
// definition too long for the last column would push the table past 80.
func TestMetricCatalogCoversRenderedKeys(t *testing.T) {
	for _, catalog := range []metricCatalog{scoringMetrics, highSignalMetrics} {
		for _, metric := range catalog {
			t.Run(metric.Key, func(t *testing.T) {
				if metric.Label == "" || metric.About == "" {
					t.Error("metric has no label or no description")
				}
				if n := utf8.RuneCountInString(metric.About); n > aboutWidth {
					t.Errorf("About is %d runes, want at most %d", n, aboutWidth)
				}
				if metric.Best < metric.Min || metric.Best > metric.Max {
					t.Errorf("Best %v sits outside %v..%v", metric.Best, metric.Min, metric.Max)
				}
				if got := catalog.byKey(metric.Key); got.Label != metric.Label {
					t.Errorf("byKey(%q) = %q, want %q", metric.Key, got.Label, metric.Label)
				}
			})
		}
	}
}

func TestMetricRange(t *testing.T) {
	tests := []struct {
		name string
		in   MetricInfo
		want string
	}{
		{name: "higher is better", in: MetricInfo{Min: -1, Max: 1, Best: 1}, want: "-1..1 up"},
		{name: "lower is better", in: MetricInfo{Min: 0, Max: 10, Best: 0}, want: "0..10 down"},
		{name: "aim for the middle", in: MetricInfo{Min: -10, Max: 10, Best: 0}, want: "-10..10 ->0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.in.Range(); got != tt.want {
				t.Errorf("Range() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{in: 0, want: "0s"},
		{in: 250 * time.Microsecond, want: "250µs"},
		{in: 12 * time.Millisecond, want: "12ms"},
		{in: 1500 * time.Millisecond, want: "1.5s"},
		{in: 72*time.Second + 400*time.Millisecond, want: "1m12.4s"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := formatDuration(tt.in); got != tt.want {
				t.Errorf("formatDuration(%v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
