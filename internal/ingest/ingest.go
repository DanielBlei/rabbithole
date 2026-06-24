// Package ingest implements the fetch -> score -> record cycle shared by the
// ingest command today and a future serve command's API/scheduler. It returns
// the cycle's Outcome as data; rendering it (markdown digest, stdout, JSON, ...)
// is the caller's concern (see the digest package).
package ingest

import (
	"context"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/DanielBlei/ai-searcher/internal/config"
	"github.com/DanielBlei/ai-searcher/internal/feeds"
	"github.com/DanielBlei/ai-searcher/internal/inference"
	"github.com/DanielBlei/ai-searcher/internal/rank"
	"github.com/DanielBlei/ai-searcher/internal/store"
)

// Options configures a single cycle.
type Options struct {
	Think  bool // enable model reasoning during scoring
	Record bool // write the cycle's items and results to the store
}

// Outcome is the result of one cycle. Rendering it (markdown file, stdout,
// JSON response, ...) is the caller's concern.
type Outcome struct {
	Fetched int           // items fetched across all feeds
	Unseen  []feeds.Item  // fresh, not-yet-seen items considered for scoring
	Results []rank.Result // every scored item, sorted best-first
}

// Run fetches every feed in cfg, then processes the feeds one at a time: each
// feed's fresh items whose link isn't already scored in db are scored against
// profile and, if opts.Record is set, recorded under day in their own
// transaction (every item with its score, plus a digest date for the selected
// ones). Dedup keys on link, so a feed re-issuing a different id for the same
// article doesn't trigger a needless re-score. Processing per feed keeps each
// feed's work isolated and durable — a feed that fails to score or record is
// logged and skipped without losing the feeds already committed. The returned
// Outcome aggregates every feed, with Results sorted best-first across all.
func Run(
	ctx context.Context,
	cfg *config.Config,
	profile string,
	db *store.Store,
	day time.Time,
	opts Options,
) (Outcome, error) {
	sources := make([]feeds.Source, len(cfg.Feeds))
	for i, f := range cfg.Feeds {
		sources[i] = feeds.Source{Name: f.Name, URL: f.URL}
		log.Debug().Str("feed", f.Name).Str("url", f.URL).Msg("configured feed")
	}
	log.Info().Int("feeds", len(sources)).Msg("ingesting feeds")

	fetchStart := time.Now()
	items := feeds.FetchAll(ctx, sources)
	log.Info().Int("items", len(items)).
		Str("elapsed", time.Since(fetchStart).Round(100*time.Millisecond).String()).Msg("fetched items")

	// Group the fetched union back by feed so each feed is processed as a unit.
	// Items carry their feed name in Source (set in feeds.fetchOne).
	byFeed := make(map[string][]feeds.Item, len(sources))
	for _, it := range items {
		byFeed[it.Source] = append(byFeed[it.Source], it)
	}

	// resolveScorer builds the backend lazily on the first feed with items to
	// score, so a run with nothing new never validates (and hits) the backend.
	var (
		scorer   rank.Scorer
		resolved bool
	)
	resolveScorer := func() (rank.Scorer, error) {
		if resolved {
			return scorer, nil
		}
		s, err := inference.Resolve(ctx, inference.Config{
			Provider: cfg.Inference.Provider,
			ChatHost: cfg.Inference.Host,
			Model:    cfg.Inference.Model,
			APIKey:   cfg.Inference.APIKey,
			Think:    opts.Think,
		})
		if err != nil {
			return nil, err
		}
		scorer, resolved = s, true
		return scorer, nil
	}

	runStart := time.Now()
	outcome := Outcome{Fetched: len(items)}
	var totalScored, totalSkipped, totalFailed int
	for _, f := range cfg.Feeds {
		feedItems := byFeed[f.Name]
		if len(feedItems) == 0 {
			continue
		}
		feedStart := time.Now()

		fresh := filterByAge(feedItems, cfg.Ingest.Since.Std())
		log.Debug().Str("feed", f.Name).Int("before", len(feedItems)).Int("after", len(fresh)).
			Int("dropped_old", len(feedItems)-len(fresh)).Msg("age filter applied")

		// dedup against the store before scoring: items already scored never
		// reach the (expensive) scoring phase. A read failure here is a
		// run-wide DB problem, so it aborts rather than skipping one feed.
		unseen, err := filterUnscored(ctx, db, fresh)
		if err != nil {
			return outcome, err
		}
		skipped := len(fresh) - len(unseen)
		totalSkipped += skipped

		// One line per feed says what there is to do: how many new items to
		// score and how many were skipped as already scored. new=0 means there
		// was nothing to process for this feed.
		log.Info().Str("feed", f.Name).Int("new", len(unseen)).Int("skipped", skipped).
			Msg("processing feed")
		if len(unseen) == 0 {
			continue
		}

		s, err := resolveScorer()
		if err != nil {
			return outcome, err
		}

		scores := rank.ScoreAll(ctx, s, profile, unseen, cfg.Scoring.BatchSize, cfg.Scoring.MaxParallel)
		failed := len(unseen) - len(scores)
		totalFailed += failed
		log.Info().Str("feed", f.Name).Int("processed", len(scores)).Int("failed", failed).
			Msg("items processed")
		results := rank.Select(unseen, scores)

		if opts.Record {
			if err := record(ctx, db, unseen, scores, cfg.Inference.Model, day); err != nil {
				log.Warn().Str("feed", f.Name).Err(err).Msg("recording feed failed, skipping")
				continue
			}
			log.Info().Str("feed", f.Name).Int("recorded", len(unseen)).Int("scored", len(scores)).
				Int("selected", len(results)).Int("failed", failed).
				Str("elapsed", time.Since(feedStart).Round(100*time.Millisecond).String()).Msg("ingested to db")
		}

		outcome.Unseen = append(outcome.Unseen, unseen...)
		outcome.Results = append(outcome.Results, results...)
		totalScored += len(scores)
	}

	// Each feed selected its own results; re-sort the merged set so the digest
	// is ordered best-first across every feed.
	rank.SortResults(outcome.Results)
	log.Info().Int("feeds", len(cfg.Feeds)).Int("fetched", outcome.Fetched).
		Int("scored", totalScored).Int("skipped", totalSkipped).Int("failed", totalFailed).
		Int("selected", len(outcome.Results)).
		Str("elapsed", time.Since(runStart).Round(100*time.Millisecond).String()).Msg("ingest complete")

	return outcome, nil
}

