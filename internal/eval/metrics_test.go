// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"math"
	"testing"
)

// scored builds an answered outcome: a sample expecting want, scored got.
func scored(id string, want, got int, tags ...Tag) Outcome {
	w := want
	return Outcome{
		Sample: Sample{ID: id, Title: id, Expected: &w, Tags: tags},
		Score:  got,
		Scored: true,
	}
}

// unscored builds an outcome the model returned nothing for.
func unscored(id string, want int, tags ...Tag) Outcome {
	w := want
	return Outcome{Sample: Sample{ID: id, Title: id, Expected: &w, Tags: tags}}
}

func closeTo(got, want float64) bool { return math.Abs(got-want) < 1e-9 }

func TestCompute(t *testing.T) {
	tests := []struct {
		name                    string
		outcomes                []Outcome
		samples, scored, missin int
		mae, signed             float64
	}{
		{
			name:     "exact agreement",
			outcomes: []Outcome{scored("a", 9, 9), scored("b", 1, 1)},
			samples:  2, scored: 2,
			mae: 0, signed: 0,
		},
		{
			name:     "uniformly generous shows in the sign",
			outcomes: []Outcome{scored("a", 5, 7), scored("b", 1, 3)},
			samples:  2, scored: 2,
			mae: 2, signed: 2,
		},
		{
			name:     "noise cancels in the sign but not in MAE",
			outcomes: []Outcome{scored("a", 5, 7), scored("b", 5, 3)},
			samples:  2, scored: 2,
			mae: 2, signed: 0,
		},
		{
			name:     "empty input does not divide by zero",
			outcomes: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := Compute(tt.outcomes)
			if m.Samples != tt.samples || m.Scored != tt.scored || m.Missing != tt.missin {
				t.Errorf("samples/scored/missing = %d/%d/%d, want %d/%d/%d",
					m.Samples, m.Scored, m.Missing, tt.samples, tt.scored, tt.missin)
			}
			if !closeTo(m.MAE, tt.mae) {
				t.Errorf("MAE = %v, want %v", m.MAE, tt.mae)
			}
			if !closeTo(m.SignedMean, tt.signed) {
				t.Errorf("SignedMean = %v, want %v", m.SignedMean, tt.signed)
			}
		})
	}
}

// A sample the model never answered is an absence of evidence. Folding it in as
// a zero would credit the benchmark for agreeing with every low label.
func TestComputeExcludesMissingRatherThanScoringThemZero(t *testing.T) {
	outcomes := []Outcome{
		scored("a", 9, 9),
		unscored("b", 0), // if this counted as 0 it would look like a perfect hit
	}
	m := Compute(outcomes)

	if m.Samples != 2 || m.Scored != 1 || m.Missing != 1 {
		t.Fatalf("samples/scored/missing = %d/%d/%d, want 2/1/1", m.Samples, m.Scored, m.Missing)
	}
	if !closeTo(m.MAE, 0) {
		t.Errorf("MAE = %v, want 0 over the one scored sample", m.MAE)
	}
	if total := m.Histogram[0]; total != 1 {
		t.Errorf("histogram at 0 = %d, want 1; the missing sample must not appear", total)
	}
}

func TestComputeAllMissing(t *testing.T) {
	m := Compute([]Outcome{unscored("a", 5), unscored("b", 5)})
	if m.Scored != 0 || m.Missing != 2 {
		t.Errorf("scored/missing = %d/%d, want 0/2", m.Scored, m.Missing)
	}
	if !closeTo(m.MAE, 0) || len(m.Histogram) != 0 {
		t.Errorf("MAE = %v, histogram = %v, want 0 and empty", m.MAE, m.Histogram)
	}
}

func TestMetricsWithin(t *testing.T) {
	m := Compute([]Outcome{
		scored("a", 5, 5), // 0
		scored("b", 5, 6), // +1
		scored("c", 5, 3), // -2
		scored("d", 5, 9), // +4
	})
	for _, tt := range []struct{ n, want int }{{0, 1}, {1, 2}, {2, 3}, {3, 3}, {4, 4}} {
		if got := m.Within(tt.n); got != tt.want {
			t.Errorf("Within(%d) = %d, want %d", tt.n, got, tt.want)
		}
	}
}

