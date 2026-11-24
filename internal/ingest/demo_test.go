// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/profile"
	"github.com/DanielBlei/rabbithole/internal/store"
)

func demoOptions() Options { return Options{Demo: true, Record: true} }

func TestRunDemoRecordsWelcomeItem(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	db := openStore(t, cfg)

	out, err := Run(ctx, cfg.Config, testProfile("vllm"), db, time.Now(), demoOptions())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Fetched != 1 || out.Scored != 1 || len(out.Unseen) != 1 || out.Skipped != 0 {
		t.Errorf("outcome = fetched %d scored %d unseen %d skipped %d, want 1 1 1 0",
			out.Fetched, out.Scored, len(out.Unseen), out.Skipped)
	}
	if len(out.Sites) != 0 {
		t.Errorf("icon sites = %v, want none", out.Sites)
	}

	rows, err := db.List(ctx, store.ListFilter{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("List = %d rows, err %v; want 1", len(rows), err)
	}
	row := rows[0]
	if row.Title != "Welcome to the Rabbit Hole" || row.Link != store.DemoItemLink {
		t.Errorf("item = %q %q", row.Title, row.Link)
	}
	if row.Source != "Demo · GitHub" {
		t.Errorf("source = %q", row.Source)
	}
	if row.LLMScore == nil || *row.LLMScore != 10 {
		t.Errorf("score = %v, want 10", row.LLMScore)
	}
	if row.LLMScoreModel == nil || *row.LLMScoreModel != store.DemoItemModel {
		t.Errorf("model = %v, want %q", row.LLMScoreModel, store.DemoItemModel)
	}
	if row.LLMScoreReason == nil || *row.LLMScoreReason != demoReason {
		t.Errorf("reason = %v", row.LLMScoreReason)
	}
	if len(row.Tags) != 2 || row.Tags[0] != "demo" || row.Tags[1] != "rabbithole" {
		t.Errorf("tags = %v", row.Tags)
	}
	if health, err := db.FeedHealthByID(ctx, 5); err != nil || len(health) != 0 {
		t.Errorf("feed history = %v, err %v; want none", health, err)
	}
	if demoFeed.Name != "Demo · GitHub" || demoFeed.Since <= 0 || demoFeed.MaxItems != 1 {
		t.Errorf("demo feed = %+v", demoFeed)
	}
}

func TestRunDemoIgnoresConfiguredFeeds(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(feedRSS("Real", "https://x.test/real")))
	}))
	t.Cleanup(srv.Close)
	cfg := testConfig(t, config.Feed{Name: "Real", URL: srv.URL})
	db := openStore(t, cfg)

	opts := demoOptions()
	opts.Client = loopbackClient
	if _, err := Run(context.Background(), cfg.Config, testProfile("x"), db, time.Now(), opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("configured feed hit %d times, want 0", n)
	}
	if got := sourceCounts(t, db); len(got) != 1 || got["Demo · GitHub"] != 1 {
		t.Errorf("sources = %v, want only the demo source", got)
	}
}

func TestRunDemoIdempotent(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	db := openStore(t, cfg)
	if _, err := Run(ctx, cfg.Config, testProfile("x"), db, time.Now(), demoOptions()); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	rows, err := db.List(ctx, store.ListFilter{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("List = %d rows, err %v", len(rows), err)
	}
	hidden := store.StatusSkipped
	if err := db.UpdateUserStateByID(ctx, rows[0].ID, store.UserPatch{Status: &hidden}); err != nil {
		t.Fatalf("hide: %v", err)
	}

	out, err := Run(ctx, cfg.Config, testProfile("x"), db, time.Now(), demoOptions())
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if len(out.Unseen) != 0 || out.Skipped != 1 || out.Scored != 0 {
		t.Errorf("second run unseen %d skipped %d scored %d, want 0 1 0", len(out.Unseen), out.Skipped, out.Scored)
	}
	row, err := db.Get(ctx, rows[0].ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.Status != store.StatusSkipped {
		t.Errorf("status = %q, want the hidden item to stay hidden", row.Status)
	}
}

// A demo run logs its trigger, fires no classification hook, never changes the
// saved config, and leaves the next normal run on the saved provider.
func TestStartDemoTriggerAndNoHook(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	saved := config.Config{}
	saved.Inference.Provider = config.ProviderOllama
	saved.Inference.Model = "saved-model"
	saved.Inference.Host = "http://saved:11434"
	m, err := NewManager(db, config.Static(saved), zerolog.InfoLevel)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	var hooks atomic.Int32
	m.SetOnRunComplete(func() { hooks.Add(1) })
	ctx := context.Background()

	if err := m.StartDemo(ctx); err != nil {
		t.Fatalf("StartDemo: %v", err)
	}
	waitIdle(t, m)
	m.bg.Wait()
	if n := hooks.Load(); n != 0 {
		t.Errorf("hook calls after demo = %d, want 0", n)
	}
	last, err := db.LastIngestRun(ctx)
	if err != nil || last == nil {
		t.Fatalf("LastIngestRun: %v", err)
	}
	if last.TriggeredBy != store.IngestTriggerDemo || last.Status != store.IngestStatusOK ||
		last.Counts.NewItems != 1 || last.Counts.Scored != 1 {
		t.Errorf("run = %+v, want an ok demo run with one new scored item", last)
	}
	if results, err := db.RunFeedResults(ctx, last.ID); err != nil || len(results) != 0 {
		t.Errorf("run feed results = %v, err %v; want none", results, err)
	}
	logged := false
	for _, line := range m.Status().Lines {
		if strings.Contains(line, "demo feed: Demo (built-in, no network)") {
			logged = true
		}
	}
	if !logged {
		t.Errorf("run log lacks the demo feed line: %v", m.Status().Lines)
	}

	var gotProvider string
	m.run = func(
		_ context.Context, cfg config.Config, _ profile.Snapshot, _ *store.Store, _ time.Time, o Options,
	) (Outcome, error) {
		gotProvider = cfg.Inference.Provider
		if o.Demo {
			t.Error("a normal run carried the demo option")
		}
		return Outcome{}, nil
	}
	if err := m.Start(ctx, store.IngestTriggerManual); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitIdle(t, m)
	m.bg.Wait()
	if gotProvider != config.ProviderOllama {
		t.Errorf("normal run provider = %q, want the saved one", gotProvider)
	}
	if n := hooks.Load(); n != 1 {
		t.Errorf("hook calls after normal run = %d, want 1", n)
	}
}

func TestDemoFeedMatchesStoreConstants(t *testing.T) {
	res := fetchDemo()
	if res.Err != nil || len(res.Items) != 1 {
		t.Fatalf("fetchDemo = %+v", res)
	}
	if res.Items[0].Link != store.DemoItemLink {
		t.Errorf("link = %q, want %q", res.Items[0].Link, store.DemoItemLink)
	}
	if res.Items[0].Published.IsZero() {
		t.Error("the demo item carries no publish time")
	}
}
