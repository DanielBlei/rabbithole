// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"fmt"
	"io"
	"strings"
)

// WriteMarkdown renders the report as a document, for pasting into an issue or
// a pull request. Same sections and same order as the text report; only the
// styling differs, and it derives nothing of its own.
func (r *Report) WriteMarkdown(w io.Writer, opt RenderOptions) error {
	var b strings.Builder

	fmt.Fprintf(&b, "# Benchmark report: %s\n\n", r.benchmarkName())

	think := "off"
	if r.Info.Think {
		think = "on"
	}
	fmt.Fprintf(&b, "%s · %s · think %s · batch %d",
		r.Info.StartedAt.Format("2006-01-02 15:04 MST"), r.backend(), think, r.Info.BatchSize)
	if r.Info.Repeats > 1 {
		fmt.Fprintf(&b, " · %d runs", r.Info.Repeats)
	}
	fmt.Fprintf(&b, "\n%s · %s", r.sampleCount(), formatDuration(r.Info.Duration()))
	if per := r.perSample(); per > 0 {
		fmt.Fprintf(&b, " · %s/sample", formatDuration(per))
	}
	fmt.Fprintf(&b, "\nprofile %s · prompt %s · benchmark %s\n\n",
		short(r.Info.ProfileHash), short(r.Info.PromptHash), short(r.Info.DatasetHash))
	fmt.Fprintf(&b, "%s\n\n", intro)

	r.markdownWarnings(&b)
	r.markdownScoring(&b)
	r.markdownHighSignal(&b)
	r.markdownSweep(&b)
	r.markdownWithin(&b)
	r.markdownHistogram(&b)
	r.markdownByTag(&b)
	r.markdownSamples(&b, opt)
	r.markdownNoiseFloor(&b)

	b.WriteString("---\n\nThe model has no temperature or seed set, so re-running will not " +
		"reproduce these numbers exactly.\n")

	_, err := io.WriteString(w, b.String())
	return err
}

// markdownWarnings leads the document, because everything below it is worth
// less when one of these fired.
func (r *Report) markdownWarnings(b *strings.Builder) {
	for _, warning := range r.Warnings {
		fmt.Fprintf(b, "> **%s**\n\n", warning.Text)
	}
}

func (r *Report) markdownScoring(b *strings.Builder) {
	res := r.Results
	b.WriteString("## Scoring\n\n")
	fmt.Fprintf(b, "%d scored, %d unanswered", res.Scored, res.Missing)
	if note := r.scoringNote(); note != "" {
		fmt.Fprintf(b, " · %s", note)
	}
	b.WriteString("\n\n")

	values := r.scoringValues()
	b.WriteString("| metric | value | range | measures |\n|---|---|---|---|\n")
	for i, metric := range scoringMetrics {
		value := fmt.Sprintf("%.2f", values[i])
		switch metric.Key {
		case "qwk":
			value = "—"
			if res.QWK != nil {
				value = fmt.Sprintf("%+.2f", *res.QWK)
			}
		case "signed_mean":
			value = fmt.Sprintf("%+.2f", values[i])
		}
		fmt.Fprintf(b, "| %s | %s | %s | %s |\n", metric.Label, value, metric.Range(), metric.About)
	}
	b.WriteString("\n")
}

