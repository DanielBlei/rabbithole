// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"cmp"
	"math"
	"slices"

	"github.com/DanielBlei/rabbithole/internal/rank"
)

// Outcome is one sample and what the model did with it.
type Outcome struct {
	Sample Sample
	// Score is the model's 0-10, meaningful only when Scored.
	Score int
	// Reason is the model's stated rationale, carried for the report.
	Reason string
	// Scored is false when the model returned nothing for this sample.
	//
	// This case has to be tracked rather than defaulted. rank.ScoreAll drops
	// items it could not score instead of failing (internal/rank/rank.go), so a
	// benchmark that read a missing entry as the zero value would score an
	// unanswered sample as 0, which is a real verdict, and quietly credit
	// itself for agreeing with any sample expecting a low score.
	Scored bool
}

// Err is the signed distance from golden: positive when the model scored
// higher. Meaningless unless Scored.
func (o Outcome) Err() int { return o.Score - o.Sample.ExpectedScore() }

// AbsErr is the unsigned distance from the label.
func (o Outcome) AbsErr() int {
	e := o.Err()
	if e < 0 {
		return -e
	}
	return e
}

// Metrics summarises how close a set of outcomes came to their labels.
//
// There is deliberately no pass/fail count. An exact match is not a meaningful
// bar: a 9 answered as an 8 is not a defect, and with no temperature or seed in
// ModelTuning no score repeats reliably anyway. Distance is what carries signal.
type Metrics struct {
	// Samples is every outcome, Scored those the model answered, Missing the
	// rest. Samples == Scored + Missing.
	Samples int
	Scored  int
	Missing int

	// MAE is the mean unsigned distance from golden, over scored samples.
	// Lower is better, 0 is exact agreement.
	MAE float64
	// RMSE penalises a large miss more than several small ones, so it separates
	// a model that is uniformly a bit off from one that is usually right and
	// occasionally catastrophic. MAE alone rates those the same.
	RMSE float64
	// SignedMean is the mean signed distance. Near zero with a high MAE means
	// the model is noisy; consistently positive or negative means it is
	// systematically generous or harsh, which is a different fix.
	SignedMean float64

	// Histogram counts scored samples by signed error, so the shape of the
	// disagreement is visible rather than just its average.
	Histogram map[int]int
}

// Within counts scored samples whose score landed no further than n from the
// label. Reported across several n rather than against one threshold, so no
// tolerance has to be argued for.
func (m Metrics) Within(n int) int {
	var total int
	for err, count := range m.Histogram {
		if err >= -n && err <= n {
			total += count
		}
	}
	return total
}

// Compute summarises outcomes. Missing ones are counted and otherwise left out:
// they are an absence of evidence, not evidence of a wrong score.
func Compute(outcomes []Outcome) Metrics {
	m := Metrics{Samples: len(outcomes), Histogram: map[int]int{}}

	var sumAbs, sumSigned, sumSquares int
	for _, o := range outcomes {
		if !o.Scored {
			m.Missing++
			continue
		}
		m.Scored++
		sumAbs += o.AbsErr()
		sumSigned += o.Err()
		sumSquares += o.Err() * o.Err()
		m.Histogram[o.Err()]++
	}
	if m.Scored > 0 {
		m.MAE = float64(sumAbs) / float64(m.Scored)
		m.RMSE = math.Sqrt(float64(sumSquares) / float64(m.Scored))
		m.SignedMean = float64(sumSigned) / float64(m.Scored)
	}
	return m
}

// QWK is quadratic weighted kappa: agreement with golden, corrected for the
// agreement chance alone would produce. 1.0 is perfect, 0.0 is none beyond
// chance, below 0 is systematic disagreement. The quadratic weighting means a
// miss by 5 counts far more than five misses by 1.
//
// It answers a question none of the other metrics can. MAE is in score units,
// so it depends on how the golden scores are spread and cannot be compared
// across fixtures; QWK is scale-free and comparable. It is also the metric that
// catches a degenerate model outright, because a scorer that answers with one
// constant agrees with chance exactly and lands at 0 however respectable its
// MAE looks.
//
// The second return is false when kappa is undefined, which happens when either
// side used a single value throughout and there is no expected disagreement to
// correct against.
//
// Reported alongside the sample count on purpose: with eleven score categories,
// kappa is volatile on a small fixture and settles as the fixture grows.
func QWK(outcomes []Outcome) (float64, bool) {
	const k = MaxScore - MinScore + 1

	var observed [k][k]float64
	var n float64
	for _, o := range outcomes {
		if !o.Scored {
			continue
		}
		want := clampScore(o.Sample.ExpectedScore())
		got := clampScore(o.Score)
		observed[want][got]++
		n++
	}
	if n == 0 {
		return 0, false
	}

	var wantMargin, gotMargin [k]float64
	for i := range k {
		for j := range k {
			wantMargin[i] += observed[i][j]
			gotMargin[j] += observed[i][j]
		}
	}

	var num, den float64
	for i := range k {
		for j := range k {
			// Squared distance, normalised so the far corners weigh 1.
			w := float64((i-j)*(i-j)) / float64((k-1)*(k-1))
			num += w * observed[i][j]
			den += w * wantMargin[i] * gotMargin[j] / n
		}
	}
	if den == 0 {
		return 0, false
	}
	return 1 - num/den, true
}

