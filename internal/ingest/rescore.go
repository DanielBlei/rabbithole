// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/profile"
	"github.com/DanielBlei/rabbithole/internal/rank"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// ProfileRescoreWindow is the intentionally bounded v1 rescore scope.
const ProfileRescoreWindow = 7 * 24 * time.Hour

// RescoreOutcome summarizes one explicit stored-item rescore operation.
type RescoreOutcome struct {
	Candidates int
	Scored     int
	Failed     int
}

// RescoreRecent scores already-stored, already-scored items in window against
// one immutable profile snapshot. It never fetches feeds. A backend that cannot
// initialize fails before any score is modified; per-item scoring failures
// retain their previous valid scores.
func RescoreRecent(
	ctx context.Context,
	cfg *config.Config,
	activeProfile profile.Snapshot,
	db *store.Store,
	now time.Time,
	window time.Duration,
) (RescoreOutcome, error) {
	if window <= 0 {
		return RescoreOutcome{}, fmt.Errorf("rescore window must be positive")
	}
	items, err := db.RecentScoredItems(ctx, now.Add(-window))
	if err != nil {
		return RescoreOutcome{}, err
	}
	outcome := RescoreOutcome{Candidates: len(items)}
	logger := zerolog.Ctx(ctx)
	logger.Info().
		Int("candidates", len(items)).
		Str("window", window.String()).
		Str("profile", activeProfile.Name).
		Msg("rescoring stored items")
	if len(items) == 0 {
		logger.Info().Int("candidates", 0).Msg("rescore complete")
		return outcome, nil
	}

	think := false
	if cfg.Inference.Think != nil {
		think = *cfg.Inference.Think
	}
	scorer, err := resolveConfiguredScorer(ctx, cfg, think)
	if err != nil {
		return outcome, err
	}
	return rescoreItems(ctx, cfg, activeProfile, db, items, scorer)
}

func rescoreItems(
	ctx context.Context,
	cfg *config.Config,
	activeProfile profile.Snapshot,
	db *store.Store,
	items []feeds.Item,
	scorer rank.Scorer,
) (RescoreOutcome, error) {
	logger := zerolog.Ctx(ctx)
	outcome := RescoreOutcome{Candidates: len(items)}
	scores := rank.ScoreAll(
		ctx,
		scorer,
		activeProfile.Content,
		items,
		cfg.Inference.BatchSize,
		cfg.Inference.MaxParallel,
		rank.OnBatch(func(done, total, scored int) {
			logger.Info().
				Int("batches_done", done).
				Int("batches_total", total).
				Int("scored", scored).
				Msg("rescore progress")
		}),
	)
	if err := ctx.Err(); err != nil {
		return outcome, err
	}

	replacements := make([]store.ScoreReplacement, 0, len(scores))
	for _, item := range items {
		score, ok := scores[item.ID]
		if !ok {
			continue
		}
		replacements = append(replacements, store.ScoreReplacement{
			ItemID:      item.ID,
			Score:       score.Score,
			Reason:      score.Reason,
			Model:       cfg.Inference.Model,
			ProfileID:   activeProfile.ID,
			ProfileName: activeProfile.Name,
			ProfileHash: activeProfile.Hash,
		})
	}
	updated, err := db.ReplaceItemScores(ctx, replacements)
	if err != nil {
		return outcome, err
	}
	outcome.Scored = updated
	outcome.Failed = outcome.Candidates - updated
	logger.Info().
		Int("candidates", outcome.Candidates).
		Int("rescored", outcome.Scored).
		Int("unchanged", outcome.Failed).
		Msg("rescore complete")
	return outcome, nil
}
