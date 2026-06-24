package rank

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/DanielBlei/ai-searcher/internal/feeds"
	"github.com/DanielBlei/ai-searcher/internal/retry"
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

// ScoreAll scores every item in batches of batchSize, running up to maxParallel
// batches concurrently. A batch that fails to score is retried item-by-item;
// items that still fail are omitted from the result.
func ScoreAll(
	ctx context.Context,
	s Scorer,
	profile string,
	items []feeds.Item,
	batchSize, maxParallel int,
) map[string]ItemScore {
	if batchSize < 1 {
		batchSize = 1
	}
	if maxParallel < 1 {
		maxParallel = 1
	}
	batches := chunk(items, batchSize)

	sem := make(chan struct{}, maxParallel)
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		out = make(map[string]ItemScore, len(items))
	)
	for i, batch := range batches {
		wg.Add(1)
		go func(idx int, batch []feeds.Item) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			log.Debug().Int("batch", idx+1).Int("of", len(batches)).
				Int("size", len(batch)).Msg("scoring batch")

			scores := scoreBatch(ctx, s, profile, batch)

			titles := make(map[string]string, len(batch))
			for _, it := range batch {
				titles[it.ID] = it.Title
			}
			mu.Lock()
			for _, sc := range scores {
				out[sc.ID] = sc
				log.Debug().Int("score", sc.Score).Str("reason", sc.Reason).
					Str("title", truncate(titles[sc.ID], 80)).Msg("scored item")
			}
			mu.Unlock()

			log.Debug().Int("batch", idx+1).Int("scored", len(scores)).
				Int("size", len(batch)).Msg("batch complete")
		}(i, batch)
	}
	wg.Wait()
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// scoreBatch scores a multi-item batch, decomposing to per-item scoring on
// failure or on a response that's missing entries for some of the batch's
// items — small models in particular sometimes return a verdict for only part
// of a batch without erroring. Decomposition isolates the problem item; the
// actual retrying happens once, at the leaf, in scoreOne.
func scoreBatch(ctx context.Context, s Scorer, profile string, batch []feeds.Item) []ItemScore {
	if len(batch) == 1 {
		return scoreOne(ctx, s, profile, batch[0])
	}
	scores, err := s.Score(ctx, profile, batch)
	if err == nil {
		missing := missingItems(batch, scores)
		if len(missing) == 0 {
			return scores
		}
		log.Warn().Int("items", len(batch)).Int("missing", len(missing)).
			Msg("batch response missing scores for some items, retrying individually")
		return append(scores, retryEach(ctx, s, profile, missing)...)
	}
	log.Warn().Int("items", len(batch)).Err(err).Msg("batch " + failureVerb(err) + ", retrying individually")
	return retryEach(ctx, s, profile, batch)
}

// scoreOne is the single retry point for scoring. It re-asks the model for one
// item with exponential backoff before giving up, since scoring is
// non-deterministic and a transient failure (a timeout, a momentary connection
// blip, or a one-shot unparseable response) usually clears on a re-ask.
// Without this, a single hiccup drops the article from the digest entirely.
func scoreOne(ctx context.Context, s Scorer, profile string, item feeds.Item) []ItemScore {
	var scores []ItemScore
	err := retry.Do(ctx, retryAttempts, retryBackoff, func() error {
		got, err := s.Score(ctx, profile, []feeds.Item{item})
		if err != nil {
			return err
		}
		if len(got) == 0 {
			return fmt.Errorf("no score returned for item")
		}
		scores = got
		return nil
	}, func(attempt int, err error, delay time.Duration) {
		log.Warn().Str("item", truncate(item.Title, 80)).Int("attempt", attempt).
			Str("retry_in", delay.String()).Err(err).Msg(failureVerb(err) + ", retrying item")
	})
	if err != nil {
		log.Warn().Str("item", truncate(item.Title, 80)).Int("attempts", retryAttempts).
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

func retryEach(ctx context.Context, s Scorer, profile string, items []feeds.Item) []ItemScore {
	var out []ItemScore
	for _, it := range items {
		out = append(out, scoreBatch(ctx, s, profile, []feeds.Item{it})...)
	}
	return out
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

func chunk(items []feeds.Item, size int) [][]feeds.Item {
	var out [][]feeds.Item
	for i := 0; i < len(items); i += size {
		end := min(i+size, len(items))
		out = append(out, items[i:end])
	}
	return out
}
