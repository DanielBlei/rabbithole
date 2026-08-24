// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// HigherIsBetter gives each headline metric its direction, so a renderer can
// orient a comparison without knowing what the metrics mean.
//
// Only evaluative metrics belong here. Counts and spreads are diagnostic: a
// wider spread describes the model's consistency, not its quality, and putting
// it here would mislead any tool that auto-orients on this map.
var HigherIsBetter = map[string]bool{
	"qwk":                   true,
	"mae":                   false,
	"rmse":                  false,
	"high_signal.precision": true,
	"high_signal.recall":    true,
}

// schemaVersion is the shape of the JSON report.
//
// Bumped only when a field is removed or changes meaning. Adding a field does
// not bump it, so a consumer has to ignore keys it does not know.
const schemaVersion = 1

// Warning codes. The code is what a script keys on; the text is for reading.
const (
	// WarnConstantScorer means the model answered with almost one value, which
	// makes every other number here an accident of where golden agreed with it.
	WarnConstantScorer = "constant_scorer"
	// WarnNoAgreement means kappa found no relationship to golden.
	WarnNoAgreement = "no_agreement"
	// WarnUnanswered means the model returned nothing for some samples.
	WarnUnanswered = "unanswered"
)

// Warning is something about the run that undermines reading the rest of it.
// These are the only judgements the report makes: everything else states what a
// metric measures and leaves the verdict to whoever is reading.
type Warning struct {
	Code string `json:"code"`
	// Text is plain prose. It carries no Markdown and no terminal styling, so
	// that each renderer can apply its own and the same string stays correct in
	// all three.
	Text string `json:"text"`
}

// Report is the computed result of a benchmark run and the single source every
// renderer draws from. The renderers are pure functions of this shape: they
// compute nothing of their own, so adding a fourth costs nothing here.
type Report struct {
	SchemaVersion int             `json:"schema_version"`
	Info          RunInfo         `json:"config"`
	Timing        TimingBlock     `json:"timing"`
	Results       ResultsBlock    `json:"results"`
	ByTag         []TagRow        `json:"by_tag"`
	Samples       []SampleRow     `json:"samples"`
	Spread        *SpreadBlock    `json:"spread,omitempty"`
	Warnings      []Warning       `json:"warnings"`
	Rated         map[string]bool `json:"higher_is_better"`
}

// TimingBlock is what the run cost in wall clock, for the headline run only.
//
// Quality is half of the choice --model exists to make: a model twice as good
// and twenty times slower is not obviously the right one, and without this the
// report cannot be used to decide.
type TimingBlock struct {
	Seconds float64 `json:"seconds"`
	// PerSample is Seconds over the samples attempted, which is the figure that
	// carries over to a real ingest of a different size.
	PerSample float64 `json:"seconds_per_sample"`
}

// ResultsBlock is the headline set.
type ResultsBlock struct {
	Samples int `json:"n_samples"`
	Scored  int `json:"n_scored"`
	// Missing counts samples the model returned nothing for. They are excluded
	// from every metric below rather than read as a zero score.
	Missing int `json:"n_unanswered"`

	MAE        float64 `json:"mae"`
	RMSE       float64 `json:"rmse"`
	SignedMean float64 `json:"signed_mean"`

	// QWK is chance-corrected agreement, nil when undefined. Unlike MAE it does
	// not depend on how the golden scores are spread, so it is the number that
	// compares across fixtures and the one that exposes a constant scorer.
	QWK *float64 `json:"qwk"`

	// HighSignal is precision and recall at the mark the feed page badges as
	// high signal. Nothing is hidden below it, so this measures what the badge
	// claims rather than what reaches the page.
	HighSignal HighSignal `json:"high_signal"`
	// HighSignalSweep is the same measurement at marks either side of it, which
	// is what separates a model that ranks badly from one that ranks well and
	// scores low. At a single mark the two are indistinguishable.
	HighSignalSweep []HighSignal `json:"high_signal_sweep"`

	// Within maps a distance to how many samples landed inside it.
	Within map[string]int `json:"within"`
	// Histogram is the signed error distribution, ascending, ready to plot.
	Histogram []HistBucket `json:"histogram"`

	// ScoreSpread counts how many samples got each score, and LabelSpread the
	// same for golden.
	//
	// A model that answers with only one or two distinct scores is not
	// discriminating at all, and every other metric here is then measuring an
	// accident: it will look accurate wherever golden happens to agree with
	// its one answer. Nothing else in the report shows that.
	ScoreSpread []ScoreBucket `json:"score_spread"`
	LabelSpread []ScoreBucket `json:"label_spread"`
}

// HistBucket is one signed-error column.
type HistBucket struct {
	Err   int `json:"err"`
	Count int `json:"count"`
}

