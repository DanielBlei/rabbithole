// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"time"

	"github.com/rs/zerolog"

	"github.com/DanielBlei/rabbithole/internal/rank"
)

// RunConfig is everything a benchmark run needs.
type RunConfig struct {
	Dataset *Dataset
	// Profile is the interest profile under test, passed to the scorer verbatim.
	Profile string
	// Prompt is the effective system prompt under test, empty when disabled.
	Prompt string
	Scorer rank.Scorer

	// BatchSize and MaxParallel come from the main config rather than being
	// forced to 1. A batched model sees the other articles in its context and
	// scores relative to them, so a benchmark that batched differently from
	// ingest would measure something the live pipeline never does.
	BatchSize   int
	MaxParallel int

	// Repeats scores the fixture this many times. Runs past the first exist to
	// measure the noise floor, not to improve the estimate.
	Repeats int

	// DatasetSamples is what the golden file holds before --limit narrowed it.
	// Zero means the run used the whole file.
	DatasetSamples int
	// Limit is the --limit that produced Dataset, recorded so a partial run is
	// not read as a full one.
	Limit int

	// Identity of the run, recorded so two reports can be compared.
	Provider string
	Model    string
	Think    bool
}

// RunInfo identifies what produced a report: which model, which profile, which
// golden file, and how long it took.
//
// The three hashes are the point: without them "did my profile edit help" is
// unanswerable, because there is nothing to prove the two runs differed only in
// the way you think they did.
//
// This block is provenance, not something to diff. StartedAt and ElapsedSeconds
// differ on every run by design, so comparing two reports means comparing
// results and reading config to check the two runs were comparable at all.
type RunInfo struct {
	Benchmark   string    `json:"benchmark"`
	StartedAt   time.Time `json:"run_started_at"`
	Provider    string    `json:"provider"`
	Model       string    `json:"model"`
	Think       bool      `json:"think"`
	BatchSize   int       `json:"batch_size"`
	MaxParallel int       `json:"max_parallel"`
	Repeats     int       `json:"repeats"`
	ProfileHash string    `json:"profile_hash"`
	PromptHash  string    `json:"prompt_hash"`
	DatasetHash string    `json:"dataset_hash"`

	// ElapsedSeconds is the wall clock across every repeat. Seconds rather than
	// a Duration so the JSON stays readable and comparable, as go test -json does.
	ElapsedSeconds float64 `json:"elapsed_seconds"`

	// DatasetSamples is what the golden file holds; the report's own sample
	// count is what this run scored. They differ under Limit.
	//
	// Both are recorded because DatasetHash cannot tell them apart: it
	// fingerprints the file, so a limited run and a full one over the same file
	// would otherwise look equally authoritative.
	DatasetSamples int `json:"dataset_samples"`
	Limit          int `json:"limit,omitempty"`
}

// Duration is ElapsedSeconds as a Duration, for Go callers and the renderers.
func (i RunInfo) Duration() time.Duration {
	return time.Duration(i.ElapsedSeconds * float64(time.Second))
}

// Results is the raw output of a run: one slice of outcomes per repeat.
type Results struct {
	Info RunInfo
	Runs [][]Outcome
	// RunSeconds is how long each repeat took, parallel to Runs. Recorded
	// because the decision --model exists to serve is quality against speed,
	// and a report carrying only the quality half answers half the question.
	RunSeconds []float64
}

