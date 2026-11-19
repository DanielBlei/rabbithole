// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/classify"
	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// The --wait path must fail when the pass cannot persist: a backfill that
// exits zero has actually been recorded. The database is closed after seeding,
// so the worker's pass fails at its first statement and the failure travels
// worker → RunOnce → waitBackfill → the command's error return. On the old
// command the pass stopped with logged warnings and the CLI exited zero.
func TestClassifyBackfillWaitSurfacesPersistenceFailure(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "classify.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	item := feeds.Item{
		ID: "a", Source: "S", Title: "Title a", Link: "https://x/a",
		Summary: "summary", Published: time.Now().UTC().Add(-time.Hour),
	}
	if err := db.Record(context.Background(), []feeds.Item{item}, []store.DigestEntry{{
		Item: item, Score: 8, Reason: "relevant",
	}}, time.Now()); err != nil {
		t.Fatalf("Record: %v", err)
	}
	cfg := config.Config{Inference: config.InferenceConfig{Provider: config.ProviderOllama}}
	if _, err := cfg.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	// From here every statement on the store fails.
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	worker := classify.NewWorker(db, config.Static(cfg), func(
		context.Context, config.Config, []string,
	) (classify.Classifier, error) {
		// Never reached: the pass fails before it resolves the backend, and
		// an unreachable model must stay a retry, not a command failure.
		return nil, errors.New("the model is never reached while the store is down")
	})
	if err := waitBackfill(context.Background(), worker, nil); err == nil {
		t.Fatal("waitBackfill reported success for a pass that could not persist")
	}
}

// verdictClassifier answers every request with a well-formed verdict, standing
// in for a healthy model backend.
type verdictClassifier struct{}

func (verdictClassifier) Validate(context.Context) error { return nil }

func (verdictClassifier) Classify(_ context.Context, items []feeds.Item) ([]classify.ItemVerdict, error) {
	out := make([]classify.ItemVerdict, 0, len(items))
	for i, item := range items {
		out = append(out, classify.ItemVerdict{Index: i + 1, ID: item.ID, Keywords: []string{"go"}})
	}
	return out, nil
}

// rejectItemTagsInserts fails every insert into item_tags — the write that
// records a classification verdict — while every other write, including the
// failure marks of the ordinary model-failure path, keeps working.
func rejectItemTagsInserts(t *testing.T, path string) {
	t.Helper()
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	ctrl, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		t.Fatalf("open control connection: %v", err)
	}
	defer func() { _ = ctrl.Close() }()
	if _, err := ctrl.Exec(`CREATE TRIGGER reject_item_tags_insert
		BEFORE INSERT ON item_tags
		BEGIN SELECT RAISE(ABORT, 'injected item_tags failure'); END`); err != nil {
		t.Fatalf("create item_tags trigger: %v", err)
	}
}

// The precise result-write failure the QA gate reproduced: the model answers,
// the verdict cannot be recorded, and the failure mark that would relabel the
// error as an ordinary model failure is itself still operational. The command
// must not exit successfully over work that was never recorded — waitBackfill
// returns the persistence error, so runClassifyBackfill's error return, and
// the command's exit code, are non-zero. On the old worker the item was
// counted as one ordinary failure and the command exited zero.
func TestClassifyBackfillWaitFailsWhenVerdictsCannotBeStored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "classify.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	item := feeds.Item{
		ID: "a", Source: "S", Title: "Title a", Link: "https://x/a",
		Summary: "summary", Published: time.Now().UTC().Add(-time.Hour),
	}
	if err := db.Record(context.Background(), []feeds.Item{item}, []store.DigestEntry{{
		Item: item, Score: 8, Reason: "relevant",
	}}, time.Now()); err != nil {
		t.Fatalf("Record: %v", err)
	}
	rejectItemTagsInserts(t, path)
	cfg := config.Config{Inference: config.InferenceConfig{Provider: config.ProviderOllama}}
	if _, err := cfg.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	worker := classify.NewWorker(db, config.Static(cfg), func(
		context.Context, config.Config, []string,
	) (classify.Classifier, error) {
		return verdictClassifier{}, nil
	})
	if err := waitBackfill(context.Background(), worker, nil); err == nil {
		t.Fatal("waitBackfill reported success for a pass whose verdicts could not be stored")
	}
	state, ok, err := db.ClassificationState(context.Background(), "a")
	if err != nil || !ok {
		t.Fatalf("state = %+v, %v, %v", state, ok, err)
	}
	if state.Status != store.ClassificationPending || state.Attempts != 0 || state.LastError != "" {
		t.Fatalf("state = %+v, want pending: the failure mark must not relabel a result-write failure", state)
	}
}

