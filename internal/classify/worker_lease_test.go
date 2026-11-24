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

// Claims are just-in-time: the worker reserves one wave of batchSize ×
// max_parallel items immediately before it runs, never a claimLimit-sized
// backlog. While a wave's requests are in flight, the rest of the queue stays
// unclaimed — free for any other worker, and stranded for at most one lease if
// this one crashes. On the old worker all four items were claimed up front, so
// this test found four live claims.
func TestWorkerClaimsJustInTimeWaves(t *testing.T) {
	db := openWorkerStore(t)
	for i := range 4 {
		seedItem(t, db, string(rune('a'+i)), time.Duration(i+1)*time.Hour)
	}
	gate := newGatedClassifier()
	w := newTestWorker(t, db, parallelConfig(t, 2), gate) // batch 1, parallel 2: waves of 2

	done := make(chan Stats, 1)
	go func() { stats, _ := w.RunOnce(t.Context()); done <- stats }()

	awaitEntry(t, gate, "first request")
	awaitEntry(t, gate, "second request")
	claimed := 0
	for i := range 4 {
		state, ok, err := db.ClassificationState(t.Context(), string(rune('a'+i)))
		if err != nil {
			t.Fatalf("state for %c: %v", 'a'+i, err)
		}
		// An item outside the wave has no queue row at all: claims only queue
		// what they take.
		if ok && state.ClaimToken != "" {
			claimed++
		}
	}
	if claimed != 2 {
		t.Fatalf("%d items were claimed while one wave of 2 was in flight, want 2", claimed)
	}

	// The next wave is reserved only after this one completes.
	gate.release <- struct{}{}
	gate.release <- struct{}{}
	awaitEntry(t, gate, "third request")
	awaitEntry(t, gate, "fourth request")
	gate.release <- struct{}{}
	gate.release <- struct{}{}

	select {
	case stats := <-done:
		if stats.Classified != 4 || stats.Failed != 0 {
			t.Fatalf("stats = %+v", stats)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pass did not finish after all requests were released")
	}
}

// awaitSignal waits for one channel signal, failing the test on timeout.
func awaitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: no signal within 5s", what)
	}
}

// singleGateClassifier fails batch requests and gates each single-item
// request, so a test can intervene between the model call and the verdict
// write.
type singleGateClassifier struct {
	mu      sync.Mutex
	entries int
	entered chan struct{}
	release chan struct{}
}

func (c *singleGateClassifier) Validate(context.Context) error { return nil }

func (c *singleGateClassifier) Classify(_ context.Context, items []feeds.Item) ([]ItemVerdict, error) {
	if len(items) > 1 {
		return nil, errors.New("batch mode unsupported")
	}
	c.mu.Lock()
	c.entries++
	c.mu.Unlock()
	c.entered <- struct{}{}
	<-c.release
	return []ItemVerdict{{Index: 1, ID: items[0].ID, Keywords: []string{"go"}}}, nil
}

func (c *singleGateClassifier) totalEntries() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.entries
}

