// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/DanielBlei/rabbithole/internal/classify"
	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/inference"
)

var (
	backfillSince string
	backfillAll   bool
	backfillLimit int
	backfillWait  bool
)

var classifyCmd = &cobra.Command{
	Use:   "classify",
	Short: "Classify stored items into content keywords and assumptions",
}

func init() {
	backfillCmd := &cobra.Command{
		Use:   "backfill",
		Short: "Queue stored, scored items for classification, optionally draining them now",
		Args:  cobra.NoArgs,
		RunE:  runClassifyBackfill,
	}
	backfillCmd.Flags().
		StringVar(&backfillSince, "since", "720h", "only items this recent, e.g. 720h or 30d")
	backfillCmd.Flags().BoolVar(&backfillAll, "all", false, "classify the full history, ignoring --since")
	backfillCmd.Flags().IntVar(&backfillLimit, "limit", 0, "cap how many items are queued (0 means no cap)")
	backfillCmd.Flags().
		BoolVar(&backfillWait, "wait", false, "drain the queue in this process instead of leaving it for serve")
	classifyCmd.AddCommand(backfillCmd)
	rootCmd.AddCommand(classifyCmd)
}

func runClassifyBackfill(cmd *cobra.Command, _ []string) error {
	s, err := openSession(cmd, sessionOpts{})
	if err != nil {
		return err
	}
	defer s.Close()
	db, src := s.db, s.src

	cutoff := time.Time{}
	if !backfillAll {
		window, err := config.ParseDuration(backfillSince)
		if err != nil {
			return fmt.Errorf("--since: %w", err)
		}
		cutoff = time.Now().Add(-window)
	}

	queued, err := db.EnqueueBackfill(cmd.Context(), cutoff, backfillLimit, classify.Version)
	if err != nil {
		return err
	}
	fmt.Printf("Queued %d item(s) for classification at version %d.\n", queued, classify.Version)
	if !backfillWait {
		if queued > 0 {
			fmt.Println("A running server classifies them on its next pass; use --wait to drain now.")
		}
		return nil
	}

	worker := classify.NewWorker(db, src, func(
		ctx context.Context, cfg config.Config, hints []string,
	) (classify.Classifier, error) {
		return inference.ResolveClassifier(ctx, cfg.Classification, hints)
	})
	return waitBackfill(cmd.Context(), worker)
}

// waitBackfill drains the queue in this process and reports the outcome. A
// pass that could not persist its outcomes fails the command: a backfill that
// exits zero has actually been recorded. Model failures are not command
// failures — they are counted, and the items retry on a later pass.
func waitBackfill(ctx context.Context, worker *classify.Worker) error {
	stats, err := worker.RunOnce(ctx)
	switch {
	case stats.Skipped:
		fmt.Printf("Classification did not run: %s.\n", stats.Reason)
	case stats.Candidates == 0:
		fmt.Println("Nothing to classify.")
	default:
		fmt.Printf("Classified %d item(s); %d failed.\n", stats.Classified, stats.Failed)
	}
	if err != nil {
		return fmt.Errorf("classification did not complete: %w", err)
	}
	return nil
}