func TestByTagGroupsOverlapAndSortWorstFirst(t *testing.T) {
	outcomes := []Outcome{
		scored("a", 9, 9, "on-target"),                 // perfect
		scored("b", 1, 8, "off-topic", "keyword-bait"), // badly overscored, in two groups
		scored("c", 1, 2, "off-topic"),                 // near
	}
	got := ByTag(outcomes, map[Tag]string{"keyword-bait": "no substance"})

	if len(got) != 3 {
		t.Fatalf("got %d groups, want 3", len(got))
	}
	if got[0].Tag != "keyword-bait" {
		t.Errorf("worst group = %q, want keyword-bait first", got[0].Tag)
	}
	if got[0].Description != "no substance" {
		t.Errorf("Description = %q, want the fixture's line", got[0].Description)
	}
	if last := got[len(got)-1].Tag; last != "on-target" {
		t.Errorf("best group = %q, want on-target last", last)
	}
	// b belongs to two groups at once.
	for _, g := range got {
		if g.Tag == "off-topic" && g.Samples != 2 {
			t.Errorf("off-topic covers %d samples, want 2", g.Samples)
		}
		if g.Tag == "keyword-bait" && g.Samples != 1 {
			t.Errorf("keyword-bait covers %d samples, want 1", g.Samples)
		}
	}
}

func TestWorst(t *testing.T) {
	outcomes := []Outcome{
		scored("a", 5, 5), // 0, excluded: not a disagreement
		scored("b", 5, 7), // 2
		scored("c", 5, 0), // 5
		scored("d", 5, 8), // 3
		unscored("e", 5),  // never answered, not a distance
	}
	got := Worst(outcomes, 2)
	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
	if got[0].Sample.ID != "c" || got[1].Sample.ID != "d" {
		t.Errorf("got %s,%s, want c,d furthest first", got[0].Sample.ID, got[1].Sample.ID)
	}
	if all := Worst(outcomes, -1); len(all) != 3 {
		t.Errorf("Worst(-1) returned %d, want all 3 disagreements", len(all))
	}
}

func TestSpreadOf(t *testing.T) {
	t.Run("a single run measures no noise", func(t *testing.T) {
		s := SpreadOf([][]Outcome{{scored("a", 5, 7)}})
		if s.Runs != 1 || s.WidestSample != 0 || s.MAEMax != 0 {
			t.Errorf("got %+v, want a zero spread for one run", s)
		}
	})

	t.Run("repeated runs expose the noise floor", func(t *testing.T) {
		s := SpreadOf([][]Outcome{
			{scored("a", 5, 5), scored("b", 5, 5)}, // MAE 0
			{scored("a", 5, 8), scored("b", 5, 6)}, // MAE 2
		})
		if s.Runs != 2 {
			t.Errorf("Runs = %d, want 2", s.Runs)
		}
		if !closeTo(s.MAEMin, 0) || !closeTo(s.MAEMax, 2) {
			t.Errorf("MAE range = %v-%v, want 0-2", s.MAEMin, s.MAEMax)
		}
		if s.WidestSample != 3 || s.WidestSampleID != "a" {
			t.Errorf("widest swing = %d on %q, want 3 on a", s.WidestSample, s.WidestSampleID)
		}
	})

	t.Run("missing scores do not widen the swing", func(t *testing.T) {
		s := SpreadOf([][]Outcome{
			{scored("a", 5, 6)},
			{unscored("a", 5)},
		})
		if s.WidestSample != 0 {
			t.Errorf("widest swing = %d, want 0; an unanswered sample is not a swing", s.WidestSample)
		}
	})
}

