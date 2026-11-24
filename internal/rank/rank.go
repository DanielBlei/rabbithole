// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package rank

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/DanielBlei/rabbithole/internal/feedback"
	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/retry"
)

// retryAttempts/retryBackoff bound how many times a single item is re-scored
// before it's dropped. Scoring is non-deterministic, so a transient failure (a
// timeout, a momentary connection blip, or a one-shot unparseable response)
// usually clears on a re-ask; without this a single hiccup drops the article
// from the digest entirely. Kept short since it runs inline per item: 3 tries
// with 1s, then 2s, between them.
var (
	retryAttempts = 3
	retryBackoff  = 1 * time.Second
)

// Result pairs an item with its relevance verdict.
type Result struct {
	Item   feeds.Item
	Score  int
	Reason string
}

// Option adjusts a ScoreAll run. Variadic so the common caller stays a plain
// call and only a caller that wants progress pays for it.
type Option func(*options)

type options struct {
	onBatch         func(done, total, scored int)
	onBatchProgress func(BatchProgress)
	feedback        feedback.Selection
}

// BatchProgress is one batch completion plus the state of the shared scoring
// pool at that moment. Group is the index of the group the batch came from, so
// a caller that keeps a group per feed can attribute progress without the
// pool knowing anything about feeds.
type BatchProgress struct {
	Batch      int // 0-based index of the completed batch across every group
	Group      int // 0-based index of the group the batch came from
	Done       int // batches completed so far, 1..Total
	Total      int // total batches across every group
	GroupDone  int // batches completed so far in this batch's group
	GroupTotal int // total batches in this batch's group
	Scored     int // items scored so far across the whole pool
	// InFlight is how many scoring requests were still running after this
	// one finished; Peak is the largest InFlight observed since scoring began.
	InFlight int
	Peak     int
}

// OnBatch registers a callback fired as each batch finishes, receiving the
// number of batches done so far, the total, and how many items are scored in
// all. It exists for the callers a human waits on: scoring is the slow part,
// and without it a long run is indistinguishable from a hung one.
//
// Batches complete out of order, so done counts completions rather than
// identifying which batch. Calls are serialised under the lock that merges
// results, so an implementation needs no locking of its own but must not block.
func OnBatch(fn func(done, total, scored int)) Option {
	return func(o *options) { o.onBatch = fn }
}

// OnBatchProgress registers a richer callback fired as each batch finishes. It
// reports the same completion counts as OnBatch plus which group the batch came
// from and the pool's current and peak in-flight scoring requests. Like
// OnBatch, calls are serialised under the lock that merges results, so an
// implementation needs no locking of its own but must not block.
func OnBatchProgress(fn func(BatchProgress)) Option {
	return func(o *options) { o.onBatchProgress = fn }
}

// WithFeedback adds explicit reader ratings to the scoring context for this
// run. The selection is bounded and ordered by the feedback package; an inactive
// selection leaves the prompt exactly as it was before adaptive scoring.
func WithFeedback(selection feedback.Selection) Option {
	return func(o *options) { o.feedback = selection }
}

// ScoreAll scores every item in batches of batchSize, running up to maxParallel
// batches concurrently. A batch that fails to score is retried item-by-item;
// items that still fail are omitted from the result.
func ScoreAll(
	ctx context.Context,
	s Scorer,
	profile string,
	items []feeds.Item,
	batchSize, maxParallel int,
	opts ...Option,
) map[string]ItemScore {
	o := applyOptions(opts)
	if batchSize < 1 {
		batchSize = 1
	}
	return scoreGroups(ctx, s, profile, [][]feeds.Item{items}, batchSize, maxParallel, o)
}

// ScoreGroups scores caller-grouped items through one shared concurrency pool.
// Each group is chunked separately, so a batch never contains items from two
// groups, then every batch from every group runs through the same maxParallel
// semaphore, wait group, verdict guard and scoring context. This lets a caller
// keep per-feed batches while treating maxParallel as a run-wide ceiling
// instead of a per-call one. Batches that fail to score are retried
// item-by-item; items that still fail are omitted from the result.
func ScoreGroups(
	ctx context.Context,
	s Scorer,
	profile string,
	groups [][]feeds.Item,
	batchSize, maxParallel int,
	opts ...Option,
) map[string]ItemScore {
	o := applyOptions(opts)
	if batchSize < 1 {
		batchSize = 1
	}
	return scoreGroups(ctx, s, profile, groups, batchSize, maxParallel, o)
}

func applyOptions(opts []Option) options {
	var o options
	for _, apply := range opts {
		apply(&o)
	}
	return o
}

