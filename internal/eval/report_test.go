// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"fmt"
	"strings"
	"testing"
)

// reportOf summarises one run of the given outcomes.
func reportOf(t *testing.T, outcomes []Outcome, tags map[Tag]string) *Report {
	t.Helper()
	return Summarize(&Results{Runs: [][]Outcome{outcomes}}, &Dataset{Tags: tags})
}

// constantScorer answers every sample with one value, spread across labels that
// are not. Three of them expect that value, so the run also looks like partial
// agreement, which is the trap the warning exists to call out.
func constantScorer() []Outcome {
	var outcomes []Outcome
	for i, want := range []int{9, 9, 7, 4, 1, 1, 7, 4, 3, 9, 4} {
		outcomes = append(outcomes, scored(string(rune('a'+i)), want, 9))
	}
	return outcomes
}

// codesOf lists the warning codes a report raised, in order.
func codesOf(rep *Report) []string {
	codes := make([]string, 0, len(rep.Warnings))
	for _, w := range rep.Warnings {
		codes = append(codes, w.Code)
	}
	return codes
}

func TestSummarizeWarnings(t *testing.T) {
	tests := []struct {
		name     string
		outcomes []Outcome
		want     []string
	}{
		{
			// A constant scorer lands at kappa 0 by construction, so
			// no_agreement would always fire beside it saying a weaker version
			// of the same thing.
			name:     "a constant scorer suppresses the weaker agreement warning",
			outcomes: constantScorer(),
			want:     []string{WarnConstantScorer},
		},
		{
			name: "a discriminating model raises nothing",
			outcomes: []Outcome{
				scored("a", 9, 9), scored("b", 7, 6), scored("c", 4, 5), scored("d", 1, 2),
			},
			want: []string{},
		},
		{
			name: "scores unrelated to golden raise no_agreement on their own",
			outcomes: []Outcome{
				scored("a", 9, 1), scored("b", 1, 9), scored("c", 7, 3),
				scored("d", 3, 7), scored("e", 4, 8),
			},
			want: []string{WarnNoAgreement},
		},
		{
			name:     "an unanswered sample is reported, not folded into a score",
			outcomes: []Outcome{scored("a", 9, 9), scored("b", 7, 6), scored("c", 1, 2), unscored("d", 4)},
			want:     []string{WarnUnanswered},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := codesOf(reportOf(t, tt.outcomes, nil))
			if len(got) != len(tt.want) {
				t.Fatalf("warnings = %v, want %v", got, tt.want)
			}
			for i, code := range tt.want {
				if got[i] != code {
					t.Errorf("warning %d = %q, want %q", i, got[i], code)
				}
			}
		})
	}
}

func TestSummarizeOrdersSamplesWorstFirst(t *testing.T) {
	rep := reportOf(t, []Outcome{
		scored("exact", 9, 9),
		unscored("none", 4),
		scored("near", 7, 6),
		scored("far", 1, 8),
	}, nil)

	// Unanswered has no distance to rank by, so it sorts last rather than
	// competing with a real disagreement.
	want := []string{"far", "near", "exact", "none"}
	for i, id := range want {
		if rep.Samples[i].ID != id {
			t.Errorf("sample %d = %q, want %q (order: %v)", i, rep.Samples[i].ID, id, want)
		}
	}
}

// The score axis is a fixed domain, so the spread covers 0-10 including the
// scores nobody gave. A chart needs the empty buckets to space its bars.
func TestScoreSpreadCoversTheWholeScale(t *testing.T) {
	rep := reportOf(t, []Outcome{scored("a", 9, 9), scored("b", 1, 9)}, nil)

	if n := len(rep.Results.ScoreSpread); n != MaxScore-MinScore+1 {
		t.Fatalf("ScoreSpread has %d buckets, want %d", n, MaxScore-MinScore+1)
	}
	for i, b := range rep.Results.ScoreSpread {
		if b.Score != i {
			t.Fatalf("bucket %d is score %d, want the scale in order", i, b.Score)
		}
	}
	if got := rep.Results.ScoreSpread[9].Count; got != 2 {
		t.Errorf("score 9 count = %d, want 2", got)
	}
	if got := rep.Results.ScoreSpread[0].Count; got != 0 {
		t.Errorf("score 0 count = %d, want an explicit empty bucket", got)
	}
	if got := distinct(rep.Results.ScoreSpread); got != 1 {
		t.Errorf("distinct = %d, want 1", got)
	}
}

func TestSpreadBlockOnlyWithRepeats(t *testing.T) {
	one := Summarize(&Results{Runs: [][]Outcome{{scored("a", 5, 5)}}}, &Dataset{})
	if one.Spread != nil {
		t.Error("a single run should carry no spread block")
	}

	two := Summarize(&Results{
		Runs:       [][]Outcome{{scored("a", 5, 5)}, {scored("a", 5, 8)}},
		RunSeconds: []float64{1.5, 1.25},
	}, &Dataset{})
	if two.Spread == nil {
		t.Fatal("repeated runs should carry a spread block")
	}
	if two.Spread.WidestSample != 3 {
		t.Errorf("widest swing = %d, want 3", two.Spread.WidestSample)
	}
	// Per-run timings sit beside the per-run MAE they explain.
	if got := two.Spread.RunSeconds; len(got) != 2 || got[0] != 1.5 {
		t.Errorf("RunSeconds = %v, want [1.5 1.25]", got)
	}
}