// stubBackfillClassifier makes the command's real synchronous path run without
// a model backend and resets the package-level backfill flags between runs.
func stubBackfillClassifier(t *testing.T) {
	t.Helper()
	oldResolve := resolveBackfillClassifier
	resolveBackfillClassifier = func(
		context.Context, config.ClassificationConfig, []string,
	) (classify.Classifier, error) {
		return verdictClassifier{}, nil
	}
	oldSince, oldAll, oldLimit, oldWait := backfillSince, backfillAll, backfillLimit, backfillWait
	backfillSince, backfillAll, backfillLimit, backfillWait = "720h", false, 0, false
	t.Cleanup(func() {
		resolveBackfillClassifier = oldResolve
		backfillSince, backfillAll, backfillLimit, backfillWait = oldSince, oldAll, oldLimit, oldWait
	})
}

func seedBackfillItems(t *testing.T, path string, items ...feeds.Item) {
	t.Helper()
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	for _, item := range items {
		if err := db.Record(context.Background(), []feeds.Item{item}, []store.DigestEntry{{
			Item: item, Score: 8, Reason: "relevant",
		}}, time.Now()); err != nil {
			t.Fatalf("Record(%s): %v", item.ID, err)
		}
	}
}

func backfillState(t *testing.T, path, id string) (store.ClassificationState, bool) {
	t.Helper()
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	state, ok, err := db.ClassificationState(context.Background(), id)
	if err != nil {
		t.Fatalf("ClassificationState(%s): %v", id, err)
	}
	return state, ok
}

// --since must bound the synchronous pass as well as the enqueue. The old
// worker rediscovered every scored item in ClaimClassification, so --wait
// classified the whole history even though only the recent item was queued.
func TestClassifyBackfillWaitHonoursSince(t *testing.T) {
	stubBackfillClassifier(t)
	e := newCLIEnv(t)
	now := time.Now().UTC()
	seedBackfillItems(t, e.db,
		feeds.Item{
			ID: "recent", Source: "S", Title: "Recent", Link: "https://x/recent",
			Summary: "summary", Published: now.Add(-time.Hour),
		},
		feeds.Item{
			ID: "old", Source: "S", Title: "Old", Link: "https://x/old",
			Summary: "summary", Published: now.Add(-30 * 24 * time.Hour),
		},
	)

	if _, _, err := e.run("classify", "backfill", "--since", "7d", "--wait"); err != nil {
		t.Fatalf("classify backfill --since 7d --wait: %v", err)
	}
	if state, ok := backfillState(t, e.db, "recent"); !ok || state.Status != store.ClassificationOK {
		t.Fatalf("recent state = %+v, %v, want ok", state, ok)
	}
	if state, ok := backfillState(t, e.db, "old"); ok && state.Status == store.ClassificationOK {
		t.Fatalf("old state = %+v, want an item outside --since left unclassified", state)
	}
}

// --limit must cap the synchronous pass too. Enqueueing one item while letting
// the worker claim the rest of the queue made --limit cosmetic.
func TestClassifyBackfillWaitHonoursLimit(t *testing.T) {
	stubBackfillClassifier(t)
	e := newCLIEnv(t)
	now := time.Now().UTC()
	seedBackfillItems(t, e.db,
		feeds.Item{
			ID: "newest", Source: "S", Title: "Newest", Link: "https://x/newest",
			Summary: "summary", Published: now.Add(-time.Hour),
		},
		feeds.Item{
			ID: "older", Source: "S", Title: "Older", Link: "https://x/older",
			Summary: "summary", Published: now.Add(-2 * time.Hour),
		},
	)

	_, _, err := e.run("classify", "backfill", "--since", "7d", "--limit", "1", "--wait")
	if err != nil {
		t.Fatalf("classify backfill --since 7d --limit 1 --wait: %v", err)
	}
	if state, ok := backfillState(t, e.db, "newest"); !ok || state.Status != store.ClassificationOK {
		t.Fatalf("newest state = %+v, %v, want ok", state, ok)
	}
	if state, ok := backfillState(t, e.db, "older"); ok && state.Status == store.ClassificationOK {
		t.Fatalf("older state = %+v, want an item beyond --limit left unclassified", state)
	}
}