// filterByAge keeps items published within the window. Items with no publish
// date are kept (age unknown) and deduped later by the store.
func filterByAge(items []feeds.Item, since time.Duration) []feeds.Item {
	cutoff := time.Now().Add(-since)
	out := items[:0:0]
	for _, it := range items {
		if it.Published.IsZero() || it.Published.After(cutoff) {
			out = append(out, it)
		}
	}
	return out
}

// filterUnscored drops items whose link already carries an LLM score, keeping
// the ones still worth scoring: links the store has never seen, plus links it
// recorded without a score (a prior run that failed to score them). Keying on
// link rather than the derived id means a feed re-issuing a different GUID for
// the same article no longer slips past dedup and gets needlessly re-scored.
func filterUnscored(ctx context.Context, db *store.Store, items []feeds.Item) ([]feeds.Item, error) {
	links := make([]string, len(items))
	for i, it := range items {
		links[i] = it.Link
	}
	scored, err := db.ScoredLinks(ctx, links)
	if err != nil {
		return nil, err
	}
	out := items[:0:0]
	for _, it := range items {
		if !scored[it.Link] {
			out = append(out, it)
		}
	}
	return out, nil
}

// record persists every scored item's verdict. Each scored item is stamped with
// the run day (digested_on), so the store retains when a score was produced.
func record(ctx context.Context, db *store.Store, all []feeds.Item, scores map[string]rank.ItemScore, model string, day time.Time) error {
	var entries []store.DigestEntry
	for _, it := range all {
		sc, ok := scores[it.ID]
		if !ok {
			continue
		}
		entries = append(entries, store.DigestEntry{
			Item:     it,
			Score:    sc.Score,
			Reason:   sc.Reason,
			Model:    model,
			Digested: true,
		})
	}
	return db.Record(ctx, all, entries, day)
}
