// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// reportOf summarises one run of the given outcomes.
func reportOf(t *testing.T, outcomes []Outcome, tags map[Tag]string) *Report {
	t.Helper()
	return Summarize(&Results{Runs: [][]Outcome{outcomes}}, &Dataset{Tags: tags})
}

func render(t *testing.T, r *Report) string {
	t.Helper()
	var b strings.Builder
	if err := r.WriteMarkdown(&b); err != nil {
		t.Fatalf("WriteMarkdown() error = %v", err)
	}
	return b.String()
}

// A model that answers with one value is not scoring, and every other metric
// then measures where the labels happen to coincide with that value. The report
// has to say so, because MAE and the tag table look ordinary.
func TestReportFlagsADegenerateModel(t *testing.T) {
	// Eleven samples spread across the scale, all answered 9.
	var outcomes []Outcome
	for i, want := range []int{9, 9, 7, 4, 1, 1, 7, 4, 3, 9, 4} {
		outcomes = append(outcomes, scored(string(rune('a'+i)), want, 9))
	}
	got := render(t, reportOf(t, outcomes, nil))

	if !strings.Contains(got, "1 different values across") {
		t.Errorf("report does not flag the degenerate model:\n%s", got)
	}
	if !strings.Contains(got, "not a judgement") {
		t.Error("the warning does not explain why the other metrics cannot be read")
	}
	// The three samples that expected 9 look like agreement and are not.
	if !strings.Contains(got, "3 of 11 matched golden exactly") {
		t.Errorf("agreement count missing or wrong:\n%s", got)
	}
}

func TestReportDoesNotFlagADiscriminatingModel(t *testing.T) {
	outcomes := []Outcome{
		scored("a", 9, 9), scored("b", 7, 6), scored("c", 4, 5), scored("d", 1, 2),
	}
	got := render(t, reportOf(t, outcomes, nil))

	if strings.Contains(got, "not telling them apart") {
		t.Errorf("false positive: four distinct scores should not warn:\n%s", got)
	}
	if !strings.Contains(got, "4 different, against") {
		t.Errorf("score spread not reported:\n%s", got)
	}
}

func TestReportAgreementPrecedesDisagreements(t *testing.T) {
	got := render(t, reportOf(t, []Outcome{
		scored("hit", 9, 9), scored("miss", 1, 8),
	}, nil))

	agree, disagree := strings.Index(got, "## Agreement"), strings.Index(got, "## Disagreements")
	switch {
	case agree < 0:
		t.Fatal("no Agreement section")
	case disagree < 0:
		t.Fatal("no Disagreements section")
	case agree > disagree:
		t.Error("Agreement should come before Disagreements")
	}
	if !strings.Contains(got, "1 of 2 matched golden exactly") {
		t.Error("agreement count missing")
	}
}

func TestReportOmitsAgreementWhenNothingMatched(t *testing.T) {
	got := render(t, reportOf(t, []Outcome{scored("a", 9, 2)}, nil))
	if strings.Contains(got, "## Agreement") {
		t.Error("an empty Agreement section should not be printed")
	}
}

// Unanswered samples must be visible rather than folded into a score.
func TestReportSurfacesUnanswered(t *testing.T) {
	got := render(t, reportOf(t, []Outcome{scored("a", 9, 9), unscored("b", 1)}, nil))
	if !strings.Contains(got, "1 unanswered") {
		t.Errorf("unanswered count missing from the summary:\n%s", got)
	}
	if !strings.Contains(got, "the model returned no score") {
		t.Error("unanswered sample not listed")
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

// The JSON is what a future renderer draws from, so it has to carry every
// sample rather than only the ones markdown chose to print.
func TestJSONCarriesEverySample(t *testing.T) {
	rep := reportOf(t, []Outcome{
		scored("a", 9, 9), scored("b", 1, 8), unscored("c", 4),
	}, nil)

	var b strings.Builder
	if err := rep.WriteJSON(&b); err != nil {
		t.Fatalf("WriteJSON() error = %v", err)
	}
	var round map[string]any
	if err := json.Unmarshal([]byte(b.String()), &round); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	for _, key := range []string{"config", "results", "by_tag", "samples", "higher_is_better"} {
		if _, ok := round[key]; !ok {
			t.Errorf("JSON is missing %q", key)
		}
	}
	if n := len(round["samples"].([]any)); n != 3 {
		t.Errorf("samples = %d, want all 3 including the unanswered one", n)
	}
}

func TestSpreadBlockOnlyWithRepeats(t *testing.T) {
	one := Summarize(&Results{Runs: [][]Outcome{{scored("a", 5, 5)}}}, &Dataset{})
	if one.Spread != nil {
		t.Error("a single run should carry no spread block")
	}

	two := Summarize(&Results{Runs: [][]Outcome{
		{scored("a", 5, 5)}, {scored("a", 5, 8)},
	}}, &Dataset{})
	if two.Spread == nil {
		t.Fatal("repeated runs should carry a spread block")
	}
	if two.Spread.WidestSample != 3 {
		t.Errorf("widest swing = %d, want 3", two.Spread.WidestSample)
	}
	if got := render(t, two); !strings.Contains(got, "Noise floor") {
		t.Error("noise floor section missing from the markdown")
	}
}

// A backend that answered nothing leaves every aggregate at its zero value, and
// MAE 0.00 reads as a flawless run. The report must refuse to print the table.
func TestReportRefusesToScoreAnUnansweredRun(t *testing.T) {
	rep := reportOf(t, []Outcome{unscored("a", 9), unscored("b", 1)}, nil)

	out := render(t, rep)
	if !strings.Contains(out, "No sample was scored") {
		t.Errorf("report does not say nothing was measured:\n%s", out)
	}
	for _, banned := range []string{"average miss (MAE)", "nothing missed", "score agreement"} {
		if strings.Contains(out, banned) {
			t.Errorf("report prints %q on a run where nothing was scored:\n%s", banned, out)
		}
	}
}

// The sweep is what separates a model that ranks badly from one that ranks well
// and scores everything low, so it must survive the case that produces them
// both: nothing reaching the badge threshold.
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
	out := render(t, rep)
	if !strings.Contains(out, "At other marks") {
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
	if out := render(t, rep); !strings.Contains(out, "thin *") {
		t.Errorf("thin group is not marked in the table:\n%s", out)
	}
}

// Quality is half the choice --model exists to make, so the run has to report
// what it cost in wall clock.
func TestTimingReportsTheHeadlineRun(t *testing.T) {
	rep := Summarize(&Results{
		Runs:    [][]Outcome{{scored("a", 9, 9)}},
		Elapsed: []time.Duration{4 * time.Second},
	}, &Dataset{})

	if rep.Timing.Seconds != 4 {
		t.Errorf("Timing.Seconds = %v, want 4", rep.Timing.Seconds)
	}
	if rep.Timing.PerSample != 4 {
		t.Errorf("Timing.PerSample = %v, want 4 over one sample", rep.Timing.PerSample)
	}
	if out := render(t, rep); !strings.Contains(out, "/sample)") {
		t.Errorf("header omits timing:\n%s", out)
	}
}
