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
	digestOut        string
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
		BoolVar(&writeMarkdown, "markdown", false, "also write the dated markdown digest (default: update the store only)")
	ingestCmd.Flags().
		StringVar(&digestOut, "out", "", "directory for --markdown (default: ingest.digest_dir if stored, else ./digests)")
}

func ingestE(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()

	s, err := openSession(cmd, sessionOpts{})
	if err != nil {
		return err
	}
	defer s.Close()
	db := s.db

	stored, version, err := db.ActiveCanonicalConfig(ctx)
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
		path, err := digest.Write(digestDir(digestOut, run.Ingest.DigestDir), day, outcome.Results)
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

// digestDir picks where --markdown writes: --out, else the stored (deprecated)
// ingest.digest_dir, else ./digests.
func digestDir(out, stored string) string {
	switch {
	case out != "":
		return out
	case stored != "":
		return stored
	default:
		return defaultDigestDir
	}
}

const defaultDigestDir = "digests"

// applyIngestOverrides returns the config a run uses: the canonical (set-only)
// stored config with --provider applied, then prepared so the provider gets its
// own defaults and is validated. Starting from the un-defaulted form means
// nothing the old provider's defaults filled in is carried over. The stored
// config is never touched.
func applyIngestOverrides(stored config.Config, provider string) (config.Config, error) {
	run := stored.Clone()
	if provider != "" {
		run.Inference.Provider = provider
	}
	if _, err := run.Prepare(); err != nil {
		if provider != "" {
			return config.Config{}, fmt.Errorf("--provider %s: %w", provider, err)
		}
		return config.Config{}, err
	}
	return run, nil
}
