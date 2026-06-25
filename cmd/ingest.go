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
		BoolVar(&noThink, "no-think", false, "override config to disable model reasoning/thinking for this run (use for models without thinking support)")
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
		cfg.Inference.Provider = providerOverride
	}
	// think comes from config; --no-think overrides it for one-off runs without
	// editing the config (precedence: flag if passed → config → default true).
	think := *cfg.Inference.Think
	if cmd.Flags().Changed("no-think") {
		think = false // --no-think always disables reasoning
	}
	log.Debug().
		Str("config", configPath).
		Str("provider", cfg.Inference.Provider).
		Str("model", cfg.Inference.Model).
		Str("host", cfg.Inference.Host).
		Int("batch_size", cfg.Scoring.BatchSize).
		Str("since", cfg.Ingest.Since.String()).
		Bool("think", think).
		Msg("config loaded")

	profile, err := cfg.LoadProfile()
	if err != nil {
		return err
	}
	log.Debug().Str("path", cfg.Profile).Int("chars", len(profile)).Msg("interest profile loaded")

	db, err := store.Open(cfg.Store.DBPath)
	if err != nil {
		return err
	}
	defer func() {
		if err := db.Close(); err != nil {
			log.Warn().Err(err).Msg("db close failed")
		}
	}()
	log.Debug().Str("db", cfg.Store.DBPath).Msg("store opened")

	day := time.Now()
	outcome, err := ingest.Run(ctx, cfg, profile, db, day, ingest.Options{
		Think:  think,
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
		if cfg.Ingest.DigestDir == "" {
			return fmt.Errorf("--markdown set but ingest.digest_dir is empty; set it in the config")
		}
		path, err := digest.Write(cfg.Ingest.DigestDir, day, outcome.Results)
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