// When a wave's claim is taken over — here because another worker reclaimed
// past the lease while a request was in flight — the wave's remaining
// candidates must not consume further model requests: their verdicts could
// only be discarded. On the old worker every remaining candidate was still
// sent to the model, one by one, after ownership was lost.
func TestWorkerStopsWaveWhenClaimIsLost(t *testing.T) {
	db := openWorkerStore(t)
	for i := range 3 {
		seedItem(t, db, string(rune('a'+i)), time.Duration(i+1)*time.Hour)
	}
	// One batch of three: the wave is the batch, and the batch failure forces
	// the per-item fallback whose remaining items the stale claim must skip.
	cfg := preparedConfig(t, func(c *config.Config) {
		c.Classification.BatchSize = 3
	})
	fake := &singleGateClassifier{entered: make(chan struct{}, 8), release: make(chan struct{})}
	w := newTestWorker(t, db, cfg, fake)

	done := make(chan Stats, 1)
	errDone := make(chan error, 1)
	go func() {
		stats, err := w.RunOnce(t.Context())
		done <- stats
		errDone <- err
	}()

	awaitSignal(t, fake.entered, "first single-item request")
	// Another worker reclaims the wave's items from the far side of the lease:
	// the claim this worker holds is treated as expired and taken over.
	future := time.Now().Add(w.claimLease + time.Hour)
	reclaimed, err := db.ClaimClassification(t.Context(), Version, future, 3, 5, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(reclaimed) != 3 {
		t.Fatalf("reclaimed %d items, want the wave's 3", len(reclaimed))
	}
	// The in-flight request completes; its verdict write is rejected as stale.
	fake.release <- struct{}{}

	select {
	case stats := <-done:
		if got := fake.totalEntries(); got != 1 {
			t.Errorf("model requests = %d, want 1: the lost claim's remaining items must not consume requests", got)
		}
		if stats.Classified != 0 || stats.Failed != 0 {
			t.Errorf("stats = %+v, want no counted outcomes for a lost claim", stats)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pass did not finish after the claim was lost")
	}
	// Losing a claim is contention, not a broken pass: the reclaiming worker
	// owns the items and this one simply moves on.
	if err := <-errDone; err != nil {
		t.Fatalf("a lost claim must not fail the pass: %v", err)
	}
}

// batchGateClassifier gates one successful batch request, so a test can take
// the claim over between the model call and the verdict writes. Single-item
// requests count and fail: the batch already answered them.
type batchGateClassifier struct {
	mu      sync.Mutex
	singles int
	entered chan struct{}
	release chan struct{}
}

func (c *batchGateClassifier) Validate(context.Context) error { return nil }

func (c *batchGateClassifier) Classify(_ context.Context, items []feeds.Item) ([]ItemVerdict, error) {
	if len(items) == 1 {
		c.mu.Lock()
		c.singles++
		c.mu.Unlock()
		return nil, errors.New("the batch already covered this item")
	}
	c.entered <- struct{}{}
	<-c.release
	// A malformed batch answer: three verdicts, but the middle item's id is
	// duplicated, so one item of the batch has no verdict and would be
	// re-asked individually.
	out := []ItemVerdict{
		{Index: 1, ID: items[0].ID, Keywords: []string{"go"}},
		{Index: 2, ID: items[0].ID, Keywords: []string{"go"}},
		{Index: 3, ID: items[2].ID, Keywords: []string{"go"}},
	}
	return out, nil
}

func (c *batchGateClassifier) singleRequests() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.singles
}

// The same abandonment inside a batch whose answer is malformed: the middle
// item has no verdict and would be re-asked individually, but the first
// verdict write is rejected as stale — the lost claim's remaining items must
// not consume the fallback model requests, and nothing the newer owner would
// have to fight may be written.
func TestWorkerSkipsRemainingVerdictsWhenClaimIsLost(t *testing.T) {
	db := openWorkerStore(t)
	for i := range 3 {
		seedItem(t, db, string(rune('a'+i)), time.Duration(i+1)*time.Hour)
	}
	cfg := preparedConfig(t, func(c *config.Config) {
		c.Classification.BatchSize = 3
	})
	fake := &batchGateClassifier{entered: make(chan struct{}, 1), release: make(chan struct{})}
	w := newTestWorker(t, db, cfg, fake)

	done := make(chan Stats, 1)
	errDone := make(chan error, 1)
	go func() {
		stats, err := w.RunOnce(t.Context())
		done <- stats
		errDone <- err
	}()

	awaitSignal(t, fake.entered, "batch request")
	future := time.Now().Add(w.claimLease + time.Hour)
	reclaimed, err := db.ClaimClassification(t.Context(), Version, future, 3, 5, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(reclaimed) != 3 {
		t.Fatalf("reclaimed %d items, want the wave's 3", len(reclaimed))
	}
	fake.release <- struct{}{}

	select {
	case stats := <-done:
		if got := fake.singleRequests(); got != 0 {
			t.Errorf("fallback model requests = %d, want 0: the item the malformed batch"+
				" skipped must not be re-asked under a lost claim", got)
		}
		if stats.Classified != 0 || stats.Failed != 0 {
			t.Errorf("stats = %+v, want no counted outcomes for a lost claim", stats)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pass did not finish after the claim was lost")
	}
	if err := <-errDone; err != nil {
		t.Fatalf("a lost claim must not fail the pass: %v", err)
	}
	for i := range 3 {
		state, ok, err := db.ClassificationState(t.Context(), string(rune('a'+i)))
		if err != nil || !ok {
			t.Fatalf("state for %c = %+v, %v, %v", 'a'+i, state, ok, err)
		}
		if state.Status == store.ClassificationOK {
			t.Errorf("item %c was recorded ok under a lost claim", 'a'+i)
		}
	}
}
