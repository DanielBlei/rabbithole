// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog"

	"github.com/DanielBlei/rabbithole/internal/cluster"
	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/inference"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// StoryClusterTimeout bounds one automatic clustering rebuild.
const StoryClusterTimeout = 2 * time.Minute

// ShouldRebuildClusters reports whether a finished OK run warrants a clustering
// rebuild. New items can change membership, and a rescore changes scores —
// which pick each cluster's representative — so both rebuild. A run that did
// neither leaves the clusters as they are, unless none exist yet: the first
// run after upgrade initializes them. A read failure errs on rebuilding.
func ShouldRebuildClusters(
	ctx context.Context, db *store.Store, newItems int, rescore bool, logger zerolog.Logger,
) bool {
	if newItems > 0 || rescore {
		return true
	}
	has, err := db.HasStoryClusters(ctx)
	if err != nil {
		logger.Warn().Err(err).Msg("story clustering: cannot read stored clusters; rebuilding")
		return true
	}
	return !has
}

// RebuildStoryClusters rebuilds persisted story metadata from recent stored
// items. Model use is optional: without embeddings or a verifier it still groups
// exact duplicates.
func RebuildStoryClusters(ctx context.Context, cfg config.Config, db *store.Store) (cluster.Stats, error) {
	logger := zerolog.Ctx(ctx)
	items, err := db.StoryItems(ctx, cluster.DefaultMaxItems)
	if err != nil {
		return cluster.Stats{}, err
	}
	if len(items) < 2 {
		if err := db.ReplaceStoryClusters(ctx, nil); err != nil {
			return cluster.Stats{}, err
		}
		return cluster.Stats{Input: len(items)}, nil
	}
	input := make([]cluster.Item, len(items))
	for i, item := range items {
		input[i] = cluster.Item{
			ID: item.ID, Source: item.Source, Title: item.Title, Link: item.Link,
			Summary: item.Summary, Published: item.Published, Score: item.Score,
		}
	}
	opts := cluster.Options{}
	if cfg.Inference.Provider != config.ProviderHeuristic {
		if cfg.Inference.EmbeddingModel != "" {
			embedder, err := inference.ResolveEmbedder(cfg.Inference)
			if err != nil {
				logger.Warn().Err(err).Msg("story clustering: embeddings unavailable")
			} else {
				opts.Embedder = embedder
			}
		}
		// The verifier answers one yes/no question inside a 64-token budget.
		// Reasoning tokens count against that budget on every provider, so a
		// thinking model can exhaust it before the JSON verdict appears and
		// every pair would read as "not the same". The decision needs no
		// reasoning: always resolve the verifier with thinking off.
		verifier, err := inference.ResolveStoryVerifier(ctx, cfg.Inference, false)
		if err != nil {
			logger.Warn().Err(err).Msg("story clustering: verifier unavailable")
		} else {
			opts.Verifier = verifier
		}
	}
	clusters, stats, err := cluster.Build(ctx, input, opts)
	if err != nil {
		return stats, fmt.Errorf("build story clusters: %w", err)
	}
	stored := make([]store.StoryCluster, len(clusters))
	for i, c := range clusters {
		items := make([]store.StoryClusterItem, len(c.Items))
		for j, item := range c.Items {
			items[j] = store.StoryClusterItem{ItemID: item.ItemID, Position: item.Position}
		}
		stored[i] = store.StoryCluster{
			ID:                   c.ID,
			RepresentativeItemID: c.RepresentativeItemID,
			Method:               c.Method,
			Confidence:           c.Confidence,
			Items:                items,
		}
	}
	if err := db.ReplaceStoryClusters(ctx, stored); err != nil {
		return stats, err
	}
	logger.Info().
		Int("items", stats.Input).
		Int("clusters", stats.Clusters).
		Int("comparisons", stats.Comparisons).
		Int("embeddings", stats.Embeddings).
		Int("verifications", stats.Verifications).
		Bool("embedding_fallback", stats.EmbeddingFallback).
		Bool("verification_fallback", stats.VerificationFallback).
		Msg("story clustering complete")
	return stats, nil
}
