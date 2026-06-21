package cmd

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/DanielBlei/ai-searcher/internal/config"
	"github.com/DanielBlei/ai-searcher/internal/digest"
	"github.com/DanielBlei/ai-searcher/internal/pipeline"
	"github.com/DanielBlei/ai-searcher/internal/store"
)

var (
	dryRun           bool
	providerOverride string
	noThink          bool
)

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Fetch feeds, rank new items, and write today's digest",
	RunE:  runE,
}

func init() {
	runCmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the digest to stdout; do not write files or record items")
	runCmd.Flags().
		StringVar(&providerOverride, "provider", "", "override the configured provider (ollama|vllm|heuristic)")
	runCmd.Flags().
		BoolVar(&noThink, "no-think", false, "disable model reasoning/thinking during scoring (use for models without thinking support)")
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
		Bool("think", !noThink).
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

	day := time.Now()
	outcome, err := pipeline.Run(ctx, cfg, profile, db, day, pipeline.Options{
		Think:  !noThink,
		Record: !dryRun,
	})
	if err != nil {
		return err
	}
	if len(outcome.Unseen) == 0 {
		fmt.Println("No new items to score.")
		return nil
	}

	if dryRun {
		log.Debug().Msg("dry-run: printing digest, not writing or recording")
		fmt.Println(digest.Render(day, outcome.Results))
		return nil
	}

	path, err := digest.Write(cfg.OutputDir, day, outcome.Results)
	if err != nil {
		return err
	}
	log.Debug().Str("path", path).Int("items", len(outcome.Results)).Msg("digest written")

	fmt.Printf("Wrote %d item(s) to %s\n", len(outcome.Results), path)
	return nil
}