// scoreJob is one batch plus the group it belongs to.
type scoreJob struct {
	group int
	items []feeds.Item
}

// scoreGroups is the shared implementation behind ScoreAll and ScoreGroups.
func scoreGroups(
	ctx context.Context,
	s Scorer,
	profile string,
	groups [][]feeds.Item,
	batchSize, maxParallel int,
	o options,
) map[string]ItemScore {
	if maxParallel < 1 {
		maxParallel = 1
	}
	scoringContext := ScoringContext{Profile: profile, Feedback: o.feedback}
	// Only a ContextScorer receives the feedback selection, so only its scores
	// may claim feedback provenance. Check before the verdictGuard wrap, which
	// implements ContextScorer for every backend.
	_, contextual := s.(ContextScorer)
	var provenance FeedbackProvenance
	if contextual {
		provenance = scoringContext.Provenance()
	}
	var (
		jobs       []scoreJob
		groupTotal = make([]int, len(groups))
		itemCount  int
	)
	for gi, items := range groups {
		batches := Chunk(items, batchSize)
		groupTotal[gi] = len(batches)
		itemCount += len(items)
		for _, batch := range batches {
			jobs = append(jobs, scoreJob{group: gi, items: batch})
		}
	}
	logger := zerolog.Ctx(ctx)
	guard := &verdictGuard{Scorer: s}
	s = guard

	sem := make(chan struct{}, maxParallel)
	var (
		mu        sync.Mutex
		wg        sync.WaitGroup
		done      int
		groupDone = make([]int, len(groups))
		out       = make(map[string]ItemScore, itemCount)
		inFlight  atomic.Int64
		peak      atomic.Int64
	)
	for i, job := range jobs {
		wg.Add(1)
		go func(idx int, job scoreJob) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			current := inFlight.Add(1)
			recordPeak(&peak, current)

			logger.Debug().Int("batch", idx+1).Int("of", len(jobs)).Int("group", job.group).
				Int("size", len(job.items)).Msg("scoring batch")

			scores := scoreBatch(ctx, s, scoringContext, job.items)
			inFlight.Add(-1)

			titles := make(map[string]string, len(job.items))
			for _, it := range job.items {
				titles[it.ID] = it.Title
			}
			mu.Lock()
			for _, sc := range scores {
				if contextual {
					sc.Feedback = provenance
				}
				out[sc.ID] = sc
				logger.Debug().Int("score", sc.Score).Str("reason", sc.Reason).
					Str("title", truncate(titles[sc.ID], 80)).Msg("scored item")
			}
			done++
			groupDone[job.group]++
			if o.onBatch != nil {
				o.onBatch(done, len(jobs), len(out))
			}
			if o.onBatchProgress != nil {
				o.onBatchProgress(BatchProgress{
					Batch:      idx,
					Group:      job.group,
					Done:       done,
					Total:      len(jobs),
					GroupDone:  groupDone[job.group],
					GroupTotal: groupTotal[job.group],
					Scored:     len(out),
					InFlight:   int(inFlight.Load()),
					Peak:       int(peak.Load()),
				})
			}
			mu.Unlock()

			logger.Debug().Int("batch", idx+1).Int("scored", len(scores)).
				Int("size", len(job.items)).Msg("batch complete")
		}(i, job)
	}
	wg.Wait()
	if n := guard.rejected.Load(); n > 0 {
		logger.Warn().Int64("unusable_verdicts", n).Int("items", itemCount).Int("scored", len(out)).
			Msg("dropped unusable verdicts during scoring; the endpoint returned well-formed replies that make no sense")
	}
	return out
}

// recordPeak raises peak to current when current is higher.
func recordPeak(peak *atomic.Int64, current int64) {
	for {
		prev := peak.Load()
		if current <= prev || peak.CompareAndSwap(prev, current) {
			return
		}
	}
}

// truncate shortens s to at most n runes, appending "…" when it does. Counting
// runes rather than bytes keeps a multibyte title from being cut mid-character,
// which would put invalid UTF-8 into the log line.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// scoreBatch scores a multi-item batch, decomposing to per-item scoring on
// failure or on a response that's missing entries for some of the batch's
// items — small models in particular sometimes return a verdict for only part
// of a batch without erroring. Decomposition isolates the problem item; the
// actual retrying happens once, at the leaf, in scoreOne.
func scoreBatch(ctx context.Context, s Scorer, scoringContext ScoringContext, batch []feeds.Item) []ItemScore {
	if len(batch) == 1 {
		return scoreOne(ctx, s, scoringContext, batch[0])
	}
	logger := zerolog.Ctx(ctx)
	scores, err := score(ctx, s, scoringContext, batch)
	if err == nil {
		missing := missingItems(batch, scores)
		if len(missing) == 0 {
			return scores
		}
		logger.Warn().Int("items", len(batch)).Int("missing", len(missing)).
			Msg("batch response missing scores for some items, retrying individually")
		return append(scores, retryEach(ctx, s, scoringContext, missing)...)
	}
	logger.Warn().Int("items", len(batch)).Err(err).Msg("batch " + failureVerb(err) + ", retrying individually")
	return retryEach(ctx, s, scoringContext, batch)
}