// ScoreBucket counts samples at one score.
type ScoreBucket struct {
	Score int `json:"score"`
	Count int `json:"count"`
}

// TagRow is one tag's slice of the run.
type TagRow struct {
	Tag         string  `json:"tag"`
	Description string  `json:"description,omitempty"`
	Samples     int     `json:"n_samples"`
	Scored      int     `json:"n_scored"`
	MAE         float64 `json:"mae"`
	SignedMean  float64 `json:"signed_mean"`
	// Thin marks a group too small for its MAE to describe the group rather
	// than one article. Carried rather than recomputed so both renderers, and
	// anything reading the JSON, draw the same line.
	Thin bool `json:"thin"`
}

// SampleRow is one sample and what the model did with it. Every sample appears,
// not only the disagreements, so a renderer can show the whole table.
type SampleRow struct {
	ID       string   `json:"id"`
	Source   string   `json:"source,omitempty"`
	Title    string   `json:"title"`
	Tags     []string `json:"tags,omitempty"`
	Expected int      `json:"expected_llm_score"`
	// Score and Err are meaningless when Scored is false.
	Scored bool   `json:"scored"`
	Score  int    `json:"llm_score"`
	Err    int    `json:"err"`
	Reason string `json:"llm_score_reason,omitempty"`
	Note   string `json:"note,omitempty"`
}

// SpreadBlock reports run-to-run disagreement, present only with repeats.
type SpreadBlock struct {
	Runs      int       `json:"runs"`
	MAEPerRun []float64 `json:"mae_per_run"`
	MAEMin    float64   `json:"mae_min"`
	MAEMax    float64   `json:"mae_max"`
	// RunSeconds is how long each repeat took, parallel to MAEPerRun, so the
	// noise floor shows what the consistency cost in time.
	RunSeconds     []float64 `json:"run_seconds,omitempty"`
	WidestSample   int       `json:"widest_sample_swing"`
	WidestSampleID string    `json:"widest_sample_id,omitempty"`
}

// Summarize computes the report from a run.
//
// The headline metrics describe the first run only; extra repeats feed Spread
// and nothing else. Pooling them would change what MAE means, mixing how hard a
// sample is with how much the model wavers on it, and the two want different
// fixes.
func Summarize(res *Results, ds *Dataset) *Report {
	first := res.Runs[0]
	m := Compute(first)

	rep := &Report{
		SchemaVersion: schemaVersion,
		Info:          res.Info,
		Rated:         HigherIsBetter,
		Results: ResultsBlock{
			Samples: m.Samples, Scored: m.Scored, Missing: m.Missing,
			MAE: m.MAE, RMSE: m.RMSE, SignedMean: m.SignedMean,
			HighSignal:      HighSignalAt(first, HighSignalThreshold),
			HighSignalSweep: HighSignalSweep(first),
			Within:          map[string]int{},
			Histogram:       histogram(m),
		},
	}
	if len(res.RunSeconds) > 0 {
		secs := res.RunSeconds[0]
		rep.Timing = TimingBlock{Seconds: secs}
		if m.Samples > 0 {
			rep.Timing.PerSample = secs / float64(m.Samples)
		}
	}
	if k, ok := QWK(first); ok {
		rep.Results.QWK = &k
	}
	for n := range 4 {
		rep.Results.Within[fmt.Sprintf("%d", n)] = m.Within(n)
	}

	gave, want := map[int]int{}, map[int]int{}
	for _, o := range first {
		want[o.Sample.ExpectedScore()]++
		if o.Scored {
			gave[o.Score]++
		}
	}
	rep.Results.ScoreSpread = spread(gave)
	rep.Results.LabelSpread = spread(want)

	for _, tm := range ByTag(first, ds.Tags) {
		rep.ByTag = append(rep.ByTag, TagRow{
			Tag: string(tm.Tag), Description: tm.Description,
			Samples: tm.Samples, Scored: tm.Scored,
			MAE: tm.MAE, SignedMean: tm.SignedMean,
			Thin: tm.Thin(),
		})
	}

	for _, o := range first {
		row := SampleRow{
			ID: o.Sample.ID, Source: o.Sample.Source, Title: o.Sample.Title,
			Expected: o.Sample.ExpectedScore(), Scored: o.Scored,
			Reason: o.Reason, Note: strings.TrimSpace(o.Sample.Note),
		}
		for _, t := range o.Sample.Tags {
			row.Tags = append(row.Tags, string(t))
		}
		if o.Scored {
			row.Score, row.Err = o.Score, o.Err()
		}
		rep.Samples = append(rep.Samples, row)
	}
	sortSamples(rep.Samples)

	if len(res.Runs) > 1 {
		s := SpreadOf(res.Runs)
		block := &SpreadBlock{
			Runs: s.Runs, MAEMin: s.MAEMin, MAEMax: s.MAEMax,
			RunSeconds:   res.RunSeconds,
			WidestSample: s.WidestSample, WidestSampleID: s.WidestSampleID,
		}
		for _, run := range res.Runs {
			block.MAEPerRun = append(block.MAEPerRun, Compute(run).MAE)
		}
		rep.Spread = block
	}

	rep.Warnings = warningsFor(rep)
	return rep
}

