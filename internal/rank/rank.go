package rank

import (
	"context"
	"sort"
	"sync"

	"github.com/rs/zerolog/log"

	"github.com/DanielBlei/ai-searcher/internal/feeds"
)

const maxParallel = 4

// Result pairs an item with its relevance verdict.
type Result struct {
	Item   feeds.Item
	Score  int
	Reason string
}

// ScoreAll scores every item in batches of batchSize, running up to maxParallel
// batches concurrently. A batch that fails to score is retried item-by-item;
// items that still fail are omitted from the result.
func ScoreAll(ctx context.Context, s Scorer, profile string, items []feeds.Item, batchSize int) map[string]ItemScore {
	if batchSize < 1 {
		batchSize = 1
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

// scoreBatch scores a batch, falling back to single-item scoring on failure.
func scoreBatch(ctx context.Context, s Scorer, profile string, batch []feeds.Item) []ItemScore {
	scores, err := s.Score(ctx, profile, batch)
	if err == nil {
		return scores
	}
	if len(batch) == 1 {
		log.Warn().Str("item", batch[0].Title).Err(err).Msg("scoring failed, skipping")
		return nil
	}
	log.Warn().Int("batch", len(batch)).Err(err).Msg("batch scoring failed, retrying individually")
	var out []ItemScore
	for _, it := range batch {
		out = append(out, scoreBatch(ctx, s, profile, []feeds.Item{it})...)
	}
	return out
}

// Select keeps items scoring at least minScore, sorts by score descending
// (newest first as a tie-break), and returns at most topN results.
func Select(items []feeds.Item, scores map[string]ItemScore, minScore, topN int) []Result {
	var results []Result
	for _, it := range items {
		sc, ok := scores[it.ID]
		if !ok || sc.Score < minScore {
			continue
		}
		results = append(results, Result{Item: it, Score: sc.Score, Reason: sc.Reason})
	}
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		return results[i].Item.Published.After(results[j].Item.Published)
	})
	if topN > 0 && len(results) > topN {
		results = results[:topN]
	}
	return results
}

func chunk(items []feeds.Item, size int) [][]feeds.Item {
	var out [][]feeds.Item
	for i := 0; i < len(items); i += size {
		end := min(i+size, len(items))
		out = append(out, items[i:end])
	}
	return out
}