// scoreOne is the single retry point for scoring. It re-asks the model for one
// item with exponential backoff before giving up, since scoring is
// non-deterministic and a transient failure (a timeout, a momentary connection
// blip, or a one-shot unparseable response) usually clears on a re-ask.
// Without this, a single hiccup drops the article from the digest entirely.
func scoreOne(ctx context.Context, s Scorer, scoringContext ScoringContext, item feeds.Item) []ItemScore {
	logger := zerolog.Ctx(ctx)
	var scores []ItemScore
	err := retry.Do(ctx, retryAttempts, retryBackoff, func() error {
		got, err := score(ctx, s, scoringContext, []feeds.Item{item})
		if err != nil {
			return err
		}
		if len(got) == 0 {
			return fmt.Errorf("no score returned for item")
		}
		scores = got
		return nil
	}, func(attempt int, err error, delay time.Duration) {
		logger.Warn().Str("item", truncate(item.Title, 80)).Int("attempt", attempt).
			Str("retry_in", delay.String()).Err(err).Msg(failureVerb(err) + ", retrying item")
	})
	if err != nil {
		logger.Warn().Str("item", truncate(item.Title, 80)).Int("attempts", retryAttempts).
			Err(err).Msg(failureVerb(err) + ", skipping after retries")
		return nil
	}
	return scores
}

// failureVerb labels why scoring a batch/item didn't produce a usable result,
// so logs distinguish a slow/unresponsive backend from one that responded but
// produced unusable output (e.g. a malformed or unmappable verdict).
func failureVerb(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "scoring timed out"
	}
	return "scoring failed"
}

func retryEach(ctx context.Context, s Scorer, scoringContext ScoringContext, items []feeds.Item) []ItemScore {
	var out []ItemScore
	for _, it := range items {
		out = append(out, scoreBatch(ctx, s, scoringContext, []feeds.Item{it})...)
	}
	return out
}

// score sends the richest context the backend understands. Prompt-based
// backends opt in through ContextScorer; others keep receiving the profile.
func score(ctx context.Context, s Scorer, scoringContext ScoringContext, items []feeds.Item) ([]ItemScore, error) {
	if contextual, ok := s.(ContextScorer); ok {
		return contextual.ScoreContext(ctx, scoringContext, items)
	}
	return s.Score(ctx, scoringContext.Profile, items)
}

// missingItems returns the items in batch that scores has no entry for.
func missingItems(batch []feeds.Item, scores []ItemScore) []feeds.Item {
	got := make(map[string]bool, len(scores))
	for _, sc := range scores {
		got[sc.ID] = true
	}
	var missing []feeds.Item
	for _, it := range batch {
		if !got[it.ID] {
			missing = append(missing, it)
		}
	}
	return missing
}

// Select turns every scored item into a Result, sorted by score descending
// (newest first as a tie-break). Items that failed to score are dropped.
func Select(items []feeds.Item, scores map[string]ItemScore) []Result {
	var results []Result
	for _, it := range items {
		sc, ok := scores[it.ID]
		if !ok {
			continue
		}
		results = append(results, Result{Item: it, Score: sc.Score, Reason: sc.Reason})
	}
	SortResults(results)
	return results
}

// SortResults orders results best-first: by score descending, newest published
// first as a tie-break. Select sorts each feed's results with it; an ingest that
// processes feeds one at a time reuses it to re-order the merged set globally.
func SortResults(results []Result) {
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		return results[i].Item.Published.After(results[j].Item.Published)
	})
}

// Chunk splits items into batches of at most size, preserving order. A size
// below 1 is treated as 1, the same normalisation ScoreAll and ScoreGroups use,
// so a caller can derive a batch total that matches what scoring will run.
func Chunk(items []feeds.Item, size int) [][]feeds.Item {
	if size < 1 {
		size = 1
	}
	var out [][]feeds.Item
	for i := 0; i < len(items); i += size {
		end := min(i+size, len(items))
		out = append(out, items[i:end])
	}
	return out
}