// sortSamples puts the biggest disagreements first, so the rows worth acting on
// read before the ones that already agree. Unanswered samples have no distance
// to sort by and go last, where the warning above the table accounts for them.
func sortSamples(rows []SampleRow) {
	slices.SortFunc(rows, func(a, b SampleRow) int {
		if a.Scored != b.Scored {
			if a.Scored {
				return -1
			}
			return 1
		}
		if c := cmp.Compare(abs(b.Err), abs(a.Err)); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
}

// warningsFor collects what would make the rest of the report misleading,
// strongest first.
func warningsFor(rep *Report) []Warning {
	var out []Warning
	res := rep.Results

	used := distinct(res.ScoreSpread)
	// Two values out of eleven is not scoring, it is a constant. Every metric
	// above will look respectable wherever the labels happen to agree with that
	// constant, and the tag table will attribute the accident to whichever tags
	// those samples carry.
	constant := used <= 2 && res.Scored > 2
	if constant {
		out = append(out, Warning{
			Code: WarnConstantScorer,
			Text: fmt.Sprintf("The model gave nearly every sample the same score: %d different "+
				"values across %d samples. It is not telling them apart, so it agrees with golden "+
				"only where golden happens to land on that value. Try a larger model first.",
				used, res.Scored),
		})
	}

	// A constant scorer lands at kappa 0 by construction, so this would always
	// fire alongside the warning above and say a weaker version of it.
	if !constant && res.QWK != nil && *res.QWK < poorAgreementBelow {
		out = append(out, Warning{
			Code: WarnNoAgreement,
			Text: "Scores show no reliable agreement with golden. Consider a larger model before " +
				"reading anything below as a judgement of the profile or the prompt.",
		})
	}

	if res.Missing > 0 {
		out = append(out, Warning{
			Code: WarnUnanswered,
			Text: fmt.Sprintf("The model returned no score for %d of %d samples. They are counted "+
				"here and excluded from every metric, rather than read as a zero.",
				res.Missing, res.Samples),
		})
	}
	return out
}

// spread tallies scores across the whole 0-10 scale, including the scores
// nobody gave. The empty buckets are the point: they are what shows a model
// answering with two values out of eleven, and keeping the axis fixed lets a
// renderer plot it without inventing the gaps itself.
func spread(counts map[int]int) []ScoreBucket {
	out := make([]ScoreBucket, 0, MaxScore-MinScore+1)
	for score := MinScore; score <= MaxScore; score++ {
		out = append(out, ScoreBucket{Score: score, Count: counts[score]})
	}
	return out
}

// distinct counts how many different scores were actually used.
func distinct(buckets []ScoreBucket) int {
	var n int
	for _, b := range buckets {
		if b.Count > 0 {
			n++
		}
	}
	return n
}

// histogram flattens the error counts into ascending buckets, with no gaps, so
// a renderer can draw it directly.
func histogram(m Metrics) []HistBucket {
	if len(m.Histogram) == 0 {
		return nil
	}
	keys := make([]int, 0, len(m.Histogram))
	for k := range m.Histogram {
		keys = append(keys, k)
	}
	lo, hi := slices.Min(keys), slices.Max(keys)

	out := make([]HistBucket, 0, hi-lo+1)
	for err := lo; err <= hi; err++ {
		out = append(out, HistBucket{Err: err, Count: m.Histogram[err]})
	}
	return out
}

// duration renders a number of seconds compactly, keeping a sub-second
// per-sample figure readable rather than rounding it to 0s.
func duration(secs float64) string {
	switch {
	case secs >= 60:
		return fmt.Sprintf("%dm%02ds", int(secs)/60, int(secs)%60)
	case secs >= 10:
		return fmt.Sprintf("%.0fs", secs)
	case secs >= 1:
		return fmt.Sprintf("%.1fs", secs)
	default:
		return fmt.Sprintf("%.0fms", secs*1000)
	}
}

const smallFixture = 30

// poorAgreementBelow is where a run stops being worth interpreting. Below it
// the model's scores carry no detectable relationship to golden.
const poorAgreementBelow = 0.2
