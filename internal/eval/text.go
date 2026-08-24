// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// Column budgets. Every table has at least one column fed by the golden file,
// and without a cap one long title or tag re-flows the whole table past the
// terminal edge. The numbers are chosen so the widest possible line lands on
// textWidth, which TestTextFitsTheWidth holds them to.
//
// The width is 120 rather than the traditional 80 because the columns being cut
// are the ones carrying the intent: a title, a tag description and your note say
// why a mark is what it is, and truncating them to fit an 80-column terminal
// removes exactly the part worth reading.
const (
	textWidth   = 120
	labelWidth  = 10
	titleWidth  = 76
	sampleIDCol = 12
	tagWidth    = 26
	tagDescCol  = 64
)

// WriteText renders the report for a terminal: aligned columns, no markup, no
// colour, and the same bytes whether it lands on a TTY or in a file.
//
// It builds the whole report before writing a byte, so a failed write leaves no
// half-rendered file behind.
func (r *Report) WriteText(w io.Writer, opt RenderOptions) error {
	var b strings.Builder

	r.textHeader(&b)
	r.textScoring(&b)
	r.textHighSignal(&b)
	r.textSweep(&b)
	r.textWithin(&b)
	r.textHistogram(&b)
	r.textScoreSpread(&b)
	r.textByTag(&b)
	r.textSamples(&b)
	if opt.ShowWhy {
		r.textWhy(&b)
	}
	r.textWarnings(&b)
	r.textNoiseFloor(&b)

	b.WriteString("The model has no temperature or seed set, so re-running will\n")
	b.WriteString("not reproduce these numbers exactly.\n")

	_, err := io.WriteString(w, b.String())
	return err
}

// textHeader states what produced these numbers, and how long it took.
func (r *Report) textHeader(b *strings.Builder) {
	summary := fmt.Sprintf(" · %s", r.sampleCount())
	if n := len(r.ByTag); n > 0 {
		summary += fmt.Sprintf(" · %d tags", n)
	}
	headerLine(b, "BENCHMARK", r.benchmarkName(), summary)

	think := "off"
	if r.Info.Think {
		think = "on"
	}
	headerLine(b, "BACKEND", r.backend(), fmt.Sprintf(" · think %s · batch %d · parallel %d",
		think, r.Info.BatchSize, r.Info.MaxParallel))

	elapsed := formatDuration(r.Info.Duration())
	if r.Info.Repeats > 1 {
		elapsed += fmt.Sprintf(" · %d runs", r.Info.Repeats)
	}
	if per := r.perSample(); per > 0 {
		elapsed += fmt.Sprintf(" · %s/sample", formatDuration(per))
	}
	headerLine(b, "ELAPSED", "", elapsed)
	headerLine(b, "STARTED", "", r.Info.StartedAt.Format("2006-01-02 15:04 MST"))
	headerLine(b, "INPUTS", "", fmt.Sprintf("profile %s · prompt %s · benchmark %s",
		short(r.Info.ProfileHash), short(r.Info.PromptHash), short(r.Info.DatasetHash)))
	b.WriteString("\n")

	wrapInto(b, "", intro)
	b.WriteString("\n")
}

// intro says what the report is for, because the numbers below invite being
// read as a grade. No model matches a hand-marked set exactly, so a run is only
// meaningful next to the run before it.
const intro = "This is for tuning, not grading. No model will match the golden marks perfectly. " +
	"What matters is whether an edit moves the dial. " +
	"If results are far off, your profile or system prompt may be worth fine-tuning."

// headerLine writes a label, a value that may be any length, and a suffix that
// must survive intact. The value is trimmed to whatever the suffix leaves, so a
// long model name shortens instead of pushing the line past the terminal edge.
func headerLine(b *strings.Builder, label, value, suffix string) {
	room := textWidth - labelWidth - 1 - len([]rune(suffix))
	fmt.Fprintf(b, "%-*s %s%s\n", labelWidth, label, truncate(value, room), suffix)
}