func clampScore(s int) int {
	return min(max(s, MinScore), MaxScore)
}

// HighSignalThreshold is the score at or above which the feed page calls an item
// high signal: it earns the tier badge and counts toward the high-signal tile.
// Read from rank so the page and this cannot drift apart.
//
// Nothing is hidden below it. Every scored item appears on the page and in the
// markdown digest whatever it scored, so this is a label the eye sorts by, not
// a filter.
const HighSignalThreshold = rank.HighSignalScore

// SweepThresholds are the cutoffs HighSignalSweep measures, bracketing
// HighSignalThreshold well on both sides so a model scoring far below the badge
// threshold still has rows to show. Below 3 nearly everything qualifies, so the
// rows stop saying anything.
//
// One threshold cannot separate a model that ranks badly from one that ranks
// well and scores low: both flag nothing. Across a sweep they look nothing
// alike. Precision climbing as the cutoff rises means the ordering is sound and
// the cutoff is the only thing misplaced, which is a dial, not a model problem.
// Precision flat near the base rate at every cutoff means the ordering carries
// no signal and no cutoff will rescue it.
var SweepThresholds = []int{3, 4, 5, 6, 7, 8, 9}

// HighSignal measures the one judgement the product publishes: which items it
// puts the high-signal badge on.
//
// This is the only metric here stated in terms a reader feels. MAE is a proxy:
// a model can be two points off everywhere and still pick out exactly the right
// articles, or be close on average and still badge all the wrong ones.
type HighSignal struct {
	Threshold int `json:"threshold"`
	// Flagged is how many the model marks high signal; Wanted how many golden
	// puts at or above the threshold.
	Flagged int `json:"n_flagged"`
	Wanted  int `json:"wanted"`
	// Hit was flagged and wanted, Noise flagged and not wanted, Missed wanted
	// and not flagged.
	Hit    int `json:"hit"`
	Noise  int `json:"noise"`
	Missed int `json:"missed"`
	// Precision and Recall are nil when undefined: precision when nothing was
	// flagged, recall when nothing was wanted. Zero would be a different claim.
	Precision *float64 `json:"precision"`
	Recall    *float64 `json:"recall"`

	// Scored is how many samples the threshold was applied to, and BaseRate the
	// precision a scorer picking at random would reach at it (Wanted/Scored).
	//
	// Precision alone is unreadable without it, and the base rate moves with the
	// threshold: nearly everything clears a low cutoff, so a high precision
	// there is arithmetic rather than skill. Comparing every row against one
	// global base rate would credit exactly that.
	Scored   int      `json:"n_scored"`
	BaseRate *float64 `json:"base_rate"`
}

// HighSignalAt measures precision and recall at a score threshold.
func HighSignalAt(outcomes []Outcome, threshold int) HighSignal {
	d := HighSignal{Threshold: threshold}
	for _, o := range outcomes {
		if !o.Scored {
			continue
		}
		d.Scored++
		wanted := o.Sample.ExpectedScore() >= threshold
		flagged := o.Score >= threshold
		switch {
		case flagged && wanted:
			d.Hit++
		case flagged:
			d.Noise++
		case wanted:
			d.Missed++
		}
	}
	d.Flagged = d.Hit + d.Noise
	d.Wanted = d.Hit + d.Missed

	if d.Scored > 0 {
		base := float64(d.Wanted) / float64(d.Scored)
		d.BaseRate = &base
	}
	if d.Flagged > 0 {
		p := float64(d.Hit) / float64(d.Flagged)
		d.Precision = &p
	}
	if d.Wanted > 0 {
		r := float64(d.Hit) / float64(d.Wanted)
		d.Recall = &r
	}
	return d
}

// HighSignalSweep measures precision and recall at each of SweepThresholds, so the
// cutoff can be read as the dial it is rather than assumed correct.
func HighSignalSweep(outcomes []Outcome) []HighSignal {
	out := make([]HighSignal, 0, len(SweepThresholds))
	for _, t := range SweepThresholds {
		out = append(out, HighSignalAt(outcomes, t))
	}
	return out
}

