// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

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

// The verifier answers inside a 64-token budget, so it must be resolved with
// thinking disabled: reasoning tokens count against that budget and would
// leave none for the JSON verdict.
func TestRebuildStoryClustersDisablesVerifierThinking(t *testing.T) {
	var (
		mu     sync.Mutex
		bodies []map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"message":{"content":"{\"same\":false}"}}`))
	}))
	t.Cleanup(srv.Close)

	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	now := time.Now()
	if err := db.Record(t.Context(), []feeds.Item{
		{ID: "a", Source: "S1", Title: "The same headline about a release", Link: "https://example.com/a"},
		{ID: "b", Source: "S2", Title: "The same headline about a release", Link: "https://example.com/b"},
	}, nil, now); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// Think must be explicitly enabled: a loaded config defaults it to true
	// for model-backed providers (see applyDefaults), and ThinkEnabled()
	// reads false only when the field is nil. With think on, the old code
	// resolved the verifier with reasoning enabled — the defect under test.
	think := true
	cfg := config.Config{Inference: config.InferenceConfig{
		Provider: config.ProviderOllama,
		Host:     srv.URL,
		Model:    "test-model",
		Think:    &think,
	}}
	if _, err := RebuildStoryClusters(t.Context(), cfg, db); err != nil {
		t.Fatalf("RebuildStoryClusters: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) == 0 {
		t.Fatal("story verification made no chat call")
	}
	for _, body := range bodies {
		if think, ok := body["think"].(bool); !ok || think {
			t.Errorf("chat request think = %v, want false: %v", body["think"], body)
		}
	}
}

func TestShouldRebuildClusters(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	logger := zerolog.Nop()

	// No clusters yet: the first run after upgrade must initialize them.
	if !ShouldRebuildClusters(context.Background(), db, 0, false, logger) {
		t.Error("empty store: rebuild skipped")
	}

	seed := []store.StoryCluster{{
		ID: "story-1", RepresentativeItemID: "x", Method: "exact", Confidence: 1,
		Items: []store.StoryClusterItem{{ItemID: "x", Position: 0}},
	}}
	if err := db.ReplaceStoryClusters(context.Background(), seed); err != nil {
		t.Fatalf("ReplaceStoryClusters: %v", err)
	}

	// Nothing new and no rescore: membership cannot change.
	if ShouldRebuildClusters(context.Background(), db, 0, false, logger) {
		t.Error("no-op run: rebuild not skipped")
	}
	// New items can change membership.
	if !ShouldRebuildClusters(context.Background(), db, 3, false, logger) {
		t.Error("run with new items: rebuild skipped")
	}
	// A rescore changes scores, which pick each cluster's representative.
	if !ShouldRebuildClusters(context.Background(), db, 0, true, logger) {
		t.Error("rescore run: rebuild skipped")
	}
}
