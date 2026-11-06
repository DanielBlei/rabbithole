// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/store"
)

func TestRebuildStoryClustersDegradesForHeuristic(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	now := time.Now()
	if err := db.Record(t.Context(), []feeds.Item{
		{ID: "a", Source: "S1", Title: "The same headline about a release", Link: "https://example.com/a"},
		{ID: "b", Source: "S1", Title: "The same headline about a release", Link: "https://example.com/b"},
		{ID: "c", Source: "S3", Title: "A different story entirely", Link: "https://example.com/c"},
	}, nil, now); err != nil {
		t.Fatalf("Record: %v", err)
	}

	stats, err := RebuildStoryClusters(t.Context(), config.Config{
		Inference: config.InferenceConfig{Provider: config.ProviderHeuristic},
	}, db)
	if err != nil {
		t.Fatalf("RebuildStoryClusters: %v", err)
	}
	if stats.Embeddings != 0 || stats.Verifications != 0 {
		t.Errorf("heuristic clustering made model calls: %+v", stats)
	}
	clusters, err := db.StoryClusters(t.Context())
	if err != nil {
		t.Fatalf("StoryClusters: %v", err)
	}
	if len(clusters) != 1 || clusters[0].Method != "exact" {
		t.Fatalf("clusters = %+v, want one exact cluster", clusters)
	}
}
