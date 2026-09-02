// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

// Package eval measures how well the configured model scores against your
// interest profile. It has two entry points with different ground truth:
// benchmark re-scores a hand-authored fixture with the current profile and
// system prompt, answering "did my edit help"; audit reads the scores already
// recorded in the store, answering "what happened". Both are read-only and
// produce a report.
package eval

import (
	"fmt"
	"time"
)

// Defaults for the flags that need one.
const (
	// DefaultLimit is how many store rows a bare `eval audit` samples. Small on
	// purpose: an audit is a look at a slice, and --all is there for the rest.
	DefaultLimit = 10
	// DefaultRepeats scores the fixture once. Raising it measures the noise
	// floor, which matters because ModelTuning carries no temperature or seed.
	DefaultRepeats = 1
)

// Format is how a report is rendered.
type Format string

const (
	// FormatText is the terminal report and the default: aligned columns, no
	// markup, the same bytes whether it lands on a TTY or in a file.
	FormatText Format = "text"
	// FormatMarkdown is the same report as a document, for pasting into an
	// issue or a pull request.
	FormatMarkdown Format = "markdown"
	// FormatJSON is the machine-readable results file. Two of these can be
	// diffed to answer whether a profile or prompt edit moved the numbers.
	FormatJSON Format = "json"
)

// ParseFormat validates a --format value.
func ParseFormat(s string) (Format, error) {
	switch Format(s) {
	case FormatText, FormatMarkdown, FormatJSON:
		return Format(s), nil
	default:
		return "", fmt.Errorf("unknown format %q, must be text, markdown or json", s)
	}
}

// Output is where a report goes and in what shape, shared by both subcommands.
type Output struct {
	Format Format
	// Path is the file to write. Empty means stdout.
	Path string
}

// RenderOptions is everything a renderer needs. Where the bytes end up is the
// caller's problem, so there is no path here.
type RenderOptions struct {
	Format Format
	// ShowWhy adds the model's stated reason and the golden note beside every
	// sample. Off by default: the sample list is a scan, not a read.
	ShowWhy bool
}

// BenchmarkOptions is a resolved `eval benchmark` invocation: score a fixture
// with the current profile and system prompt, and compare against its labels.
type BenchmarkOptions struct {
	// Path is the golden dataset to load.
	Path string
	// Repeats scores the fixture this many times. Above 1 the report gains a
	// spread, which is the only way to tell a real change from run-to-run jitter.
	Repeats int
	// Limit scores only the first Limit samples, in file order. Zero scores
	// them all. File order rather than a draw, so a limited run stays
	// comparable to the limited run before it.
	Limit int

	// Provider, Host and Model override the configured backend when non-empty,
	// so a bigger local model can be measured without editing the config.
	Provider string
	Host     string
	Model    string

	// BatchSize and MaxParallel override the configured values. Zero keeps the
	// config's, which is the default because a benchmark that batched
	// differently from ingest would measure something the live pipeline never
	// does. Overriding is for a backend ingest never uses, where matching it
	// buys nothing and a bigger batch is faster and cheaper.
	BatchSize   int
	MaxParallel int
	// Think mirrors the resolved inference.think for this run.
	Think bool

	// ShowWhy adds the model's reason and your golden note beside each sample.
	// The samples themselves are listed either way.
	ShowWhy bool

	Output Output
}

// AuditOptions is a resolved `eval audit` invocation: read what the store
// already recorded. No model is involved.
type AuditOptions struct {
	// RatedOnly keeps only rows carrying a user_score, so the report is built
	// from items you actually judged. A missing user_note is fine.
	RatedOnly bool
	// Source and ScoredBy narrow to one feed and one scoring model. ScoredBy
	// matters because llm_score_model is the only provenance the store keeps,
	// so a sample spanning models can otherwise mix incomparable rows.
	Source   string
	ScoredBy string

	// After bounds the window, resolved from --since. Zero means unbounded.
	After time.Time

	// Limit caps the sample. All takes every matching row and ignores Limit.
	Limit int
	All   bool
	// Seed makes the random draw reproducible. Zero draws fresh each run.
	Seed int64
	// Newest orders by recency instead of sampling randomly. Off by default:
	// "the most recent N" is biased toward whichever feeds ran last, which
	// quietly makes the report describe your fetch schedule instead of your feed.
	Newest bool

	Output Output
}
