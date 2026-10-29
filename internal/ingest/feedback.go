// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"context"
	"fmt"

	"github.com/rs/zerolog"

	"github.com/DanielBlei/rabbithole/internal/feedback"
	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/profile"
	"github.com/DanielBlei/rabbithole/internal/rank"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// loadScoringContext snapshots the active profile and recent explicit ratings
// once per ingest or rescore operation. Reading feedback here rather than per
// batch keeps every item in a run scored against the same context. A read
// failure aborts like the other run-wide store reads instead of silently
// falling back to profile-only scoring; callers skip this call when there is
// nothing to score. Items in candidates are the operation's scoring targets;
// they never appear as feedback examples for their own scores.
func loadScoringContext(
	ctx context.Context,
	db *store.Store,
	activeProfile profile.Snapshot,
	candidates []feeds.Item,
) (rank.ScoringContext, error) {
	examples, err := db.RecentFeedbackExcluding(ctx, candidateLinks(candidates))
	if err != nil {
		return rank.ScoringContext{}, fmt.Errorf("load explicit feedback: %w", err)
	}
	selection := feedback.SelectExcluding(examples, candidateKeys(candidates))
	if selection.Active() {
		zerolog.Ctx(ctx).Info().
			Int("positive", len(selection.Positive)).
			Int("negative", len(selection.Negative)).
			Msg("using explicit reader feedback for scoring")
	}
	return rank.ScoringContext{Profile: activeProfile.Content, Feedback: selection}, nil
}

// candidateLinks extracts the canonical keys the feedback query can exclude.
// The SQL layer filters by link, while candidateKeys also covers item IDs for
// the in-memory selection.
func candidateLinks(items []feeds.Item) []string {
	links := make([]string, 0, len(items))
	for _, item := range items {
		if item.Link != "" {
			links = append(links, item.Link)
		}
	}
	return links
}

// candidateKeys indexes every item being scored by one operation. It includes
// both IDs and links because feedback rows identify items by ID while feeds and
// dedup identify them by link, and a reissued GUID changes only the former.
// Passing the whole candidate set, rather than just the current batch, keeps
// feedback context identical across batches and prevents an item from coaching
// its own score during ingest or rescore.
func candidateKeys(items []feeds.Item) map[string]struct{} {
	keys := make(map[string]struct{}, 2*len(items))
	for _, item := range items {
		if item.ID != "" {
			keys[item.ID] = struct{}{}
		}
		if item.Link != "" {
			keys[item.Link] = struct{}{}
		}
	}
	return keys
}