// textScoring is the headline table: what each metric measures and where this
// run landed, with no reading of whether that is good.
func (r *Report) textScoring(b *strings.Builder) {
	res := r.Results
	head := fmt.Sprintf("%d scored, %d unanswered", res.Scored, res.Missing)
	if note := r.scoringNote(); note != "" {
		head += " · " + note
	}
	section(b, "SCORING", head)

	values := r.scoringValues()
	table := newTable(b)
	table.row("  METRIC\t VALUE\tRANGE\tMEASURES\n")
	for i, metric := range scoringMetrics {
		value := fmt.Sprintf("%6.2f", values[i])
		switch metric.Key {
		case "qwk":
			// Undefined when either side used one value throughout, in which
			// case there is no number to print and the warnings say why.
			value = fmt.Sprintf("%6s", "—")
			if res.QWK != nil {
				value = fmt.Sprintf("%+6.2f", *res.QWK)
			}
		case "signed_mean":
			value = fmt.Sprintf("%+6.2f", values[i])
		}
		table.row("  %s\t%s\t%s\t%s\n",
			metric.Label, value, metric.Range(), truncate(metric.About, aboutWidth))
	}
	table.flush()
	b.WriteString("\n")
}

// textHighSignal reports the decision the product makes: of what the feed page
// would have badged as high signal, how much you wanted.
func (r *Report) textHighSignal(b *strings.Builder) {
	d := r.Results.HighSignal
	if d.Flagged == 0 && d.Wanted == 0 {
		return
	}
	section(b, fmt.Sprintf("HIGH SIGNAL AT %d+", d.Threshold),
		"is the tier the feed page badges trustworthy")

	table := newTable(b)
	table.row("  METRIC\t VALUE\tRANGE\tMEASURES\n")
	for _, metric := range highSignalMetrics {
		value := d.Precision
		if metric.Key == "high_signal.recall" {
			value = d.Recall
		}
		cell := fmt.Sprintf("%6s", "—")
		if value != nil {
			cell = fmt.Sprintf("%6.2f", *value)
		}
		table.row("  %s\t%s\t%s\t%s\n",
			metric.Label, cell, metric.Range(), truncate(metric.About, aboutWidth))
	}
	table.flush()

	fmt.Fprintf(b, "  the model badges %d · golden says %d deserve it\n", d.Flagged, d.Wanted)
	if d.Precision == nil {
		b.WriteString("  nothing reached the mark, so the tier would have been empty\n")
	} else {
		fmt.Fprintf(b, "  precision %.2f — %d of the %d it badges do not deserve it\n",
			*d.Precision, d.Noise, d.Flagged)
	}
	if d.Recall != nil {
		fmt.Fprintf(b, "  recall %.2f — it misses %d of the %d that do\n",
			*d.Recall, d.Missed, d.Wanted)
	}
	b.WriteString("\n")
}

// textSweep prints the same measure at marks either side of the badge, so one
// mark is not the whole picture. The reading is the reader's to make.
func (r *Report) textSweep(b *strings.Builder) {
	sweep := r.Results.HighSignalSweep
	if len(sweep) == 0 {
		return
	}
	section(b, "AT OTHER MARKS", "")

	table := newTable(b)
	table.row("  MARK\tBADGED\tWANTED\tPRECISION\tCHANCE\tRECALL\n")
	for _, s := range sweep {
		// The badge mark gets no marker of its own: the section above names it
		// in the line right before this table.
		table.row("  %d+\t%d\t%d\t%s\t%s\t%s\n",
			s.Threshold, s.Flagged, s.Wanted,
			optionalRate(s.Precision), optionalRate(s.BaseRate), optionalRate(s.Recall))
	}
	table.flush()
	wrapInto(b, "  ", sweepNote)
	b.WriteString("\n")
}

// sweepNote defines CHANCE and stops there. Without it the precision column
// cannot be read at all; past it, the reading belongs to whoever is reading.
const sweepNote = "CHANCE is the precision a scorer picking at random would reach at that mark."

// optionalRate renders a rate that may be undefined, which is a different claim
// from zero and has to stay distinguishable from it.
func optionalRate(v *float64) string {
	if v == nil {
		return "—"
	}
	return fmt.Sprintf("%.2f", *v)
}

