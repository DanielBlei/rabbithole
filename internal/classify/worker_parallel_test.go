// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package classify

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// gatedClassifier blocks every Classify call until the test releases it and
// reports each entry, so a test can observe — and hold — exactly which
// requests the worker puts in flight.
type gatedClassifier struct {
	mu          sync.Mutex
	inFlight    int
	maxInFlight int
	entries     int

	entered chan struct{}
	release chan struct{}
}

func newGatedClassifier() *gatedClassifier {
	return &gatedClassifier{
		entered: make(chan struct{}, 64),
		release: make(chan struct{}),
	}
}

func (g *gatedClassifier) Validate(context.Context) error { return nil }

func (g *gatedClassifier) Classify(_ context.Context, items []feeds.Item) ([]ItemVerdict, error) {
	g.mu.Lock()
	g.entries++
	g.inFlight++
	if g.inFlight > g.maxInFlight {
		g.maxInFlight = g.inFlight
	}
	g.mu.Unlock()
	g.entered <- struct{}{}
	<-g.release
	g.mu.Lock()
	g.inFlight--
	g.mu.Unlock()

	out := make([]ItemVerdict, 0, len(items))
	for i, item := range items {
		out = append(out, ItemVerdict{Index: i + 1, ID: item.ID, Keywords: []string{"go"}})
	}
	return out, nil
}

func (g *gatedClassifier) totalEntries() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.entries
}

func (g *gatedClassifier) peakInFlight() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.maxInFlight
}

// awaitEntry waits for one classifier entry, failing the test on timeout.
func awaitEntry(t *testing.T, g *gatedClassifier, what string) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: no classification request arrived within 5s (entries so far: %d)",
			what, g.totalEntries())
	}
}

func parallelConfig(t *testing.T, maxParallel int) config.Config {
	t.Helper()
	return preparedConfig(t, func(c *config.Config) {
		c.Classification.BatchSize = 1
		c.Classification.MaxParallel = maxParallel
	})
}