// Run scores the fixture Repeats times and pairs every score back to its label.
//
// It never writes to the store and never mutates the dataset.
func Run(ctx context.Context, cfg RunConfig) (*Results, error) {
	if cfg.Dataset == nil || len(cfg.Dataset.Samples) == 0 {
		return nil, fmt.Errorf("benchmark has no samples")
	}
	if cfg.Scorer == nil {
		return nil, fmt.Errorf("no scorer")
	}
	repeats := max(cfg.Repeats, 1)

	// A run over the whole file still records how big the file was, so every
	// report answers "how much of the dataset is this" the same way.
	datasetSamples := cfg.DatasetSamples
	if datasetSamples == 0 {
		datasetSamples = len(cfg.Dataset.Samples)
	}

	res := &Results{
		Info: RunInfo{
			Benchmark:      cfg.Dataset.Metadata.Name,
			StartedAt:      time.Now().UTC(),
			Provider:       cfg.Provider,
			Model:          cfg.Model,
			Think:          cfg.Think,
			BatchSize:      cfg.BatchSize,
			MaxParallel:    cfg.MaxParallel,
			Repeats:        repeats,
			ProfileHash:    hash(cfg.Profile),
			PromptHash:     hash(cfg.Prompt),
			DatasetHash:    cfg.Dataset.Hash,
			DatasetSamples: datasetSamples,
			Limit:          cfg.Limit,
		},
	}

	log := zerolog.Ctx(ctx)
	items := cfg.Dataset.Items()

	// Scoring is the slow part and it happens behind one call, so a run with no
	// progress line is indistinguishable from a hung one. These are Info rather
	// than Debug because someone is watching them.
	log.Info().
		Int("samples", len(items)).
		Int("batch_size", cfg.BatchSize).
		Int("max_parallel", cfg.MaxParallel).
		Int("repeats", repeats).
		Msg("scoring fixture")

	wallClock := time.Now()
	for i := range repeats {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		started := time.Now()
		scores := rank.ScoreAll(ctx, cfg.Scorer, cfg.Profile, items, cfg.BatchSize, cfg.MaxParallel,
			rank.OnBatch(progress(log, started, i+1, repeats)))
		elapsed := time.Since(started)
		outcomes := reconcile(cfg.Dataset.Samples, scores)
		res.Runs = append(res.Runs, outcomes)
		res.RunSeconds = append(res.RunSeconds, elapsed.Seconds())

		m := Compute(outcomes)
		e := log.Info()
		if repeats > 1 {
			e = e.Str("run", fmt.Sprintf("%d/%d", i+1, repeats))
		}
		e.Int("scored", m.Scored).
			Int("unanswered", m.Missing).
			// Rounded: the log line is for reading, and the report carries the
			// full value for anything comparing two runs.
			Float64("mae", math.Round(m.MAE*100)/100).
			Str("elapsed", dur(elapsed)).
			Msg("run complete")
	}
	if repeats > 1 {
		log.Info().Int("runs", repeats).Str("elapsed", dur(time.Since(wallClock))).
			Msg("benchmark complete")
	}
	res.Info.ElapsedSeconds = time.Since(wallClock).Seconds()
	return res, nil
}

// progress builds the per-batch reporter, carrying an estimate of the time
// left. The estimate is the plain one (elapsed per batch so far, times the
// batches remaining), which is enough to answer the only question being asked
// of it: whether to keep waiting.
func progress(log *zerolog.Logger, started time.Time, run, repeats int) func(done, total, scored int) {
	return func(done, total, scored int) {
		e := log.Info().Int("batch", done).Int("of", total).Int("scored", scored)
		if repeats > 1 {
			e = e.Str("run", fmt.Sprintf("%d/%d", run, repeats))
		}
		if done < total {
			per := time.Since(started) / time.Duration(done)
			e = e.Str("eta", dur(per*time.Duration(total-done)))
		}
		e.Msg("scoring")
	}
}

// dur renders a duration for a log line, through the same formatter the report
// header uses so the two agree on what "1m14s" looks like.
func dur(d time.Duration) string { return duration(d.Seconds()) }

// reconcile pairs each sample with its score.
//
// A sample absent from the map was never answered. rank.ScoreAll drops items it
// could not score rather than returning an error, so this is the only place the
// difference between "scored 0" and "no answer" survives, and losing it here
// would let an unanswered sample masquerade as agreement with a low label.
func reconcile(samples []Sample, scores map[string]rank.ItemScore) []Outcome {
	out := make([]Outcome, 0, len(samples))
	for _, s := range samples {
		o := Outcome{Sample: s}
		if sc, ok := scores[s.ID]; ok {
			o.Score, o.Reason, o.Scored = sc.Score, sc.Reason, true
		}
		out = append(out, o)
	}
	return out
}

// hash fingerprints an input that a run depends on.
func hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}
