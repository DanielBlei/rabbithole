// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package classify

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/store"
)

type fakeClassifier struct {
	mu      sync.Mutex
	calls   int
	failFor map[string]error
	verdict map[string]ItemVerdict
}

func (f *fakeClassifier) Validate(context.Context) error { return nil }

func (f *fakeClassifier) Classify(_ context.Context, items []feeds.Item) ([]ItemVerdict, error) {
	f.mu.Lock()
	f.calls += len(items)
	f.mu.Unlock()
	out := make([]ItemVerdict, 0, len(items))
	for i, item := range items {
		if err, ok := f.failFor[item.ID]; ok {
			return nil, err
		}
		verdict, ok := f.verdict[item.ID]
		if !ok {
			verdict = ItemVerdict{}
		}
		verdict.Index = i + 1
		verdict.ID = item.ID
		out = append(out, verdict)
	}
	return out, nil
}

func (f *fakeClassifier) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func preparedConfig(t *testing.T, mutate func(*config.Config)) config.Config {
	t.Helper()
	cfg := config.Config{
		Inference: config.InferenceConfig{Provider: config.ProviderOllama},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	if _, err := cfg.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return cfg
}

func openWorkerStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "classify.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func seedItem(t *testing.T, db *store.Store, id string, age time.Duration) {
	t.Helper()
	item := feeds.Item{
		ID: id, Source: "S", Title: "Title " + id, Link: "https://x/" + id,
		Summary: "summary", Published: time.Now().UTC().Add(-age),
	}
	if err := db.Record(context.Background(), []feeds.Item{item}, []store.DigestEntry{{
		Item: item, Score: 8, Reason: "relevant",
	}}, time.Now()); err != nil {
		t.Fatalf("Record: %v", err)
	}
}

func newTestWorker(t *testing.T, db *store.Store, cfg config.Config, fake Classifier) *Worker {
	t.Helper()
	w := NewWorker(db, config.Static(cfg), func(
		context.Context, config.Config, []string,
	) (Classifier, error) {
		if fake == nil {
			return nil, errors.New("no classifier")
		}
		return fake, nil
	})
	w.retryBackoff = time.Millisecond
	return w
}

func TestWorkerClassifiesAndStores(t *testing.T) {
	db := openWorkerStore(t)
	seedItem(t, db, "a", time.Hour)
	fake := &fakeClassifier{verdict: map[string]ItemVerdict{
		"a": {Keywords: []string{"vLLM", "paged attention"}, Assumptions: []string{"inference"}},
	}}
	w := newTestWorker(t, db, preparedConfig(t, nil), fake)

	stats, err := w.RunOnce(t.Context())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Classified != 1 || stats.Failed != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	tags, err := db.TagsForItem(t.Context(), "a")
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 3 {
		t.Fatalf("tags = %+v", tags)
	}
	state, ok, err := db.ClassificationState(t.Context(), "a")
	if err != nil || !ok || state.Status != store.ClassificationOK {
		t.Fatalf("state = %+v, %v, %v", state, ok, err)
	}
}

func TestWorkerFailureIsolation(t *testing.T) {
	db := openWorkerStore(t)
	seedItem(t, db, "good", time.Hour)
	seedItem(t, db, "bad", 2*time.Hour)
	fake := &fakeClassifier{failFor: map[string]error{
		"bad": errors.New("model rejected this item"),
	}, verdict: map[string]ItemVerdict{
		"good": {Keywords: []string{"go"}},
	}}
	w := newTestWorker(t, db, preparedConfig(t, nil), fake)
	w.retryBackoff = time.Millisecond

	stats, err := w.RunOnce(t.Context())
	if err != nil {
		t.Fatalf("a model failure must not fail the pass: %v", err)
	}
	if stats.Classified != 1 || stats.Failed != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	good, ok, err := db.ClassificationState(t.Context(), "good")
	if err != nil || !ok || good.Status != store.ClassificationOK {
		t.Fatalf("good state = %+v, %v, %v", good, ok, err)
	}
	bad, ok, err := db.ClassificationState(t.Context(), "bad")
	if err != nil || !ok {
		t.Fatalf("bad state = %+v, %v, %v", bad, ok, err)
	}
	if bad.Status != store.ClassificationFailed || bad.Attempts != 1 || bad.LastError == "" {
		t.Fatalf("bad state = %+v", bad)
	}
	if bad.NextAttemptAt == nil || !bad.NextAttemptAt.After(time.Now()) {
		t.Errorf("next attempt = %v, want a future backoff", bad.NextAttemptAt)
	}
}

func TestWorkerRetriesOneItem(t *testing.T) {
	db := openWorkerStore(t)
	seedItem(t, db, "a", time.Hour)
	fake := &fakeClassifier{failFor: map[string]error{"a": errors.New("down")}}
	w := newTestWorker(t, db, preparedConfig(t, nil), fake)
	w.retryBackoff = time.Millisecond

	stats, err := w.RunOnce(t.Context())
	if err != nil {
		t.Fatalf("a model failure must not fail the pass: %v", err)
	}
	if stats.Failed != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	if got := fake.callCount(); got != defaultRetryAttempts {
		t.Errorf("calls = %d, want %d retry attempts", got, defaultRetryAttempts)
	}
}