func TestRMSEPenalisesLargeMissesMoreThanMAE(t *testing.T) {
	// Same total error, different shape: four small misses against one big one.
	even := Compute([]Outcome{
		scored("a", 5, 7), scored("b", 5, 3), scored("c", 5, 7), scored("d", 5, 3),
	})
	spiky := Compute([]Outcome{
		scored("a", 5, 5), scored("b", 5, 5), scored("c", 5, 5), scored("d", 5, 13),
	})
	if !closeTo(even.MAE, spiky.MAE) {
		t.Fatalf("MAE differs (%v vs %v); the point is that it does not", even.MAE, spiky.MAE)
	}
	if spiky.RMSE <= even.RMSE {
		t.Errorf("RMSE %v (spiky) should exceed %v (even); it is what separates them",
			spiky.RMSE, even.RMSE)
	}
}

func TestQWK(t *testing.T) {
	tests := []struct {
		name     string
		outcomes []Outcome
		want     float64
		defined  bool
	}{
		{
			name:     "perfect agreement",
			outcomes: []Outcome{scored("a", 9, 9), scored("b", 4, 4), scored("c", 1, 1)},
			want:     1, defined: true,
		},
		{
			// The property that makes kappa worth having: a scorer answering
			// with one constant agrees exactly as much as chance would, so it
			// lands on 0 no matter how its MAE looks.
			name: "a constant scorer lands on zero",
			outcomes: []Outcome{
				scored("a", 9, 9), scored("b", 4, 9), scored("c", 1, 9), scored("d", 7, 9),
			},
			want: 0, defined: true,
		},
		{
			name:     "inverted ordering goes negative",
			outcomes: []Outcome{scored("a", 10, 0), scored("b", 0, 10)},
			want:     -1, defined: true,
		},
		{
			name:     "undefined when neither side varies",
			outcomes: []Outcome{scored("a", 5, 5), scored("b", 5, 5)},
			defined:  false,
		},
		{
			name:     "undefined with nothing scored",
			outcomes: []Outcome{unscored("a", 5)},
			defined:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := QWK(tt.outcomes)
			if ok != tt.defined {
				t.Fatalf("defined = %v, want %v", ok, tt.defined)
			}
			if tt.defined && !closeTo(got, tt.want) {
				t.Errorf("QWK = %v, want %v", got, tt.want)
			}
		})
	}
}

// A model can sit close on average and still fill the page with the wrong
// things, which is what the digest numbers are for.
func TestHighSignalAt(t *testing.T) {
	outcomes := []Outcome{
		scored("hit", 9, 8),    // wanted, shown
		scored("noise", 2, 9),  // not wanted, shown
		scored("missed", 8, 3), // wanted, not shown
		scored("agreed", 1, 2), // not wanted, not shown
		unscored("skipped", 9), // never answered, counts nowhere
	}
	d := HighSignalAt(outcomes, 7)

	if d.Hit != 1 || d.Noise != 1 || d.Missed != 1 {
		t.Errorf("hit/noise/missed = %d/%d/%d, want 1/1/1", d.Hit, d.Noise, d.Missed)
	}
	if d.Flagged != 2 || d.Wanted != 2 {
		t.Errorf("surfaced/wanted = %d/%d, want 2/2", d.Flagged, d.Wanted)
	}
	if d.Precision == nil || !closeTo(*d.Precision, 0.5) {
		t.Errorf("precision = %v, want 0.5", d.Precision)
	}
	if d.Recall == nil || !closeTo(*d.Recall, 0.5) {
		t.Errorf("recall = %v, want 0.5", d.Recall)
	}
}

// Nothing surfaced and nothing wanted are different from scoring zero, so they
// stay nil rather than becoming a number that reads as a result.
func TestHighSignalUndefinedRatherThanZero(t *testing.T) {
	empty := HighSignalAt([]Outcome{scored("a", 9, 1), scored("b", 8, 2)}, 7)
	if empty.Precision != nil {
		t.Errorf("precision = %v, want nil when nothing was surfaced", *empty.Precision)
	}
	if empty.Recall == nil || !closeTo(*empty.Recall, 0) {
		t.Error("recall should be 0: two were wanted and both missed")
	}

	nothingWanted := HighSignalAt([]Outcome{scored("a", 1, 9)}, 7)
	if nothingWanted.Recall != nil {
		t.Errorf("recall = %v, want nil when nothing was wanted", *nothingWanted.Recall)
	}
}