// max_parallel=2 must actually overlap requests: with two items and single-item
// batches, the second request starts while the first is still in flight. On the
// old worker — which never read the setting — the second entry only arrives
// after the first is released, and this test times out.
func TestWorkerMaxParallelRunsBatchesConcurrently(t *testing.T) {
	db := openWorkerStore(t)
	seedItem(t, db, "a", time.Hour)
	seedItem(t, db, "b", 2*time.Hour)
	gate := newGatedClassifier()
	w := newTestWorker(t, db, parallelConfig(t, 2), gate)

	done := make(chan Stats, 1)
	go func() { stats, _ := w.RunOnce(t.Context()); done <- stats }()

	awaitEntry(t, gate, "first request")
	awaitEntry(t, gate, "second concurrent request")
	gate.release <- struct{}{}
	gate.release <- struct{}{}

	select {
	case stats := <-done:
		if stats.Classified != 2 || stats.Failed != 0 {
			t.Fatalf("stats = %+v", stats)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pass did not finish after both requests were released")
	}
	if peak := gate.peakInFlight(); peak != 2 {
		t.Errorf("peak in-flight requests = %d, want 2", peak)
	}
}

// The same semaphore must also hold the line: with max_parallel=2 and six
// items, a third request may never be in flight while two are held.
func TestWorkerMaxParallelIsBounded(t *testing.T) {
	db := openWorkerStore(t)
	for i := range 6 {
		seedItem(t, db, string(rune('a'+i)), time.Duration(i+1)*time.Hour)
	}
	gate := newGatedClassifier()
	w := newTestWorker(t, db, parallelConfig(t, 2), gate)

	done := make(chan Stats, 1)
	go func() { stats, _ := w.RunOnce(t.Context()); done <- stats }()

	for round := range 3 {
		awaitEntry(t, gate, "first request of round")
		awaitEntry(t, gate, "second request of round")
		select {
		case <-gate.entered:
			t.Fatalf("round %d: a third request was in flight with max_parallel=2", round)
		case <-time.After(150 * time.Millisecond):
		}
		gate.release <- struct{}{}
		gate.release <- struct{}{}
	}

	select {
	case stats := <-done:
		if stats.Classified != 6 || stats.Failed != 0 {
			t.Fatalf("stats = %+v", stats)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pass did not finish after all requests were released")
	}
	if peak := gate.peakInFlight(); peak != 2 {
		t.Errorf("peak in-flight requests = %d, want 2", peak)
	}
}

// max_parallel=1 keeps the sequential behaviour a single-request install
// expects: one request at a time, in claim order.
func TestWorkerMaxParallelOneStaysSequential(t *testing.T) {
	db := openWorkerStore(t)
	seedItem(t, db, "a", time.Hour)
	seedItem(t, db, "b", 2*time.Hour)
	gate := newGatedClassifier()
	w := newTestWorker(t, db, parallelConfig(t, 1), gate)

	done := make(chan Stats, 1)
	go func() { stats, _ := w.RunOnce(t.Context()); done <- stats }()

	awaitEntry(t, gate, "first request")
	select {
	case <-gate.entered:
		t.Fatal("a second request was in flight with max_parallel=1")
	case <-time.After(150 * time.Millisecond):
	}
	gate.release <- struct{}{}
	awaitEntry(t, gate, "second request")
	gate.release <- struct{}{}

	select {
	case stats := <-done:
		if stats.Classified != 2 || stats.Failed != 0 {
			t.Fatalf("stats = %+v", stats)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pass did not finish after both requests were released")
	}
	if peak := gate.peakInFlight(); peak != 1 {
		t.Errorf("peak in-flight requests = %d, want 1", peak)
	}
}

// A cancelled pass stops feeding the semaphore: requests already in flight
// finish (or fail) on their own, but no new batch starts.
func TestWorkerParallelPassStopsFeedingOnCancel(t *testing.T) {
	db := openWorkerStore(t)
	for i := range 4 {
		seedItem(t, db, string(rune('a'+i)), time.Duration(i+1)*time.Hour)
	}
	gate := newGatedClassifier()
	w := newTestWorker(t, db, parallelConfig(t, 2), gate)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan Stats, 1)
	go func() { stats, _ := w.RunOnce(ctx); done <- stats }()

	awaitEntry(t, gate, "first request")
	awaitEntry(t, gate, "second request")
	cancel()
	select {
	case <-gate.entered:
		t.Fatal("a new request started after the pass was cancelled")
	case <-time.After(150 * time.Millisecond):
	}
	gate.release <- struct{}{}
	gate.release <- struct{}{}

	select {
	case stats := <-done:
		// Claims are just-in-time: only the in-flight wave of two was
		// claimed, both requests ran, and neither could record an outcome on
		// a cancelled context.
		if stats.Candidates != 2 || stats.Classified != 0 || stats.Failed != 0 {
			t.Fatalf("stats = %+v, want the in-flight wave of 2 claimed and no recorded outcomes", stats)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled pass did not return")
	}
	if got := gate.totalEntries(); got != 2 {
		t.Errorf("total requests = %d, want 2 (no new batches after cancellation)", got)
	}
}

// closingClassifier closes the store from inside its first Classify call, so
// the verdict the worker is about to persist cannot be written.
type closingClassifier struct {
	once sync.Once
	db   *store.Store
}

func (c *closingClassifier) Validate(context.Context) error { return nil }

func (c *closingClassifier) Classify(_ context.Context, items []feeds.Item) ([]ItemVerdict, error) {
	c.once.Do(func() { _ = c.db.Close() })
	out := make([]ItemVerdict, 0, len(items))
	for i, item := range items {
		out = append(out, ItemVerdict{Index: i + 1, ID: item.ID, Keywords: []string{"go"}})
	}
	return out, nil
}

// A failure that cannot be persisted is not progress: the item is not counted,
// the pass stops instead of looping over items whose outcomes vanish, and the
// failure reaches the caller — a `classify backfill --wait` that exits zero
// must have actually recorded its work. On the old worker the failure was
// counted after a logged-and-ignored write error, so stats.Failed was 1,
// nothing had reached the database, and the CLI exited zero.
func TestWorkerStopsWhenFailureCannotBePersisted(t *testing.T) {
	db := openWorkerStore(t)
	seedItem(t, db, "a", time.Hour)
	w := newTestWorker(t, db, preparedConfig(t, nil), &closingClassifier{db: db})

	stats, err := w.RunOnce(t.Context())
	if err == nil {
		t.Fatal("RunOnce did not report the persistence failure")
	}
	if stats.Candidates != 1 {
		t.Fatalf("stats = %+v, want the claimed item processed", stats)
	}
	if stats.Classified != 0 || stats.Failed != 0 {
		t.Fatalf("stats = %+v, want no counted outcome for an unpersisted failure", stats)
	}
}

// A persistence failure under parallel workers cannot be swallowed: whichever
// goroutine hits it stops the pass, and the error surfaces from RunOnce along
// with the stats of the work that did land.
func TestWorkerParallelPassSurfacesPersistenceFailure(t *testing.T) {
	db := openWorkerStore(t)
	seedItem(t, db, "a", time.Hour)
	seedItem(t, db, "b", 2*time.Hour)
	w := newTestWorker(t, db, parallelConfig(t, 2), &closingClassifier{db: db})

	stats, err := w.RunOnce(t.Context())
	if err == nil {
		t.Fatal("RunOnce did not report the persistence failure")
	}
	if stats.Classified != 0 || stats.Failed != 0 {
		t.Fatalf("stats = %+v, want no counted outcomes for unpersisted work", stats)
	}
}

// The retry budget is per classifier version: an item that burned its budget at
// v1 gets a fresh one at v2. On the old queue the attempt count survived the
// bump, so a single v2 failure made the item terminal for a year.
func TestWorkerVersionBumpRestoresRetryBudget(t *testing.T) {
	db := openWorkerStore(t)
	seedItem(t, db, "a", time.Hour)
	failing := &fakeClassifier{failFor: map[string]error{"a": errors.New("down")}}
	w := newTestWorker(t, db, preparedConfig(t, nil), failing)
	// A backoff that outlives the rest of one pass (so a pass records at most
	// one failure) but is cleared by the sleep between synchronous passes.
	w.slowRetry = 50 * time.Millisecond
	w.maxAttempts = 2
	runPass := func() Stats {
		t.Helper()
		time.Sleep(100 * time.Millisecond)
		stats, _ := w.RunOnce(t.Context())
		return stats
	}

	if stats := runPass(); stats.Failed != 1 {
		t.Fatalf("first v1 pass stats = %+v", stats)
	}
	if stats := runPass(); stats.Failed != 1 {
		t.Fatalf("second v1 pass stats = %+v", stats)
	}
	state, ok, err := db.ClassificationState(t.Context(), "a")
	if err != nil || !ok {
		t.Fatalf("state = %+v, %v, %v", state, ok, err)
	}
	if state.Attempts != 2 || state.Status != store.ClassificationFailed {
		t.Fatalf("after two v1 failures state = %+v, want attempts 2 and failed", state)
	}
	// The budget is spent at v1: no further pass picks the item up.
	if stats := runPass(); stats.Candidates != 0 {
		t.Fatalf("exhausted item reclaimed at v1: %+v", stats)
	}

	w.version = 2
	if stats := runPass(); stats.Failed != 1 {
		t.Fatalf("v2 pass stats = %+v", stats)
	}
	state, _, err = db.ClassificationState(t.Context(), "a")
	if err != nil {
		t.Fatal(err)
	}
	if state.ClassifierVersion != 2 || state.Attempts != 1 {
		t.Fatalf("after one v2 failure state = %+v, want version 2 with a reset budget (attempts 1)", state)
	}
	if state.NextAttemptAt == nil || state.NextAttemptAt.After(time.Now().Add(time.Hour)) {
		t.Fatalf("v2 failure was terminal, want a normal backoff: %+v", state)
	}
	// And the restored budget means a later v2 pass can still try again.
	if stats := runPass(); stats.Failed != 1 {
		t.Fatalf("follow-up v2 pass stats = %+v, want the item retried", stats)
	}
	state, _, err = db.ClassificationState(t.Context(), "a")
	if err != nil {
		t.Fatal(err)
	}
	if state.Attempts != 2 {
		t.Fatalf("second v2 failure state = %+v, want attempts 2 (budget preserved within v2)", state)
	}
}