// TagMetrics is one tag's slice of the run.
type TagMetrics struct {
	Tag Tag
	// Description is the tag's line from the fixture, printed beside the group.
	Description string
	Metrics
}

// MinTagSamples is the smallest group whose MAE is worth reading as a property
// of the group rather than of one article. Below it a tag's MAE is a single
// sample's error wearing the costume of a statistic.
const MinTagSamples = 3

// Thin reports whether the group is too small for its MAE to mean anything.
func (t TagMetrics) Thin() bool { return t.Samples < MinTagSamples }

// ByTag groups outcomes by tag and summarises each, worst MAE first so the
// group needing attention reads first. A sample carrying several tags counts
// toward each, so the groups overlap by design.
//
// This is where a bad number becomes an actionable one: a poor overall MAE says
// only that scoring is off, while one tag standing out says which kind of
// article it is off about.
//
// Groups under MinTagSamples sort after every full one regardless of MAE. The
// ordering exists to put what needs attention at the top, and a one-sample
// group taking that slot from a real one inverts the purpose: it promotes the
// noisiest row in the table precisely because it is the noisiest.
func ByTag(outcomes []Outcome, descriptions map[Tag]string) []TagMetrics {
	grouped := map[Tag][]Outcome{}
	for _, o := range outcomes {
		for _, t := range o.Sample.Tags {
			grouped[t] = append(grouped[t], o)
		}
	}

	out := make([]TagMetrics, 0, len(grouped))
	for tag, group := range grouped {
		out = append(out, TagMetrics{
			Tag:         tag,
			Description: descriptions[tag],
			Metrics:     Compute(group),
		})
	}
	// Thin groups last, then worst first, then by name so equal groups keep a
	// stable order.
	slices.SortFunc(out, func(a, b TagMetrics) int {
		if c := cmp.Compare(boolToInt(a.Thin()), boolToInt(b.Thin())); c != 0 {
			return c
		}
		if c := cmp.Compare(b.MAE, a.MAE); c != 0 {
			return c
		}
		return cmp.Compare(a.Tag, b.Tag)
	})
	return out
}

// Worst returns up to n scored outcomes, furthest from their label first, so
// the report can print the disagreements alongside the note explaining what you
// meant. Ties break by id to keep runs comparable.
func Worst(outcomes []Outcome, n int) []Outcome {
	scored := make([]Outcome, 0, len(outcomes))
	for _, o := range outcomes {
		if o.Scored && o.AbsErr() > 0 {
			scored = append(scored, o)
		}
	}
	slices.SortFunc(scored, func(a, b Outcome) int {
		if c := cmp.Compare(b.AbsErr(), a.AbsErr()); c != 0 {
			return c
		}
		return cmp.Compare(a.Sample.ID, b.Sample.ID)
	})
	if n >= 0 && len(scored) > n {
		scored = scored[:n]
	}
	return scored
}

// Spread reports how much repeated runs of the same fixture disagreed with each
// other. It is the noise floor, and nothing smaller than it should be read as a
// result: ModelTuning carries no temperature or seed, so two identical runs are
// not expected to agree, and a profile edit that moves MAE by less than the
// spread has not been shown to do anything.
type Spread struct {
	Runs int
	// MAEMin and MAEMax bound the aggregate across runs.
	MAEMin, MAEMax float64
	// WidestSample is the largest gap between the highest and lowest score any
	// single sample was given across runs, and the id it belongs to.
	WidestSample   int
	WidestSampleID string
}

// SpreadOf measures the disagreement across repeated runs. Each element of runs
// is one full pass over the fixture. Fewer than two runs measures nothing.
func SpreadOf(runs [][]Outcome) Spread {
	s := Spread{Runs: len(runs)}
	if len(runs) < 2 {
		return s
	}

	for i, run := range runs {
		mae := Compute(run).MAE
		if i == 0 || mae < s.MAEMin {
			s.MAEMin = mae
		}
		if i == 0 || mae > s.MAEMax {
			s.MAEMax = mae
		}
	}

	type bounds struct{ lo, hi int }
	seen := map[string]bounds{}
	for _, run := range runs {
		for _, o := range run {
			if !o.Scored {
				continue
			}
			b, ok := seen[o.Sample.ID]
			if !ok {
				seen[o.Sample.ID] = bounds{o.Score, o.Score}
				continue
			}
			b.lo = min(b.lo, o.Score)
			b.hi = max(b.hi, o.Score)
			seen[o.Sample.ID] = b
		}
	}
	// Sorted so a tie between samples resolves the same way every run.
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		if swing := seen[id].hi - seen[id].lo; swing > s.WidestSample {
			s.WidestSample, s.WidestSampleID = swing, id
		}
	}
	return s
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