// textWithin shows how the misses are distributed against a few tolerances,
// rather than arguing for one.
func (r *Report) textWithin(b *strings.Builder) {
	res := r.Results
	if res.Scored == 0 {
		return
	}
	section(b, "DISTANCE FROM GOLDEN", "")

	table := newTable(b)
	table.row("  exact\t%d of %d\n", res.Within["0"], res.Scored)
	for n := 1; n < 4; n++ {
		table.row("  within %d\t%d of %d\n", n, res.Within[fmt.Sprintf("%d", n)], res.Scored)
	}
	table.flush()
	b.WriteString("\n")
}

func (r *Report) textHistogram(b *strings.Builder) {
	buckets := r.Results.Histogram
	if len(buckets) == 0 {
		return
	}
	section(b, "ERROR DISTRIBUTION", "signed, model minus golden")

	var widest int
	for _, h := range buckets {
		widest = max(widest, h.Count)
	}
	for _, h := range buckets {
		bar := strings.Repeat("█", scaleBar(h.Count, widest))
		fmt.Fprintf(b, "  %+3d  %-24s %d\n", h.Err, bar, h.Count)
	}
	b.WriteString("\n")
}

// textScoreSpread is what shows a model answering with two values out of
// eleven, which no other number here would reveal.
func (r *Report) textScoreSpread(b *strings.Builder) {
	res := r.Results
	if res.Scored == 0 {
		return
	}
	used, labelled := distinct(res.ScoreSpread), distinct(res.LabelSpread)
	section(b, "SCORE SPREAD",
		fmt.Sprintf("%d distinct values used, golden uses %d", used, labelled))
	fmt.Fprintf(b, "  model   %s\n", usedScores(res.ScoreSpread))
	fmt.Fprintf(b, "  golden  %s\n\n", usedScores(res.LabelSpread))
}

// textByTag is where a bad number becomes an actionable one: a poor overall MAE
// says only that scoring is off, while one tag standing out says which kind of
// article it is off about.
func (r *Report) textByTag(b *strings.Builder) {
	if len(r.ByTag) == 0 {
		return
	}
	section(b, "BY TAG", "worst first")

	var thin bool
	table := newTable(b)
	table.row("  TAG\t    N\t  MAE\tSIGNED\tDESCRIPTION\n")
	for _, t := range r.ByTag {
		mark := ""
		if t.Thin {
			mark, thin = " *", true
		}
		table.row("  %s\t%5s\t%5.2f\t%+6.2f\t%s\n",
			truncate(t.Tag, tagWidth)+mark, tagCount(t), t.MAE, t.SignedMean,
			truncate(t.Description, tagDescCol))
	}
	table.flush()
	b.WriteString("\n")

	if thin {
		wrapInto(b, "  ", fmt.Sprintf("* fewer than %d samples: that MAE is one or two articles, "+
			"not a property of the group. Add samples to the tag before acting on it.",
			MinTagSamples))
		b.WriteString("\n")
	}
}

// textSamples lists every sample the run scored, one line each. The whole set
// rather than only the failures: a long agreement list built from one repeated
// score is a warning, and that is only visible when both are shown together.
func (r *Report) textSamples(b *strings.Builder) {
	if len(r.Samples) == 0 {
		return
	}
	section(b, "SAMPLES", "worst first")

	table := newTable(b)
	table.row("  ID\tGOLDEN\t MODEL\t DIFF\tTITLE\n")
	for _, s := range r.Samples {
		score, diff := signedScore(s)
		table.row("  %s\t%6d\t%6s\t%5s\t%s\n",
			truncate(s.ID, sampleIDCol), s.Expected, score, diff,
			truncate(s.Title, titleWidth))
	}
	table.flush()
	b.WriteString("\n")
}

