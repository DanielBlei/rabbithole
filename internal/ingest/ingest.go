// Package ingest implements the fetch -> score -> record cycle shared by the
// ingest command today and a future serve command's API/scheduler. It returns
// the cycle's Outcome as data; rendering it (markdown digest, stdout, JSON, ...)
// is the caller's concern (see the digest package).
package ingest

import (
	"context"
	"sort"
	"time"

	"github.com/rs/zerolog"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/inference"
	"github.com/DanielBlei/rabbithole/internal/rank"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// Options configures a single cycle.
type Options struct {
	Think  bool // enable model reasoning during scoring
	Record bool // write the cycle's items and results to the store
}

// Outcome is the result of one cycle. Rendering it (markdown file, stdout,
// JSON response, ...) is the caller's concern.
type Outcome struct {
	Fetched int           // items within the configured recency window (age-filtered, pre-dedup), across all feeds
	Unseen  []feeds.Item  // fresh, not-yet-seen items considered for scoring
	Results []rank.Result // every scored item, sorted best-first
	Scored  int           // items the model scored, across all feeds
	Skipped int           // items skipped as already scored
	Failed  int           // items the model failed to score
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
	logger := zerolog.Ctx(ctx)
	// Only enabled feeds are fetched; a disabled one is parked, not deleted,
	// so it stays in the config (and the viewer) without costing a request.
	active := cfg.Feeds.Enabled()
	sources := make([]feeds.Source, len(active))
	for i, f := range active {
		sources[i] = feeds.Source{Name: f.Name, URL: f.URL}
		logger.Debug().Str("feed", f.Name).Str("url", f.URL).
			Str("since", f.Since.String()).Int("max_items", f.MaxItems).Msg("configured feed")
	}
	if disabled := cfg.Feeds.Len() - len(active); disabled > 0 {
		logger.Info().Int("disabled", disabled).Msg("skipping disabled feeds")
	}
	if len(sources) == 0 {
		// Not an early return: the cycle still runs to completion (over nothing)
		// so a successful run always ends with the "ingest complete" line the
		// run manager and its log tail rely on.
		logger.Warn().Msg("no enabled feeds; nothing to ingest")
	}
	logger.Info().Int("feeds", len(sources)).Msg("ingesting feeds")

	fetchStart := time.Now()
	// FetchAll returns one Result per source, positionally — results[i] is
	// active[i]'s outcome. That pairing is used directly below rather than
	// flattening the items and regrouping them by feed name.
	results := feeds.FetchAll(ctx, sources)
	fetched, failed := tallyFetches(results)
	logger.Info().Int("items", fetched).Int("failed_feeds", failed).
		Str("elapsed", time.Since(fetchStart).Round(100*time.Millisecond).String()).Msg("fetched items")

	// Per-feed fetch outcomes are recorded whether or not the run records
	// items: a dead feed should surface even on a --dry-run. Best-effort —
	// history is observability and must never fail the run.
	if err := recordFeedFetches(ctx, db, active, results); err != nil {
		logger.Warn().Err(err).Msg("recording feed fetch history failed")
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
		s, err := inference.Resolve(ctx, cfg.Inference, opts.Think)
		if err != nil {
			return nil, err
		}
		scorer, resolved = s, true
		return scorer, nil
	}

	runStart := time.Now()
	// Counts are written straight into outcome as each feed is processed
	// (rather than into locals assigned at the end), so a feed that aborts the
	// run early (a DB error, a backend that fails to resolve) still returns
	// accurate counts for the feeds already completed, instead of resetting
	// everything to zero.
	outcome := Outcome{}
	for i, f := range active {
		feedItems := results[i].Items
		if len(feedItems) == 0 {
			continue
		}
		feedStart := time.Now()

		// Each feed filters against its own resolved window, so a firehose can
		// run a tight lookback while a rarely-updated blog keeps a wide one.
		fresh := filterByAge(feedItems, f.Since)
		logger.Debug().Str("feed", f.Name).Int("before", len(feedItems)).Int("after", len(fresh)).
			Int("dropped_old", len(feedItems)-len(fresh)).Str("since", f.Since.String()).
			Msg("age filter applied")

		// Then the per-feed cap, newest first, so one prolific feed can't eat
		// the run's whole scoring budget.
		if capped := capNewest(fresh, f.MaxItems); len(capped) < len(fresh) {
			logger.Debug().Str("feed", f.Name).Int("dropped_over_cap", len(fresh)-len(capped)).
				Int("max_items", f.MaxItems).Msg("item cap applied")
			fresh = capped
		}
		outcome.Fetched += len(fresh)

		// dedup against the store before scoring: items already scored never
		// reach the (expensive) scoring phase. A read failure here is a
		// run-wide DB problem, so it aborts rather than skipping one feed.
		unseen, err := filterUnscored(ctx, db, fresh)
		if err != nil {
			return outcome, err
		}
		skipped := len(fresh) - len(unseen)
		outcome.Skipped += skipped

		// One line per feed says what there is to do: how many new items to
		// score and how many were skipped as already scored. new=0 means there
		// was nothing to process for this feed.
		logger.Info().Str("feed", f.Name).Int("new", len(unseen)).Int("skipped", skipped).
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
		outcome.Failed += failed
		logger.Info().Str("feed", f.Name).Int("processed", len(scores)).Int("failed", failed).
			Msg("items processed")
		results := rank.Select(unseen, scores)

		if opts.Record {
			if err := record(ctx, db, unseen, scores, cfg.Inference.Model, day); err != nil {
				logger.Warn().Str("feed", f.Name).Err(err).Msg("recording feed failed, skipping")
				continue
			}
			logger.Info().Str("feed", f.Name).Int("recorded", len(unseen)).Int("scored", len(scores)).
				Int("selected", len(results)).Int("failed", failed).
				Str("elapsed", time.Since(feedStart).Round(100*time.Millisecond).String()).Msg("ingested to db")
		}

		outcome.Unseen = append(outcome.Unseen, unseen...)
		outcome.Results = append(outcome.Results, results...)
		outcome.Scored += len(scores)
	}

	// Each feed selected its own results; re-sort the merged set so the digest
	// is ordered best-first across every feed.
	rank.SortResults(outcome.Results)
	logger.Info().Int("feeds", len(active)).Int("fetched", outcome.Fetched).
		Int("failed_feeds", failed).
		Int("scored", outcome.Scored).Int("skipped", outcome.Skipped).Int("failed", outcome.Failed).
		Int("selected", len(outcome.Results)).
		Str("elapsed", time.Since(runStart).Round(100*time.Millisecond).String()).Msg("ingest complete")

	return outcome, nil
}

// tallyFetches counts the items fetched across every source and how many
// sources failed.
func tallyFetches(results []feeds.Result) (items, failed int) {
	for _, r := range results {
		items += len(r.Items)
		if r.Err != nil {
			failed++
		}
	}
	return items, failed
}

// recordFeedFetches appends each source's fetch outcome to the feed history so
// a feed that has stopped resolving is visible in the UI instead of only in
// one run's log, then trims the history to its retention bound. feeds and
// results are positionally paired (see Run).
func recordFeedFetches(
	ctx context.Context,
	db *store.Store,
	feedCfgs []config.ResolvedFeed,
	results []feeds.Result,
) error {
	fetches := make([]store.FeedFetch, 0, len(results))
	for i, r := range results {
		f := store.FeedFetch{
			FeedID:  feedCfgs[i].ID,
			Name:    r.Source.Name,
			URL:     r.Source.URL,
			Items:   len(r.Items),
			Elapsed: r.Elapsed,
			At:      r.At,
		}
		if r.Err != nil {
			f.Error = r.Err.Error()
		}
		fetches = append(fetches, f)
	}
	if err := db.RecordFeedFetches(ctx, fetches); err != nil {
		return err
	}
	// Append-only needs a ceiling; trimming here keeps it tied to the only
	// thing that grows the table.
	return db.PruneFeedFetches(ctx, 0)
}

// capNewest keeps at most maxItems items, newest first. maxItems <= 0 means
// uncapped. Items with no publish date sort last: an unknown date shouldn't
// outrank a known-recent one for a scarce slot.
func capNewest(items []feeds.Item, maxItems int) []feeds.Item {
	if maxItems <= 0 || len(items) <= maxItems {
		return items
	}
	sorted := make([]feeds.Item, len(items))
	copy(sorted, items)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i].Published, sorted[j].Published
		if a.IsZero() != b.IsZero() {
			return b.IsZero()
		}
		return a.After(b)
	})
	return sorted[:maxItems]
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
func record(
	ctx context.Context,
	db *store.Store,
	all []feeds.Item,
	scores map[string]rank.ItemScore,
	model string,
	day time.Time,
) error {
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
