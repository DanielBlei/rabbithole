// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/digest"
	"github.com/DanielBlei/rabbithole/internal/ingest"
	"github.com/DanielBlei/rabbithole/internal/profilemgr"
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
		StringVar(&providerOverride, "provider", "", "override the configured provider (ollama|openai|vllm|heuristic)")
	ingestCmd.Flags().
		BoolVar(&noThink, "no-think", false, "override config to disable model reasoning/thinking for this run (use for models without thinking support)")
	ingestCmd.Flags().
		BoolVar(&writeMarkdown, "markdown", false, "also write the dated markdown digest to the output dir (default: update the store only)")
}

func ingestE(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()

	s, err := openSession(cmd, sessionOpts{})
	if err != nil {
		return err
	}
	defer s.Close()
	db := s.db

	stored, version, err := s.src.Active(ctx)
	if err != nil {
		return err
	}
	run, err := applyIngestOverrides(stored, providerOverride)
	if err != nil {
		return err
	}
	// think comes from the stored config; --no-think overrides it for one-off runs
	// without editing it (precedence: flag if passed -> config -> default true).
	think := run.Inference.ThinkEnabled()
	if cmd.Flags().Changed("no-think") {
		think = false // --no-think always disables reasoning
	}
	log.Debug().
		Int("version", version).
		Str("provider", run.Inference.Provider).
		Str("model", run.Inference.Model).
		Str("host", run.Inference.Host).
		Int("batch_size", run.Inference.BatchSize).
		Str("since", run.Ingest.Since.String()).
		Bool("think", think).
		Msg("config loaded")

	activeProfile, err := profilemgr.New(db).Resolve(ctx)
	if err != nil {
		return err
	}
	log.Debug().
		Str("profile_id", activeProfile.ID).
		Str("profile_name", activeProfile.Name).
		Int("chars", len(activeProfile.Content)).
		Msg("interest profile resolved")

	day := time.Now()
	outcome, err := ingest.Run(ctx, run, activeProfile, db, day, ingest.Options{
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
		if run.Ingest.DigestDir == "" {
			return fmt.Errorf("--markdown set but ingest.digest_dir is not set")
		}
		path, err := digest.Write(run.Ingest.DigestDir, day, outcome.Results)
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

// applyIngestOverrides returns a copy of the stored config with --provider
// applied for this run. The copy is prepared again so the new provider gets its
// own defaults and is validated; the stored config is never touched.
func applyIngestOverrides(stored config.Config, provider string) (config.Config, error) {
	run := stored.Clone()
	if provider == "" {
		return run, nil
	}
	run.Inference.Provider = provider
	if _, err := run.Prepare(); err != nil {
		return config.Config{}, fmt.Errorf("--provider %s: %w", provider, err)
	}
	return run, nil
}