// textWhy prints the reasoning on both sides, which is the part that says what
// to change rather than that something is off.
func (r *Report) textWhy(b *strings.Builder) {
	if len(r.Samples) == 0 {
		return
	}
	section(b, "WHY", "the model's reason and your golden note, worst first")

	for _, s := range r.Samples {
		head := fmt.Sprintf("  %s  %s", truncate(s.ID, sampleIDCol), sampleVerdict(s))
		if len(s.Tags) > 0 {
			room := textWidth - len([]rune(head)) - 3
			head += " · " + truncate(strings.Join(s.Tags, ", "), room)
		}
		fmt.Fprintf(b, "%s\n", head)
		wrapInto(b, "    title:  ", s.Title)
		if s.Reason != "" {
			wrapInto(b, "    model:  ", oneLine(s.Reason))
		}
		if s.Note != "" {
			wrapInto(b, "    golden: ", oneLine(s.Note))
		}
		b.WriteString("\n")
	}
}

// textWarnings prints the only judgements the report makes, and prints nothing
// when there are none.
func (r *Report) textWarnings(b *strings.Builder) {
	if len(r.Warnings) == 0 {
		return
	}
	section(b, "WARNINGS", "")
	for _, warning := range r.Warnings {
		wrapInto(b, "  ", warning.Text)
		b.WriteString("\n")
	}
}

// textNoiseFloor reports what the same fixture does when nothing changed, which
// is the bar any claimed improvement has to clear.
func (r *Report) textNoiseFloor(b *strings.Builder) {
	s := r.Spread
	if s == nil {
		return
	}
	section(b, "NOISE FLOOR", fmt.Sprintf("%d runs of the same dataset, unchanged", s.Runs))

	table := newTable(b)
	table.row("  RUN\t  MAE\t    TIME\n")
	for i, mae := range s.MAEPerRun {
		elapsed := "—"
		if i < len(s.RunSeconds) {
			elapsed = formatDuration(secondsToDuration(s.RunSeconds[i]))
		}
		table.row("  %3d\t%5.2f\t%8s\n", i+1, mae, elapsed)
	}
	table.flush()

	if s.WidestSample > 0 {
		fmt.Fprintf(b, "  Widest single-sample swing %d points (%s).\n", s.WidestSample, s.WidestSampleID)
	}
	b.WriteString("  A change smaller than this is not evidence of anything.\n\n")
}

// section writes a heading, with an optional subtitle on the same line.
func section(b *strings.Builder, title, subtitle string) {
	if subtitle == "" {
		fmt.Fprintf(b, "%s\n", title)
		return
	}
	fmt.Fprintf(b, "%s  %s\n", title, subtitle)
}

// table writes aligned columns into the report buffer.
//
// Each section builds its own: one shared writer would align columns across
// unrelated sections, which is what makes tabular output look broken. Writes
// land in a strings.Builder and so cannot fail, which is why nothing here
// returns an error.
type table struct {
	w *tabwriter.Writer
}

func newTable(b *strings.Builder) *table {
	return &table{w: tabwriter.NewWriter(b, 0, 2, 2, ' ', 0)}
}

// row writes one tab-separated line.
func (t *table) row(format string, args ...any) {
	_, _ = fmt.Fprintf(t.w, format, args...)
}

// flush lays the collected rows out in columns.
func (t *table) flush() {
	_ = t.w.Flush()
}

// wrapInto writes prose under a prefix, wrapping to the terminal width and
// indenting continuation lines to line up under the first.
func wrapInto(b *strings.Builder, prefix, text string) {
	indent := strings.Repeat(" ", len([]rune(prefix)))
	width := textWidth - len([]rune(prefix))

	for i, line := range wrapText(text, width) {
		if i == 0 {
			fmt.Fprintf(b, "%s%s\n", prefix, line)
			continue
		}
		fmt.Fprintf(b, "%s%s\n", indent, line)
	}
}

// wrapText breaks text on word boundaries at width runes. A single word longer
// than the width gets its own line rather than being cut.
func wrapText(text string, width int) []string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return []string{""}
	}
	var (
		lines []string
		line  strings.Builder
	)
	for _, word := range words {
		switch {
		case line.Len() == 0:
			line.WriteString(word)
		case len([]rune(line.String()))+1+len([]rune(word)) <= width:
			line.WriteString(" " + word)
		default:
			lines = append(lines, line.String())
			line.Reset()
			line.WriteString(word)
		}
	}
	return append(lines, line.String())
}