func TestWorkerSkipsHeuristic(t *testing.T) {
	db := openWorkerStore(t)
	seedItem(t, db, "a", time.Hour)
	cfg := preparedConfig(t, func(c *config.Config) {
		c.Classification.Provider = config.ProviderHeuristic
	})
	w := newTestWorker(t, db, cfg, &fakeClassifier{})
	stats, _ := w.RunOnce(t.Context())
	if !stats.Skipped || stats.Reason == "" {
		t.Fatalf("stats = %+v, want a skipped pass", stats)
	}
	if _, ok, err := db.ClassificationState(t.Context(), "a"); err != nil || ok {
		t.Fatalf("heuristic recorded state: ok=%v err=%v", ok, err)
	}
}

func TestWorkerHonoursDisabled(t *testing.T) {
	db := openWorkerStore(t)
	seedItem(t, db, "a", time.Hour)
	cfg := preparedConfig(t, func(c *config.Config) {
		off := false
		c.Classification.Enabled = &off
	})
	w := newTestWorker(t, db, cfg, &fakeClassifier{})
	stats, _ := w.RunOnce(t.Context())
	if !stats.Skipped || stats.Reason != "classification disabled" {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestWorkerBatchesMultipleItems(t *testing.T) {
	db := openWorkerStore(t)
	seedItem(t, db, "a", time.Hour)
	seedItem(t, db, "b", 2*time.Hour)
	fake := &fakeClassifier{verdict: map[string]ItemVerdict{
		"a": {Keywords: []string{"go"}},
		"b": {Keywords: []string{"rust"}},
	}}
	cfg := preparedConfig(t, func(c *config.Config) {
		c.Classification.BatchSize = 2
	})
	w := newTestWorker(t, db, cfg, fake)

	stats, err := w.RunOnce(t.Context())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Classified != 2 || stats.Failed != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	if fake.callCount() != 2 {
		t.Errorf("calls = %d, want one batched request for two items", fake.callCount())
	}
}

func TestWorkerStartNotifyShutdown(t *testing.T) {
	db := openWorkerStore(t)
	seedItem(t, db, "a", time.Hour)
	fake := &fakeClassifier{verdict: map[string]ItemVerdict{
		"a": {Keywords: []string{"go"}},
	}}
	cfg := preparedConfig(t, nil)
	w := newTestWorker(t, db, cfg, fake)
	w.interval = time.Hour // only the explicit notify can drive this test
	w.Start()
	w.Notify()

	deadline := time.Now().Add(5 * time.Second)
	for {
		state, ok, err := db.ClassificationState(t.Context(), "a")
		if err != nil {
			t.Fatal(err)
		}
		if ok && state.Status == store.ClassificationOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("item was not classified by the background pass: state=%+v ok=%v", state, ok)
		}
		time.Sleep(10 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	w.Shutdown(ctx)
}

func TestWorkerIdleDoesNotResolveBackend(t *testing.T) {
	db := openWorkerStore(t)
	resolved := 0
	w := NewWorker(db, config.Static(preparedConfig(t, nil)), func(
		context.Context, config.Config, []string,
	) (Classifier, error) {
		resolved++
		return nil, errors.New("must not be called on an idle queue")
	})
	stats, _ := w.RunOnce(t.Context())
	if stats.Candidates != 0 || stats.Skipped {
		t.Fatalf("stats = %+v", stats)
	}
	if resolved != 0 {
		t.Errorf("backend resolved %d times on an idle queue", resolved)
	}
}

func TestWorkerLeavesItemsPendingWhenBackendUnavailable(t *testing.T) {
	db := openWorkerStore(t)
	seedItem(t, db, "a", time.Hour)
	w := NewWorker(db, config.Static(preparedConfig(t, nil)), func(
		context.Context, config.Config, []string,
	) (Classifier, error) {
		return nil, errors.New("model down")
	})
	stats, _ := w.RunOnce(t.Context())
	if !stats.Skipped || stats.Reason != "backend unavailable" {
		t.Fatalf("stats = %+v", stats)
	}
	if _, ok, err := db.ClassificationState(t.Context(), "a"); err != nil || ok {
		t.Fatalf("backend failure recorded state: ok=%v err=%v", ok, err)
	}
	claims, err := db.ClaimClassification(t.Context(), Version, time.Now(), 10, 5, 30*time.Minute)
	if err != nil || len(claims) != 1 {
		t.Fatalf("item should still be claimable: %+v, %v", claims, err)
	}
}

func TestWorkerVersionBumpReclaims(t *testing.T) {
	db := openWorkerStore(t)
	seedItem(t, db, "a", time.Hour)
	fake := &fakeClassifier{verdict: map[string]ItemVerdict{"a": {Keywords: []string{"go"}}}}
	w := newTestWorker(t, db, preparedConfig(t, nil), fake)

	if stats, _ := w.RunOnce(t.Context()); stats.Classified != 1 {
		t.Fatalf("first pass stats = %+v", stats)
	}
	if stats, _ := w.RunOnce(t.Context()); stats.Candidates != 0 {
		t.Fatalf("second pass reclaimed an ok item: %+v", stats)
	}

	w.version = 2
	fake.verdict["a"] = ItemVerdict{Keywords: []string{"rust"}}
	if stats, _ := w.RunOnce(t.Context()); stats.Classified != 1 {
		t.Fatalf("version-bump pass stats = %+v", stats)
	}
	tags, err := db.TagsForItem(t.Context(), "a")
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 1 || tags[0].Slug != "rust" {
		t.Fatalf("tags = %+v, want rust", tags)
	}
}
