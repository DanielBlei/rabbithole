// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// Write renders the report in the requested format. Where the bytes go is the
// caller's business: this only ever writes to w.
func (r *Report) Write(w io.Writer, opt RenderOptions) error {
	switch opt.Format {
	case FormatJSON:
		return r.WriteJSON(w)
	case FormatMarkdown:
		return r.WriteMarkdown(w, opt)
	case FormatText, "":
		return r.WriteText(w, opt)
	default:
		return fmt.Errorf("unknown format %q", opt.Format)
	}
}

// benchmarkName is the dataset's name, or a stand-in when it has none.
func (r *Report) benchmarkName() string {
	if r.Info.Benchmark == "" {
		return "benchmark"
	}
	return r.Info.Benchmark
}

// backend names what did the scoring. A model-free backend has no model to name.
func (r *Report) backend() string {
	if r.Info.Model == "" {
		return r.Info.Provider
	}
	return r.Info.Model + " via " + r.Info.Provider
}

// sampleCount says how much of the golden file this run covers, and says it the
// long way when --limit narrowed it: a partial run must not read like a full one.
func (r *Report) sampleCount() string {
	total := r.Info.DatasetSamples
	if total == 0 || total == r.Results.Samples {
		return fmt.Sprintf("%d samples", r.Results.Samples)
	}
	return fmt.Sprintf("%d of %d samples (--limit %d)", r.Results.Samples, total, r.Info.Limit)
}

// scoringNote qualifies the headline metrics without judging them. Kept out of
// the warnings because it fires on any hand-authored dataset, and a warning that
// always fires stops being read.
func (r *Report) scoringNote() string {
	if r.Results.QWK != nil && r.Results.Scored < smallFixture {
		return fmt.Sprintf("QWK volatile below %d samples", smallFixture)
	}
	return ""
}

// scoringValues pairs each headline metric with its value, nil where undefined.
func (r *Report) scoringValues() []float64 {
	res := r.Results
	qwk := 0.0
	if res.QWK != nil {
		qwk = *res.QWK
	}
	return []float64{qwk, res.MAE, res.RMSE, res.SignedMean}
}

// perSample is the mean wall clock per scored sample across every repeat.
func (r *Report) perSample() time.Duration {
	scored := r.Results.Samples * max(r.Info.Repeats, 1)
	if scored == 0 || r.Info.ElapsedSeconds == 0 {
		return 0
	}
	return r.Info.Duration() / time.Duration(scored)
}

// tagCount renders a tag's size, as "scored/samples" when the model left some
// of the group unanswered. MAE beside it covers only the scored ones, so a bare
// total would describe a group the number was not computed over.
func tagCount(t TagRow) string {
	if t.Scored == t.Samples {
		return fmt.Sprintf("%d", t.Samples)
	}
	return fmt.Sprintf("%d/%d", t.Scored, t.Samples)
}

// secondsToDuration converts a recorded elapsed time back for formatting.
func secondsToDuration(seconds float64) time.Duration {
	return time.Duration(seconds * float64(time.Second))
}

// formatDuration prints a run time at a precision worth reading, down to
// microseconds so a model-free run does not report itself as taking no time.
func formatDuration(d time.Duration) string {
	switch {
	case d == 0:
		return "0s"
	case d >= 10*time.Second:
		return d.Round(100 * time.Millisecond).String()
	case d >= time.Second:
		return d.Round(10 * time.Millisecond).String()
	case d >= time.Millisecond:
		return d.Round(time.Millisecond).String()
	default:
		return d.Round(time.Microsecond).String()
	}
}

// signedScore renders a sample's score and its distance from golden, with an em
// dash where the model never answered and there is nothing to state.
func signedScore(row SampleRow) (score, diff string) {
	if !row.Scored {
		return "—", "—"
	}
	return fmt.Sprintf("%d", row.Score), fmt.Sprintf("%+d", row.Err)
}

// sampleVerdict states one sample's outcome as a sentence, for the places that
// read rather than tabulate.
func sampleVerdict(row SampleRow) string {
	if !row.Scored {
		return fmt.Sprintf("golden %d, unanswered", row.Expected)
	}
	return fmt.Sprintf("golden %d, model %d (%+d)", row.Expected, row.Score, row.Err)
}

// short trims a hash to something a person can compare by eye.
func short(h string) string {
	h = strings.TrimPrefix(h, "sha256:")
	if len(h) > 12 {
		return h[:12]
	}
	return h
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

// truncate shortens s to at most n runes, appending "…" when it does. Rune-safe
// so a multibyte title is not cut mid-character, and it returns nothing rather
// than panicking when a caller's column budget has already run out.
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
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
