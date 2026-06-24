package cmd

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/DanielBlei/ai-searcher/internal/config"
	"github.com/DanielBlei/ai-searcher/internal/digest"
	"github.com/DanielBlei/ai-searcher/internal/ingest"
	"github.com/DanielBlei/ai-searcher/internal/store"
)

var (
	dryRun           bool
	providerOverride string
	noThink          bool
	writeMarkdown    bool
)

var ingestCmd = &cobra.Command{
	Use:   "ingest",
	Short: "Fetch feeds, rank new items, update the store, and optionally write the markdown digest",
	RunE:  ingestE,
}

func init() {
	ingestCmd.Flags().
		BoolVar(&dryRun, "dry-run", false, "print the digest to stdout; do not write files or record items")
	ingestCmd.Flags().
		StringVar(&providerOverride, "provider", "", "override the configured provider (ollama|vllm|heuristic)")
	ingestCmd.Flags().
		BoolVar(&noThink, "no-think", false, "disable model reasoning/thinking during scoring (use for models without thinking support)")
	ingestCmd.Flags().
		BoolVar(&writeMarkdown, "markdown", false, "also write the dated markdown digest to the output dir (default: update the store only)")
}

func ingestE(cmd *cobra.Command, _ []string) error {
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
	outcome, err := ingest.Run(ctx, cfg, profile, db, day, ingest.Options{
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

	if writeMarkdown {
		path, err := digest.Write(cfg.OutputDir, day, outcome.Results)
		if err != nil {
			return err
		}
		log.Debug().Str("path", path).Int("items", len(outcome.Results)).Msg("digest written")
		fmt.Printf("Recorded %d item(s); wrote digest to %s\n", len(outcome.Results), path)
		return nil
	}

	fmt.Printf("Recorded %d item(s) to the store.\n", len(outcome.Results))
	return nil
}
