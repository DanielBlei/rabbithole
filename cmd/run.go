package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/DanielBlei/ai-searcher/internal/config"
	"github.com/DanielBlei/ai-searcher/internal/digest"
	"github.com/DanielBlei/ai-searcher/internal/feeds"
	"github.com/DanielBlei/ai-searcher/internal/inference"
	"github.com/DanielBlei/ai-searcher/internal/rank"
	"github.com/DanielBlei/ai-searcher/internal/store"
)

var (
	dryRun           bool
	providerOverride string
	think            bool
)

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Fetch feeds, rank new items, and write today's digest",
	RunE:  runE,
}

func init() {
	runCmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the digest to stdout; do not write files or record items")
	runCmd.Flags().StringVar(&providerOverride, "provider", "", "override the configured provider (ollama|vllm|heuristic)")
	runCmd.Flags().BoolVar(&think, "think", false, "enable model reasoning/thinking during scoring (slower; off by default)")
}

func runE(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()

	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if providerOverride != "" {
		cfg.Provider = providerOverride
	}
	log.Debug().
		Str("config", configPath).
		Str("provider", cfg.Provider).
		Str("model", cfg.ChatModel).
		Str("chat_host", cfg.ChatHost).
		Int("batch_size", cfg.BatchSize).
		Int("min_score", cfg.MinScore).
		Int("top_n", cfg.TopN).
		Str("since", cfg.Since.String()).
		Bool("think", think).
		Msg("config loaded")

	profile, err := cfg.LoadProfile()
	if err != nil {
		return err
	}
	log.Debug().Str("path", cfg.Profile).Int("chars", len(profile)).Msg("interest profile loaded")

	db, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	log.Debug().Str("db", cfg.DBPath).Msg("store opened")

	// Fetch all feeds.
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

	// Filter to fresh, unseen items.
	fresh := filterByAge(items, cfg.Since.Std())
	log.Debug().Int("before", len(items)).Int("after", len(fresh)).
		Int("dropped_old", len(items)-len(fresh)).Msg("age filter applied")

	unseen, err := filterSeen(ctx, db, fresh)
	if err != nil {
		return err
	}
	log.Debug().Int("fresh", len(fresh)).Int("already_seen", len(fresh)-len(unseen)).
		Msg("dedup filter applied")
	log.Info().Int("new", len(unseen)).Msg("new items to score")
	if len(unseen) == 0 {
		fmt.Println("No new items to score.")
		return nil
	}

	// Resolve backend and score.
	scorer, err := inference.Resolve(ctx, inference.Config{
		Provider: cfg.Provider,
		ChatHost: cfg.ChatHost,
		Model:    cfg.ChatModel,
		APIKey:   cfg.APIKey,
		Think:    think,
	})
	if err != nil {
		return err
	}
	batches := (len(unseen) + cfg.BatchSize - 1) / cfg.BatchSize
	log.Info().Str("provider", cfg.Provider).Int("items", len(unseen)).
		Int("batches", batches).Int("batch_size", cfg.BatchSize).Bool("think", think).
		Msg("scoring items")

	scoreStart := time.Now()
	scores := rank.ScoreAll(ctx, scorer, profile, unseen, cfg.BatchSize)
	log.Info().Int("scored", len(scores)).Int("of", len(unseen)).
		Str("elapsed", time.Since(scoreStart).Round(time.Millisecond).String()).Msg("scoring complete")

	results := rank.Select(unseen, scores, cfg.MinScore, cfg.TopN)
	log.Info().Int("selected", len(results)).Int("min_score", cfg.MinScore).
		Int("top_n", cfg.TopN).Msg("items selected for digest")

	day := time.Now()
	if dryRun {
		log.Debug().Msg("dry-run: printing digest, not writing or recording")
		fmt.Println(digest.Render(day, results))
		return nil
	}

	path, err := digest.Write(cfg.OutputDir, day, results)
	if err != nil {
		return err
	}
	log.Debug().Str("path", path).Int("items", len(results)).Msg("digest written")

	if err := recordRun(ctx, db, unseen, results, day); err != nil {
		return err
	}
	log.Debug().Int("recorded", len(unseen)).Int("digested", len(results)).Msg("items recorded in store")

	fmt.Printf("Wrote %d item(s) to %s\n", len(results), path)
	return nil
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

func recordRun(ctx context.Context, db *store.Store, all []feeds.Item, results []rank.Result, day time.Time) error {
	entries := make([]store.DigestEntry, len(results))
	for i, r := range results {
		entries[i] = store.DigestEntry{Item: r.Item, Score: r.Score, Reason: r.Reason}
	}
	return db.Record(ctx, all, entries, day)
}
