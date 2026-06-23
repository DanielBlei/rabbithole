// Package ingest implements the fetch -> score -> record cycle shared by the
// digest command today and a future serve command's API/scheduler. Writing the
// markdown digest is an opt-in final step (Options.Markdown), so flows that only
// need to update the store can stop before it.
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
	Think     bool   // enable model reasoning during scoring
	Record    bool   // write the cycle's items and results to the store
	Markdown  bool   // also write the dated markdown digest file (needs Record)
	OutputDir string // directory the Markdown digest is written to
}

// Outcome is the result of one cycle. Rendering it (markdown file, stdout,
// JSON response, ...) is the caller's concern.
type Outcome struct {
	Fetched    int           // items fetched across all feeds
	Unseen     []feeds.Item  // fresh, not-yet-seen items considered for scoring
	Results    []rank.Result // items that cleared min_score, sorted best-first
	DigestPath string        // path of the markdown digest written, or "" if none
}

// Run fetches every feed in cfg, scores items not already in db against
// profile, and, if opts.Record is set, records the cycle in db under day:
// every unseen item as seen, plus the selected results with their score and
// reason.
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
	log.Info().Int("feeds", len(sources)).Msg("fetching feeds")
	fetchStart := time.Now()
	items := feeds.FetchAll(ctx, sources)
	log.Info().Int("items", len(items)).
		Str("elapsed", time.Since(fetchStart).Round(time.Millisecond).String()).Msg("fetched items")

	fresh := filterByAge(items, cfg.Since.Std())
	log.Debug().Int("before", len(items)).Int("after", len(fresh)).
		Int("dropped_old", len(items)-len(fresh)).Msg("age filter applied")

	unseen, err := filterSeen(ctx, db, fresh)
	if err != nil {
		return Outcome{Fetched: len(items)}, err
	}
	log.Debug().Int("fresh", len(fresh)).Int("already_seen", len(fresh)-len(unseen)).
		Msg("dedup filter applied")
	log.Info().Int("new", len(unseen)).Msg("new items to score")
	if len(unseen) == 0 {
		return Outcome{Fetched: len(items)}, nil
	}

	scorer, err := inference.Resolve(ctx, inference.Config{
		Provider: cfg.Provider,
		ChatHost: cfg.ChatHost,
		Model:    cfg.ChatModel,
		APIKey:   cfg.APIKey,
		Think:    opts.Think,
	})
	if err != nil {
		return Outcome{Fetched: len(items), Unseen: unseen}, err
	}
	batches := (len(unseen) + cfg.BatchSize - 1) / cfg.BatchSize
	log.Info().Str("provider", cfg.Provider).Int("items", len(unseen)).
		Int("batches", batches).Int("batch_size", cfg.BatchSize).Int("max_parallel", cfg.MaxParallel).
		Bool("think", opts.Think).Msg("scoring items")

	scoreStart := time.Now()
	scores := rank.ScoreAll(ctx, scorer, profile, unseen, cfg.BatchSize, cfg.MaxParallel)
	log.Info().Int("scored", len(scores)).Int("of", len(unseen)).
		Str("elapsed", time.Since(scoreStart).Round(time.Millisecond).String()).Msg("scoring complete")

	results := rank.Select(unseen, scores, cfg.MinScore, cfg.TopN)
	log.Info().Int("selected", len(results)).Int("min_score", cfg.MinScore).
		Int("top_n", cfg.TopN).Msg("items selected for digest")

	outcome := Outcome{Fetched: len(items), Unseen: unseen, Results: results}
	if !opts.Record {
		return outcome, nil
	}
	if err := record(ctx, db, unseen, results, day); err != nil {
		return outcome, err
	}
	log.Debug().Int("recorded", len(unseen)).Int("digested", len(results)).Msg("items recorded in store")

	if opts.Markdown {
		path, err := Write(opts.OutputDir, day, results)
		if err != nil {
			return outcome, err
		}
		outcome.DigestPath = path
		log.Debug().Str("path", path).Int("items", len(results)).Msg("digest written")
	}
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

func filterSeen(ctx context.Context, db *store.Store, items []feeds.Item) ([]feeds.Item, error) {
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.ID
	}
	seen, err := db.Seen(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := items[:0:0]
	for _, it := range items {
		if !seen[it.ID] {
			out = append(out, it)
		}
	}
	return out, nil
}

func record(ctx context.Context, db *store.Store, all []feeds.Item, results []rank.Result, day time.Time) error {
	entries := make([]store.DigestEntry, len(results))
	for i, r := range results {
		entries[i] = store.DigestEntry{Item: r.Item, Score: r.Score, Reason: r.Reason}
	}
	return db.Record(ctx, all, entries, day)
}
