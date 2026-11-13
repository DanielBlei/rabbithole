// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package classify

import (
	"database/sql"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// openTriggerWorkerStore opens a worker store whose file path the test keeps,
// so a second connection can install schema objects into the same database.
func openTriggerWorkerStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "classify.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}

// rejectItemTagsInserts installs a trigger that fails every insert into
// item_tags — the write a verdict needs to be recorded — while every other
// write (items, the classification queue, failure marks, the tags themselves)
// keeps working. That is the failure shape a broken result write must be
// reported for: the model answered, the verdict cannot be stored, and the
// ordinary failure path is still fully operational — the exact combination
// that used to turn a database failure into a counted, non-fatal one.
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

// A verdict that cannot be persisted is fatal for the pass: RunOnce returns
// the persistence error, the item is counted neither as classified nor as an
// ordinary model failure, no failure mark launders the outcome, and no
// further classification work is scheduled. On the old worker the trigger's
// error went through recordFailure, whose still-working failure mark turned
// it into one counted failure — and the pass, and `classify backfill --wait`,
// completed successfully over a database that had recorded nothing.
func TestWorkerVerdictPersistenceFailureIsFatal(t *testing.T) {
	t.Run("sequential", func(t *testing.T) {
		db, path := openTriggerWorkerStore(t)
		seedItem(t, db, "a", time.Hour)
		seedItem(t, db, "b", 2*time.Hour)
		rejectItemTagsInserts(t, path)
		fake := &fakeClassifier{verdict: map[string]ItemVerdict{
			"a": {Keywords: []string{"go"}},
			"b": {Keywords: []string{"rust"}},
		}}
		cfg := preparedConfig(t, func(c *config.Config) { c.Classification.BatchSize = 1 })
		w := newTestWorker(t, db, cfg, fake)

		stats, err := w.RunOnce(t.Context())
		if err == nil {
			t.Fatal("RunOnce did not report the verdict persistence failure")
		}
		if stats.Candidates != 1 || stats.Classified != 0 || stats.Failed != 0 {
			t.Fatalf("stats = %+v, want one candidate and no counted outcomes", stats)
		}
		if got := fake.callCount(); got != 1 {
			t.Errorf("model requests = %d, want 1: no work beyond the failed write", got)
		}
		state, ok, err := db.ClassificationState(t.Context(), "a")
		if err != nil || !ok {
			t.Fatalf("state for a = %+v, %v, %v", state, ok, err)
		}
		if state.Status != store.ClassificationPending || state.Attempts != 0 || state.LastError != "" {
			t.Fatalf("state for a = %+v, want pending with the retry budget untouched", state)
		}
		if _, ok, err := db.ClassificationState(t.Context(), "b"); err != nil || ok {
			t.Fatalf("state for b = ok %v err %v, want the second item never scheduled", ok, err)
		}
	})

	t.Run("max_parallel=2", func(t *testing.T) {
		db, path := openTriggerWorkerStore(t)
		seedItem(t, db, "a", time.Hour)
		seedItem(t, db, "b", 2*time.Hour)
		rejectItemTagsInserts(t, path)
		fake := &fakeClassifier{verdict: map[string]ItemVerdict{
			"a": {Keywords: []string{"go"}},
			"b": {Keywords: []string{"rust"}},
		}}
		w := newTestWorker(t, db, parallelConfig(t, 2), fake)

		stats, err := w.RunOnce(t.Context())
		if err == nil {
			t.Fatal("RunOnce did not report the verdict persistence failure")
		}
		if stats.Candidates != 2 || stats.Classified != 0 || stats.Failed != 0 {
			t.Fatalf("stats = %+v, want the in-flight wave of 2 and no counted outcomes", stats)
		}
		// Both wave items were asked before the failure; neither may be asked
		// again, and neither outcome may be recorded.
		if got := fake.callCount(); got != 2 {
			t.Errorf("model requests = %d, want 2 (the wave, with no retries)", got)
		}
		for _, id := range []string{"a", "b"} {
			state, ok, err := db.ClassificationState(t.Context(), id)
			if err != nil || !ok {
				t.Fatalf("state for %s = %+v, %v, %v", id, state, ok, err)
			}
			if state.Status != store.ClassificationPending || state.Attempts != 0 || state.LastError != "" {
				t.Fatalf("state for %s = %+v, want pending with the retry budget untouched", id, state)
			}
		}
	})
}

// A model-authored tag the store refuses is an ordinary model failure, not a
// persistence failure: the item is marked failed with its retry budget, the
// pass counts it in Stats.Failed, and neither the pass nor the command fails.
func TestWorkerInvalidModelTagsAreOrdinaryFailures(t *testing.T) {
	db := openWorkerStore(t)
	seedItem(t, db, "a", time.Hour)
	fake := &fakeClassifier{verdict: map[string]ItemVerdict{
		"a": {Keywords: []string{strings.Repeat("x", 200)}},
	}}
	w := newTestWorker(t, db, preparedConfig(t, nil), fake)

	stats, err := w.RunOnce(t.Context())
	if err != nil {
		t.Fatalf("an invalid model tag must not fail the pass: %v", err)
	}
	if stats.Classified != 0 || stats.Failed != 1 {
		t.Fatalf("stats = %+v, want one ordinary failure counted", stats)
	}
	state, ok, err := db.ClassificationState(t.Context(), "a")
	if err != nil || !ok {
		t.Fatalf("state = %+v, %v, %v", state, ok, err)
	}
	if state.Status != store.ClassificationFailed || state.Attempts != 1 || state.LastError == "" {
		t.Fatalf("state = %+v, want a recorded failure with one attempt spent", state)
	}
	if state.NextAttemptAt == nil || !state.NextAttemptAt.After(time.Now()) {
		t.Errorf("next attempt = %v, want a future retry", state.NextAttemptAt)
	}
}