func TestDatasetLimit(t *testing.T) {
	full := &Dataset{
		Hash:    "sha256:abc",
		Samples: []Sample{{ID: "a"}, {ID: "b"}, {ID: "c"}},
	}

	tests := []struct {
		name string
		n    int
		want []string
	}{
		{name: "narrows to the first n in file order", n: 2, want: []string{"a", "b"}},
		{name: "one keeps only the first", n: 1, want: []string{"a"}},
		{name: "n at the sample count is not a narrowing", n: 3, want: []string{"a", "b", "c"}},
		{name: "n above the sample count means all", n: 99, want: []string{"a", "b", "c"}},
		{name: "zero means all", n: 0, want: []string{"a", "b", "c"}},
		{name: "negative means all", n: -1, want: []string{"a", "b", "c"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := full.Limit(tt.n)
			if len(got.Samples) != len(tt.want) {
				t.Fatalf("Limit(%d) kept %d samples, want %d", tt.n, len(got.Samples), len(tt.want))
			}
			for i, id := range tt.want {
				if got.Samples[i].ID != id {
					t.Errorf("sample %d = %q, want %q", i, got.Samples[i].ID, id)
				}
			}
			// The hash fingerprints the file, not the slice taken from it.
			if got.Hash != full.Hash {
				t.Errorf("Hash = %q, want it carried over unchanged", got.Hash)
			}
			if len(full.Samples) != 3 {
				t.Errorf("Limit mutated the original dataset: %d samples left", len(full.Samples))
			}
		})
	}
}

// The sweep is what separates a model that ranks badly from one that ranks well
// and scores low, so it has to survive the case where the badge threshold
// flagged nothing at all.
func TestSweepSurvivesNothingFlagged(t *testing.T) {
	rep := reportOf(t, []Outcome{
		scored("a", 9, 4), scored("b", 8, 3),
		scored("c", 2, 0), scored("d", 1, 0),
	}, nil)

	if got := len(rep.Results.HighSignalSweep); got != len(SweepThresholds) {
		t.Fatalf("sweep has %d rows, want %d", got, len(SweepThresholds))
	}
	if rep.Results.HighSignal.Flagged != 0 {
		t.Fatalf("expected nothing badged at the threshold, got %d",
			rep.Results.HighSignal.Flagged)
	}

	out := render(t, rep, RenderOptions{Format: FormatText})
	if !strings.Contains(out, "AT OTHER MARKS") {
		t.Errorf("sweep missing when nothing reached the threshold:\n%s", out)
	}
	// Every mark gets a row, including the badge threshold that flagged nothing.
	for _, th := range SweepThresholds {
		if want := fmt.Sprintf("%d+", th); !strings.Contains(out, want) {
			t.Errorf("sweep is missing the %s row:\n%s", want, out)
		}
	}
}

// A one-sample group taking the top slot inverts the point of sorting worst
// first: it promotes the noisiest row precisely because it is the noisiest.
func TestThinTagsSortBelowFullOnes(t *testing.T) {
	rep := reportOf(t, []Outcome{
		scored("a", 9, 0, "thin"),
		scored("b", 9, 8, "full"),
		scored("c", 9, 7, "full"),
		scored("d", 9, 6, "full"),
	}, map[Tag]string{"thin": "one sample", "full": "three samples"})

	if rep.ByTag[0].Tag != "full" {
		t.Errorf("ByTag[0] = %q with MAE %.2f, want the full group first",
			rep.ByTag[0].Tag, rep.ByTag[0].MAE)
	}
	if !rep.ByTag[1].Thin {
		t.Errorf("tag %q with %d samples is not marked thin", rep.ByTag[1].Tag, rep.ByTag[1].Samples)
	}
	if out := render(t, rep, RenderOptions{Format: FormatText}); !strings.Contains(out, "thin *") {
		t.Errorf("thin group is not marked in the table:\n%s", out)
	}
}

// Quality is half the choice --model exists to make, so the run has to report
// what it cost in wall clock.
func TestTimingReportsTheHeadlineRun(t *testing.T) {
	rep := Summarize(&Results{
		Runs:       [][]Outcome{{scored("a", 9, 9)}},
		RunSeconds: []float64{4},
	}, &Dataset{})

	if rep.Timing.Seconds != 4 {
		t.Errorf("Timing.Seconds = %v, want 4", rep.Timing.Seconds)
	}
	if rep.Timing.PerSample != 4 {
		t.Errorf("Timing.PerSample = %v, want 4 over one sample", rep.Timing.PerSample)
	}
}
