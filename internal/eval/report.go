// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
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

// Report is the computed result of a benchmark run and the single source every
// renderer draws from. Markdown adds nothing to it and computes nothing of its
// own, so a second renderer (HTML, say) stays a pure function of this shape.
type Report struct {
	Info    RunInfo         `json:"config"`
	Timing  TimingBlock     `json:"timing"`
	Results ResultsBlock    `json:"results"`
	ByTag   []TagRow        `json:"by_tag"`
	Samples []SampleRow     `json:"samples"`
	Spread  *SpreadBlock    `json:"spread,omitempty"`
	Rated   map[string]bool `json:"higher_is_better"`
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
	Runs           int       `json:"runs"`
	MAEPerRun      []float64 `json:"mae_per_run"`
	MAEMin         float64   `json:"mae_min"`
	MAEMax         float64   `json:"mae_max"`
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
		Info:  res.Info,
		Rated: HigherIsBetter,
		Results: ResultsBlock{
			Samples: m.Samples, Scored: m.Scored, Missing: m.Missing,
			MAE: m.MAE, RMSE: m.RMSE, SignedMean: m.SignedMean,
			HighSignal:      HighSignalAt(first, HighSignalThreshold),
			HighSignalSweep: HighSignalSweep(first),
			Within:          map[string]int{},
			Histogram:       histogram(m),
		},
	}
	if len(res.Elapsed) > 0 {
		secs := res.Elapsed[0].Seconds()
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

	if len(res.Runs) > 1 {
		s := SpreadOf(res.Runs)
		block := &SpreadBlock{
			Runs: s.Runs, MAEMin: s.MAEMin, MAEMax: s.MAEMax,
			WidestSample: s.WidestSample, WidestSampleID: s.WidestSampleID,
		}
		for _, run := range res.Runs {
			block.MAEPerRun = append(block.MAEPerRun, Compute(run).MAE)
		}
		rep.Spread = block
	}
	return rep
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

// WriteJSON writes the machine-readable report. Two of these can be diffed to
// answer whether an edit to the profile or the prompt moved anything.
func (r *Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// WriteMarkdown renders the report for reading. It is a projection of the same
// values WriteJSON emits and derives nothing of its own.
func (r *Report) WriteMarkdown(w io.Writer) error {
	var b strings.Builder

	name := r.Info.Benchmark
	if name == "" {
		name = "benchmark"
	}
	fmt.Fprintf(&b, "# Benchmark report: %s\n\n", name)

	think := "off"
	if r.Info.Think {
		think = "on"
	}
	// A model-free backend has no model to name.
	backend := r.Info.Provider
	if r.Info.Model != "" {
		backend = r.Info.Model + " via " + r.Info.Provider
	}
	fmt.Fprintf(&b, "%s · %s · think %s · batch %d",
		r.Info.StartedAt.Format("2006-01-02 15:04 MST"), backend, think, r.Info.BatchSize)
	if r.Info.Repeats > 1 {
		fmt.Fprintf(&b, " · %d runs", r.Info.Repeats)
	}
	if r.Timing.Seconds > 0 {
		fmt.Fprintf(&b, " · %s (%s/sample)",
			duration(r.Timing.Seconds), duration(r.Timing.PerSample))
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "profile %s · prompt %s · benchmark %s\n\n",
		short(r.Info.ProfileHash), short(r.Info.PromptHash), short(r.Info.DatasetHash))

	res := r.Results
	b.WriteString("## Summary\n\n")
	fmt.Fprintf(&b, "%d samples, %d scored", res.Samples, res.Scored)
	if res.Missing > 0 {
		fmt.Fprintf(&b, ", **%d unanswered** (excluded from every metric below)", res.Missing)
	}
	b.WriteString("\n\n")

	// Nothing scored means nothing measured. Every aggregate below is computed
	// over the scored samples, so with none of them MAE and RMSE are 0 and the
	// table would report a flawless run against a backend that answered
	// nothing at all.
	if res.Scored == 0 {
		b.WriteString("**No sample was scored, so nothing here was measured.** " +
			"Every metric is computed over answered samples and there were none. " +
			"Check the backend is reachable and that the model is returning parseable JSON.\n\n")
		r.writeDisagreements(&b)
		_, err := io.WriteString(w, b.String())
		return err
	}

	fmt.Fprintf(&b, "| | value | what it means |\n|---|---|---|\n")
	if res.QWK != nil {
		fmt.Fprintf(&b, "| **score agreement** (QWK) | **%+.2f** | %s |\n", *res.QWK, kappaReading(*res.QWK))
	}
	fmt.Fprintf(&b, "| average miss (MAE) | %.2f | model sits %.1f points from golden, on average |\n",
		res.MAE, res.MAE)
	fmt.Fprintf(&b, "| large misses (RMSE) | %.2f | %s |\n", res.RMSE, missShape(res.MAE, res.RMSE))
	fmt.Fprintf(&b, "| score offset (signed mean) | %+.2f | %s |\n", res.SignedMean, offset(res.SignedMean))
	b.WriteString("\n")

	if v := poorAgreement(res.QWK); v != "" {
		fmt.Fprintf(&b, "%s\n\n", v)
	}
	if res.QWK != nil && res.Scored < smallFixture {
		fmt.Fprintf(&b, "Score agreement may be volatile on a small fixture dataset "+
			"(%d samples here; %d or more steadies it).\n\n", res.Scored, smallFixture)
	}

	if res.Scored > 0 {
		fmt.Fprintf(&b, "Landed on golden exactly: **%d of %d**", res.Within["0"], res.Scored)
		for n := 1; n < 4; n++ {
			fmt.Fprintf(&b, " · within %d point", n)
			if n > 1 {
				b.WriteString("s")
			}
			fmt.Fprintf(&b, ": %d", res.Within[fmt.Sprintf("%d", n)])
		}
		b.WriteString("\n\n")

		used, labelled := distinct(res.ScoreSpread), distinct(res.LabelSpread)
		fmt.Fprintf(&b, "Scores the model used: %s (%d different, against %d in golden)\n\n",
			usedScores(res.ScoreSpread), used, labelled)

		// Two values out of eleven is not scoring, it is a constant. Say so
		// loudly: every metric above will look respectable wherever the labels
		// happen to agree with that constant, and the tag table below will
		// attribute the accident to whichever tags those samples carry.
		if used <= 2 && res.Scored > 2 {
			fmt.Fprintf(&b, "> **The model gave nearly every sample the same score: %d different "+
				"values across %d samples.** It is not telling them apart, so it agrees with "+
				"golden only where golden happens to land on that same value. Everything below "+
				"is that coincidence, not a judgement. Try a larger model first.\n\n",
				used, res.Scored)
		}
	}

	if len(res.Histogram) > 0 {
		b.WriteString("## Error distribution\n\n```\n")
		widest := 0
		for _, h := range res.Histogram {
			widest = max(widest, h.Count)
		}
		for _, h := range res.Histogram {
			bar := strings.Repeat("█", scaleBar(h.Count, widest))
			fmt.Fprintf(&b, "%+3d  %-24s %d\n", h.Err, bar, h.Count)
		}
		b.WriteString("```\n\n")
	}

	r.writeByTag(&b)

	r.writeHighSignal(&b)
	r.writeSweep(&b)
	r.writeAgreement(&b)
	r.writeDisagreements(&b)

	if s := r.Spread; s != nil {
		b.WriteString("## Noise floor\n\n")
		fmt.Fprintf(&b, "%d runs of the same fixture, unchanged: MAE ranged %.2f to %.2f.\n",
			s.Runs, s.MAEMin, s.MAEMax)
		if s.WidestSample > 0 {
			fmt.Fprintf(&b, "The widest single-sample swing was %d points (%s).\n", s.WidestSample, s.WidestSampleID)
		}
		fmt.Fprintf(&b, "\nA change smaller than this is not evidence of anything.\n\n")
	}

	b.WriteString("---\n\n**What next.** Consider tuning your profile where the model's marks " +
		"missed yours: each mismatch above pairs its reasoning with your note, which often shows " +
		"the line worth rewording. Run again to compare.\n\n")
	b.WriteString("One sample, not a measurement: the model has no temperature or seed set, ")
	b.WriteString("so re-running will not reproduce these numbers exactly.\n")

	_, err := io.WriteString(w, b.String())
	return err
}

// writeByTag prints each tag's slice of the run, worst first, with groups too
// small to read marked and pushed below the rest.
func (r *Report) writeByTag(b *strings.Builder) {
	if len(r.ByTag) == 0 {
		return
	}
	b.WriteString("## By tag\n\nWorst first.\n\n")

	var thin bool
	tw := tabwriter.NewWriter(b, 0, 2, 2, ' ', 0)
	row(tw, "TAG\tN\tMAE\tSIGNED\t\n")
	for _, t := range r.ByTag {
		mark := ""
		if t.Thin {
			mark, thin = " *", true
		}
		row(tw, "%s%s\t%d\t%.2f\t%+.2f\t%s\n",
			t.Tag, mark, t.Samples, t.MAE, t.SignedMean, t.Description)
	}
	_ = tw.Flush()
	b.WriteString("\n")

	if thin {
		fmt.Fprintf(b, "\\* fewer than %d samples: that MAE is one or two articles, not a "+
			"property of the group. Add samples to the tag before acting on it.\n\n",
			MinTagSamples)
	}
}

// highSignalWritten reports whether writeHighSignal emitted its section, so the
// sweep knows whether it still needs the heading.
func (r *Report) highSignalWritten() bool {
	d := r.Results.HighSignal
	return d.Flagged != 0 || d.Wanted != 0
}

// writeHighSignal reports the one judgement the product publishes, in the terms
// a reader feels: of what it badged as high signal, how much did you want.
//
// Nothing is hidden below the threshold, so this is about what the badge and the
// high-signal tile claim, not about what reaches the page.
func (r *Report) writeHighSignal(b *strings.Builder) {
	if !r.highSignalWritten() {
		return
	}
	d := r.Results.HighSignal
	fmt.Fprintf(b, "## Is the high-signal tier trustworthy?\n\n")
	fmt.Fprintf(b, "At the %d+ mark the feed page badges as high signal:\n\n", d.Threshold)
	fmt.Fprintf(b, "- the model badges **%d**; golden says **%d** deserve it\n", d.Flagged, d.Wanted)

	if d.Precision == nil {
		fmt.Fprintf(b, "- nothing reached the mark, so the tier would have been empty\n")
	} else {
		fmt.Fprintf(b, "- precision **%.2f** — %d of the %d it badges do not deserve it\n",
			*d.Precision, d.Noise, d.Flagged)
	}
	if d.Recall != nil {
		fmt.Fprintf(b, "- recall **%.2f** — it misses %d of the %d that do\n",
			*d.Recall, d.Missed, d.Wanted)
	}
	b.WriteString("\n")
}

// writeSweep prints precision and recall at marks either side of the badge
// threshold, which is what turns a bad result into a diagnosis.
//
// One mark cannot tell a model that ranks badly from one that ranks well and
// scores everything low: both badge nothing. Read down the precision column
// instead. Climbing steadily means the ordering is sound and the mark is the
// only thing misplaced. Flat near the base rate at every mark means the
// ordering carries no signal and moving the mark will not help.
func (r *Report) writeSweep(b *strings.Builder) {
	sweep := r.Results.HighSignalSweep
	if len(sweep) == 0 {
		return
	}
	if !r.highSignalWritten() {
		b.WriteString("## Is the high-signal tier trustworthy?\n\n")
	}
	b.WriteString("At other marks:\n\n```\n")
	tw := tabwriter.NewWriter(b, 0, 2, 2, ' ', 0)
	row(tw, "MARK\tBADGED\tWANTED\tPRECISION\tCHANCE\tRECALL\t\n")
	for _, d := range sweep {
		// The badge threshold gets no marker of its own: the section above names
		// it in the sentence right before this table, and calling it "live" here
		// implied it was a dial someone had set, which it is not.
		row(tw, "%d+\t%d\t%d\t%s\t%s\t%s\t\n",
			d.Threshold, d.Flagged, d.Wanted, ratio(d.Precision), ratio(d.BaseRate), ratio(d.Recall))
	}
	_ = tw.Flush()
	b.WriteString("```\n\n")

	b.WriteString("CHANCE is the precision a scorer picking at random would reach at that " +
		"mark, and it is the only thing that makes the precision column readable: nearly " +
		"everything clears a low mark, so a high precision there is arithmetic rather than " +
		"skill. Precision pulling further ahead of chance as the mark rises means the model " +
		"ranks well and only the mark is misplaced, which is a dial rather than a model " +
		"problem. Precision tracking chance at every mark means the ordering carries no " +
		"signal and moving the mark will not rescue it.\n\n")
}

// ratio renders an optional rate, distinguishing undefined from zero.
func ratio(v *float64) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%.2f", *v)
}

// writeAgreement lists the samples the model landed exactly on, so the report
// shows what is working and not only what is broken. It comes before the
// disagreements: read together with the score spread above, a long agreement
// list built from one repeated score is a warning rather than a result.
func (r *Report) writeAgreement(b *strings.Builder) {
	var rows []SampleRow
	for _, s := range r.Samples {
		if s.Scored && s.Err == 0 {
			rows = append(rows, s)
		}
	}
	if len(rows) == 0 {
		return
	}
	fmt.Fprintf(b, "## Agreement\n\n%d of %d matched golden exactly.\n\n", len(rows), r.Results.Samples)
	tw := tabwriter.NewWriter(b, 0, 2, 2, ' ', 0)
	for _, s := range rows {
		row(tw, "%s\t%d\t%s\t%s\n",
			s.ID, s.Score, strings.Join(s.Tags, ", "), truncate(s.Title, 56))
	}
	_ = tw.Flush()
	b.WriteString("\n")
}

// writeDisagreements lists what the model got furthest from, with your own note
// beside it: the report is for reading, and the note is why you said what you
// said.
func (r *Report) writeDisagreements(b *strings.Builder) {
	var rows []SampleRow
	for _, s := range r.Samples {
		if s.Scored && s.Err != 0 {
			rows = append(rows, s)
		}
	}
	slices.SortFunc(rows, func(a, c SampleRow) int {
		if d := abs(c.Err) - abs(a.Err); d != 0 {
			return d
		}
		return strings.Compare(a.ID, c.ID)
	})

	var unanswered []SampleRow
	for _, s := range r.Samples {
		if !s.Scored {
			unanswered = append(unanswered, s)
		}
	}

	if len(rows) == 0 && len(unanswered) == 0 {
		return
	}
	b.WriteString("## Disagreements\n\n")
	for _, s := range rows {
		fmt.Fprintf(b, "**%s** golden %d, model %d (%+d)", s.ID, s.Expected, s.Score, s.Err)
		if len(s.Tags) > 0 {
			fmt.Fprintf(b, " · %s", strings.Join(s.Tags, ", "))
		}
		fmt.Fprintf(b, "\n\n> %s\n\n", s.Title)
		if s.Reason != "" {
			fmt.Fprintf(b, "- model: %s\n", oneLine(s.Reason))
		}
		if s.Note != "" {
			fmt.Fprintf(b, "- golden: %s\n", oneLine(s.Note))
		}
		b.WriteString("\n")
	}
	for _, s := range unanswered {
		fmt.Fprintf(b, "**%s** unanswered: the model returned no score.\n\n> %s\n\n", s.ID, s.Title)
	}
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

// row writes one tabwriter line. Every caller's sink is the report's
// strings.Builder, which cannot fail, so there is no error to act on.
func row(tw *tabwriter.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(tw, format, args...)
}

func short(h string) string {
	h = strings.TrimPrefix(h, "sha256:")
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// smallFixture is roughly where kappa stops being jumpy on an 11-point scale.
const smallFixture = 30

// poorAgreementBelow is where a run stops being worth interpreting. Below it
// the model's scores carry no detectable relationship to golden.
const poorAgreementBelow = 0.2

// kappaReading puts a kappa in words, with the scale attached, since the number
// means nothing to a reader who has not met it before.
//
// The scale note says "none beyond chance" rather than anything like "far from
// ideal", because 0 does not mean poor, it means no relationship was detected:
// a model giving every sample the same score lands exactly on 0, and below 0 is
// systematic inversion. Collapsing that into a badness scale would discard the
// reason this metric is here.
func kappaReading(k float64) string {
	const scale = " (1.0 perfect, 0.0 none beyond chance)"
	// Guarded at the rounding boundary rather than at 0, so a kappa that prints
	// as -0.00 is not labelled as an inversion it is too small to be.
	switch {
	case k < -0.005:
		return "systematic disagreement" + scale
	case k < poorAgreementBelow:
		return "no agreement beyond chance" + scale
	case k < 0.4:
		return "slight agreement" + scale
	case k < 0.6:
		return "moderate agreement" + scale
	case k < 0.8:
		return "substantial agreement" + scale
	default:
		return "near-perfect agreement" + scale
	}
}

// missShape reads the gap between MAE and RMSE. They are equal when every miss
// is the same size, and RMSE pulls away as a few large ones dominate.
func missShape(mae, rmse float64) string {
	if mae == 0 {
		return "nothing missed"
	}
	if rmse > mae*1.5 {
		return "well above the average, so a few samples carry most of the error"
	}
	return "level with the average, so no single sample dominates"
}

// offset says which way the model leans and by how much. A consistent lean and
// scattered misses want different fixes, which is why this sits beside the
// average rather than inside it.
func offset(signed float64) string {
	switch {
	case signed > 0.5:
		return fmt.Sprintf("model scores %.1f points above golden, consistently", signed)
	case signed < -0.5:
		return fmt.Sprintf("model scores %.1f points below golden, consistently", -signed)
	default:
		return "no consistent lean, so the misses fall on both sides"
	}
}

// poorAgreement flags the one result that is not a matter of interpretation:
// scores with no detectable relationship to golden, where nothing further down
// can be read as evidence about the profile or the prompt.
//
// Deliberately the only judgement the report makes. What counts as good enough
// above this line depends on what the feed is for, and that call belongs to
// whoever is reading, not to a threshold picked here.
func poorAgreement(qwk *float64) string {
	if qwk == nil || *qwk >= poorAgreementBelow {
		return ""
	}
	return "**Scores show no reliable agreement with golden.** Consider a larger model before " +
		"reading anything below as a judgement of the profile or the prompt."
}

// scaleBar sizes a histogram bar, never returning 0 for a non-empty bucket so a
// single-sample column stays visible.
func scaleBar(count, widest int) int {
	const width = 24
	if count == 0 || widest == 0 {
		return 0
	}
	n := count * width / widest
	return max(n, 1)
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// oneLine flattens a block-scalar note onto a single line so it sits inside a
// list item.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// truncate shortens s to n runes, rune-safe so a multibyte title is not cut
// mid-character.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// usedScores renders the scores that were given and how often, e.g. "6×1 9×11".
func usedScores(buckets []ScoreBucket) string {
	var parts []string
	for _, b := range buckets {
		if b.Count > 0 {
			parts = append(parts, fmt.Sprintf("%d×%d", b.Score, b.Count))
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, " ")
}