func (r *Report) markdownHighSignal(b *strings.Builder) {
	d := r.Results.HighSignal
	if d.Flagged == 0 && d.Wanted == 0 {
		return
	}
	b.WriteString("## Is the high-signal tier trustworthy?\n\n")
	fmt.Fprintf(b, "At the %d+ mark the feed page badges as high signal. Nothing is hidden "+
		"below it, so this is about whether the badge tells the truth rather than what "+
		"reaches the page.\n\n", d.Threshold)

	b.WriteString("| metric | value | range | measures |\n|---|---|---|---|\n")
	for _, metric := range highSignalMetrics {
		value := d.Precision
		if metric.Key == "high_signal.recall" {
			value = d.Recall
		}
		cell := "—"
		if value != nil {
			cell = fmt.Sprintf("%.2f", *value)
		}
		fmt.Fprintf(b, "| %s | %s | %s | %s |\n", metric.Label, cell, metric.Range(), metric.About)
	}
	fmt.Fprintf(b, "\n- the model badges **%d**; golden says **%d** deserve it\n",
		d.Flagged, d.Wanted)
	if d.Precision == nil {
		b.WriteString("- nothing reached the mark, so the tier would have been empty\n")
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

// markdownSweep is the same measure at marks either side of the badge, so one
// mark is not the whole picture. The reading is the reader's to make.
func (r *Report) markdownSweep(b *strings.Builder) {
	sweep := r.Results.HighSignalSweep
	if len(sweep) == 0 {
		return
	}
	b.WriteString("At other marks:\n\n")
	b.WriteString("| mark | badged | wanted | precision | chance | recall |\n|---|---|---|---|---|---|\n")
	for _, s := range sweep {
		fmt.Fprintf(b, "| %d+ | %d | %d | %s | %s | %s |\n",
			s.Threshold, s.Flagged, s.Wanted,
			optionalRate(s.Precision), optionalRate(s.BaseRate), optionalRate(s.Recall))
	}
	fmt.Fprintf(b, "\n%s\n\n", sweepNote)
}

func (r *Report) markdownWithin(b *strings.Builder) {
	res := r.Results
	if res.Scored == 0 {
		return
	}
	b.WriteString("## Distance from golden\n\n")
	fmt.Fprintf(b, "- exact: **%d of %d**\n", res.Within["0"], res.Scored)
	for n := 1; n < 4; n++ {
		fmt.Fprintf(b, "- within %d: %d of %d\n", n, res.Within[fmt.Sprintf("%d", n)], res.Scored)
	}
	used, labelled := distinct(res.ScoreSpread), distinct(res.LabelSpread)
	fmt.Fprintf(b, "\nScores the model used: %s (%d distinct, against %d in golden)\n\n",
		usedScores(res.ScoreSpread), used, labelled)
}

func (r *Report) markdownHistogram(b *strings.Builder) {
	buckets := r.Results.Histogram
	if len(buckets) == 0 {
		return
	}
	b.WriteString("## Error distribution\n\nSigned, model minus golden.\n\n```\n")
	var widest int
	for _, h := range buckets {
		widest = max(widest, h.Count)
	}
	for _, h := range buckets {
		fmt.Fprintf(b, "%+3d  %-24s %d\n", h.Err, strings.Repeat("█", scaleBar(h.Count, widest)), h.Count)
	}
	b.WriteString("```\n\n")
}

func (r *Report) markdownByTag(b *strings.Builder) {
	if len(r.ByTag) == 0 {
		return
	}
	b.WriteString("## By tag\n\nWorst first.\n\n")
	b.WriteString("| tag | n | mae | signed | description |\n|---|---|---|---|---|\n")
	var thin bool
	for _, t := range r.ByTag {
		mark := ""
		if t.Thin {
			mark, thin = " \\*", true
		}
		fmt.Fprintf(b, "| %s%s | %s | %.2f | %+.2f | %s |\n",
			t.Tag, mark, tagCount(t), t.MAE, t.SignedMean, t.Description)
	}
	b.WriteString("\n")

	if thin {
		fmt.Fprintf(b, "\\* fewer than %d samples: that MAE is one or two articles, not a "+
			"property of the group. Add samples to the tag before acting on it.\n\n",
			MinTagSamples)
	}
}

func (r *Report) markdownSamples(b *strings.Builder, opt RenderOptions) {
	if len(r.Samples) == 0 {
		return
	}
	b.WriteString("## Samples\n\nWorst first.\n\n")
	b.WriteString("| id | golden | model | diff | title |\n|---|---|---|---|---|\n")
	for _, s := range r.Samples {
		score, diff := signedScore(s)
		fmt.Fprintf(b, "| %s | %d | %s | %s | %s |\n", s.ID, s.Expected, score, diff, s.Title)
	}
	b.WriteString("\n")

	if !opt.ShowWhy {
		return
	}
	b.WriteString("## Why\n\nThe model's reason and your golden note, worst first.\n\n")
	for _, s := range r.Samples {
		fmt.Fprintf(b, "**%s** %s", s.ID, sampleVerdict(s))
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
}

func (r *Report) markdownNoiseFloor(b *strings.Builder) {
	s := r.Spread
	if s == nil {
		return
	}
	fmt.Fprintf(b, "## Noise floor\n\n%d runs of the same dataset, unchanged.\n\n", s.Runs)
	b.WriteString("| run | mae | time |\n|---|---|---|\n")
	for i, mae := range s.MAEPerRun {
		elapsed := "—"
		if i < len(s.RunSeconds) {
			elapsed = formatDuration(secondsToDuration(s.RunSeconds[i]))
		}
		fmt.Fprintf(b, "| %d | %.2f | %s |\n", i+1, mae, elapsed)
	}
	b.WriteString("\n")
	if s.WidestSample > 0 {
		fmt.Fprintf(b, "Widest single-sample swing %d points (%s).\n\n", s.WidestSample, s.WidestSampleID)
	}
	b.WriteString("A change smaller than this is not evidence of anything.\n\n")
}
