package ingest

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// feedRSS renders a one-item RSS feed for a test server.
func feedRSS(title, link string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel><title>Feed</title>
  <item><title>%s</title><link>%s</link><guid>%s</guid>
  <description>summary</description></item>
</channel></rss>`, title, link, link)
}

// serveRSS starts a test server returning the given RSS body, registered for
// cleanup, and returns its URL.
func serveRSS(t *testing.T, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// testConfig builds a heuristic-provider config (no backend needed) over the
// given feeds, with a wide age window and a low threshold so test items pass.
func testConfig(t *testing.T, feeds ...config.Feed) *config.Config {
	t.Helper()
	return &config.Config{
		Inference: config.InferenceConfig{Provider: "heuristic", Model: "test-model"},
		Scoring:   config.ScoringConfig{BatchSize: 2, MaxParallel: 2},
		Ingest:    config.IngestConfig{Since: config.Duration(365 * 24 * time.Hour)},
		Store:     config.StoreConfig{DBPath: filepath.Join(t.TempDir(), "test.db")},
		Feeds:     feeds,
	}
}

func openStore(t *testing.T, cfg *config.Config) *store.Store {
	t.Helper()
	db, err := store.Open(cfg.Store.DBPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func sourceCounts(t *testing.T, db *store.Store) map[string]int {
	t.Helper()
	srcs, err := db.Sources(context.Background())
	if err != nil {
		t.Fatalf("sources: %v", err)
	}
	got := make(map[string]int, len(srcs))
	for _, s := range srcs {
		got[s.Source] = s.Count
	}
	return got
}

func TestRunProcessesFeedsPerSourceAndDedups(t *testing.T) {
	ctx := context.Background()
	// "Alpha" scores 2 (matches vllm, inference); "Beta" scores 1 (latency).
	feedA := config.Feed{Name: "Alpha", URL: serveRSS(t, feedRSS("vLLM inference notes", "https://x.test/a"))}
	feedB := config.Feed{Name: "Beta", URL: serveRSS(t, feedRSS("Latency guide", "https://x.test/b"))}
	cfg := testConfig(t, feedA, feedB)
	db := openStore(t, cfg)
	profile := "vllm inference latency batching"

	out, err := Run(ctx, cfg, profile, db, time.Now(), Options{Record: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Fetched != 2 {
		t.Errorf("Fetched = %d, want 2", out.Fetched)
	}
	if len(out.Unseen) != 2 {
		t.Errorf("Unseen = %d, want 2", len(out.Unseen))
	}
	if len(out.Results) != 2 {
		t.Fatalf("Results = %d, want 2", len(out.Results))
	}
	// Results are sorted best-first across feeds: Alpha (2) before Beta (1).
	if out.Results[0].Item.Source != "Alpha" || out.Results[1].Item.Source != "Beta" {
		t.Errorf("results not sorted best-first across feeds: %q then %q",
			out.Results[0].Item.Source, out.Results[1].Item.Source)
	}
	// Both feeds were recorded, grouped by source.
	if got := sourceCounts(t, db); got["Alpha"] != 1 || got["Beta"] != 1 {
		t.Errorf("source counts = %v, want Alpha:1 Beta:1", got)
	}

	// Second run: every item is already in the store, so the pre-scoring dedup
	// drops them all and nothing new is processed.
	out2, err := Run(ctx, cfg, profile, db, time.Now(), Options{Record: true})
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if len(out2.Unseen) != 0 {
		t.Errorf("second run Unseen = %d, want 0 (all deduped)", len(out2.Unseen))
	}
}

func TestRunSkipsFailedFeed(t *testing.T) {
	ctx := context.Background()
	good := config.Feed{Name: "Good", URL: serveRSS(t, feedRSS("vLLM inference notes", "https://x.test/g"))}
	bad := config.Feed{Name: "Bad", URL: serveRSS(t, "not xml at all")}
	cfg := testConfig(t, good, bad)
	db := openStore(t, cfg)

	out, err := Run(ctx, cfg, "vllm inference", db, time.Now(), Options{Record: true})
	if err != nil {
		t.Fatalf("Run should not fail because one feed failed: %v", err)
	}
	if len(out.Unseen) != 1 {
		t.Errorf("Unseen = %d, want 1 (only the good feed)", len(out.Unseen))
	}
	if got := sourceCounts(t, db); got["Good"] != 1 || got["Bad"] != 0 {
		t.Errorf("source counts = %v, want only Good:1", got)
	}
}
