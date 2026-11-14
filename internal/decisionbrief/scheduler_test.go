// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package decisionbrief

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/articlefetch"
	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// Additional E: generation admission is bounded in both the active and the
// waiting dimension. These tests drive the two channels directly so the
// scheduling decisions are deterministic rather than timing-dependent.

func fillActive(t *testing.T, svc *Service) {
	t.Helper()
	for i := 0; i < maxConcurrentGenerations; i++ {
		select {
		case svc.sem <- struct{}{}:
		default:
			t.Fatalf("active semaphore had room at token %d", i)
		}
	}
}

func fillQueue(t *testing.T, svc *Service) {
	t.Helper()
	for i := 0; i < maxQueuedGenerations; i++ {
		select {
		case svc.queue <- struct{}{}:
		default:
			t.Fatalf("queue had room at token %d", i)
		}
	}
}

// releaseActive frees exactly the held slots. Unlike a drain loop, it cannot
// race with blocked senders and steal tokens that a worker has just acquired.
func releaseActive(t *testing.T, svc *Service) {
	t.Helper()
	for i := 0; i < maxConcurrentGenerations; i++ {
		select {
		case <-svc.sem:
		case <-time.After(5 * time.Second):
			t.Fatalf("active slot %d was not held", i)
		}
	}
}

func drainSem(svc *Service) {
	for {
		select {
		case <-svc.sem:
		default:
			return
		}
	}
}

func drainQueue(svc *Service) {
	for {
		select {
		case <-svc.queue:
		default:
			return
		}
	}
}

func schedulerCounts(svc *Service) (active, queued, running int) {
	active, queued = len(svc.sem), len(svc.queue)
	svc.mu.Lock()
	running = len(svc.running)
	svc.mu.Unlock()
	return active, queued, running
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestBusyWhenActiveAndQueueFull(t *testing.T) {
	db := testStore(t, item("a"))
	var calls, fetches int32
	svc := New(db, testProfile(), func(context.Context) (Generator, Provenance, error) {
		return genFunc(func(context.Context, string, string, json.RawMessage, int) (string, error) {
			atomic.AddInt32(&calls, 1)
			return validJSON, nil
		}), Provenance{}, nil
	})
	stub := stubFetcher(validBody(), nil)
	svc.SetFetcher(func(ctx context.Context, raw string, limits articlefetch.Limits) (articlefetch.Result, error) {
		atomic.AddInt32(&fetches, 1)
		return stub(ctx, raw, limits)
	})

	fillActive(t, svc)
	fillQueue(t, svc)
	defer drainSem(svc)
	defer drainQueue(svc)

	if _, err := svc.Start(context.Background(), "a", false, Options{}); !errors.Is(err, ErrBriefBusy) {
		t.Fatalf("Start err = %v, want ErrBriefBusy", err)
	}
	if active, queued, running := schedulerCounts(svc); active != 2 || queued != 16 || running != 0 {
		t.Fatalf("counts = active %d queued %d running %d", active, queued, running)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("model calls = %d, want 0", got)
	}
	if got := atomic.LoadInt32(&fetches); got != 0 {
		t.Fatalf("fetches = %d, want 0", got)
	}
	// A rejected request creates no run and no state; the item stays idle.
	snap, err := svc.Status(context.Background(), "a")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if snap.Status != StatusIdle {
		t.Fatalf("status = %s, want idle", snap.Status)
	}
}

func TestQueuedGenerationPromotesWhenSlotFrees(t *testing.T) {
	db := testStore(t, item("a"))
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var calls int32
	svc := New(db, testProfile(), func(context.Context) (Generator, Provenance, error) {
		return genFunc(func(context.Context, string, string, json.RawMessage, int) (string, error) {
			atomic.AddInt32(&calls, 1)
			started <- struct{}{}
			<-release
			return validJSON, nil
		}), Provenance{}, nil
	})
	svc.SetFetcher(stubFetcher(validBody(), nil))

	fillActive(t, svc)
	defer drainSem(svc)
	snap, err := svc.Start(context.Background(), "a", false, Options{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if snap.Status != StatusQueued || snap.Phase != PhaseQueued {
		t.Fatalf("start snapshot = %+v, want queued", snap)
	}
	if _, queued, running := schedulerCounts(svc); queued != 1 || running != 1 {
		t.Fatalf("after Start: queued %d running %d, want 1/1", queued, running)
	}

	<-svc.sem // free one active slot
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("queued generation did not acquire the freed slot")
	}
	waitFor(t, "queue token release", func() bool { return len(svc.queue) == 0 })
	snap, err = svc.Status(context.Background(), "a")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if snap.Status != StatusRunning {
		t.Fatalf("status = %s, want running", snap.Status)
	}

	close(release)
	waitTerminal(t, svc, "a")
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("calls = %d, want 1", got)
	}
	waitFor(t, "tokens to settle", func() bool {
		active, queued, running := schedulerCounts(svc)
		return active == 1 && queued == 0 && running == 0
	})
}

func TestDuplicateStartWhileQueuedDoesNotConsumeAdmission(t *testing.T) {
	db := testStore(t, item("a"))
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var calls int32
	svc := New(db, testProfile(), func(context.Context) (Generator, Provenance, error) {
		return genFunc(func(context.Context, string, string, json.RawMessage, int) (string, error) {
			atomic.AddInt32(&calls, 1)
			started <- struct{}{}
			<-release
			return validJSON, nil
		}), Provenance{}, nil
	})
	svc.SetFetcher(stubFetcher(validBody(), nil))

	fillActive(t, svc)
	defer drainSem(svc)
	if _, err := svc.Start(context.Background(), "a", false, Options{}); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	snap, err := svc.Start(context.Background(), "a", false, Options{})
	if err != nil {
		t.Fatalf("second Start: %v", err)
	}
	if snap.Status != StatusQueued {
		t.Fatalf("second Start status = %s, want queued", snap.Status)
	}
	if _, queued, running := schedulerCounts(svc); queued != 1 || running != 1 {
		t.Fatalf("duplicate Start consumed admission: queued %d running %d", queued, running)
	}

	<-svc.sem
	<-started
	close(release)
	waitTerminal(t, svc, "a")
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("calls = %d, want 1", got)
	}
	waitFor(t, "tokens to settle", func() bool {
		active, queued, running := schedulerCounts(svc)
		return active == 1 && queued == 0 && running == 0
	})
}

func TestQueueTimeoutWhileWaitingReleasesTokens(t *testing.T) {
	db := testStore(t, item("a"))
	var calls int32
	svc := New(db, testProfile(), func(context.Context) (Generator, Provenance, error) {
		return genFunc(func(context.Context, string, string, json.RawMessage, int) (string, error) {
			atomic.AddInt32(&calls, 1)
			return validJSON, nil
		}), Provenance{}, nil
	})
	svc.generationTimeout = 80 * time.Millisecond
	svc.SetFetcher(stubFetcher(validBody(), nil))

	fillActive(t, svc)
	defer drainSem(svc)
	if _, err := svc.Start(context.Background(), "a", false, Options{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	snap := waitTerminal(t, svc, "a")
	if snap.Status != StatusFailed || snap.ErrorCode != CodeTimeout {
		t.Fatalf("snap = %+v, want failed timeout", snap)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("calls = %d, want 0", got)
	}
	waitFor(t, "queue and run release", func() bool {
		active, queued, running := schedulerCounts(svc)
		return active == 2 && queued == 0 && running == 0
	})
}

func TestRefreshWhileSaturatedReturnsBusyAndKeepsBrief(t *testing.T) {
	db := testStore(t, item("a"))
	if err := db.SaveItemBrief(context.Background(), store.ItemBrief{
		ItemID: "a", Status: store.ItemBriefReady, Why: "stored", Recommendation: "skim",
	}); err != nil {
		t.Fatalf("SaveItemBrief: %v", err)
	}
	var calls int32
	svc := New(db, testProfile(), func(context.Context) (Generator, Provenance, error) {
		return genFunc(func(context.Context, string, string, json.RawMessage, int) (string, error) {
			atomic.AddInt32(&calls, 1)
			return validJSON, nil
		}), Provenance{}, nil
	})
	svc.SetFetcher(stubFetcher(validBody(), nil))

	fillActive(t, svc)
	fillQueue(t, svc)
	defer drainSem(svc)
	defer drainQueue(svc)

	if _, err := svc.Start(context.Background(), "a", true, Options{}); !errors.Is(err, ErrBriefBusy) {
		t.Fatalf("refresh err = %v, want ErrBriefBusy", err)
	}
	// A non-refresh request still serves the stored brief without admission.
	snap, err := svc.Start(context.Background(), "a", false, Options{})
	if err != nil {
		t.Fatalf("cached Start: %v", err)
	}
	if snap.Status != StatusReady || snap.Brief.Why != "stored" {
		t.Fatalf("cached snapshot = %+v", snap)
	}
	stored, err := db.ItemBrief(context.Background(), "a")
	if err != nil {
		t.Fatalf("ItemBrief: %v", err)
	}
	if stored.Why != "stored" || stored.Status != store.ItemBriefReady {
		t.Fatalf("stored brief changed: %+v", stored)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("calls = %d, want 0", got)
	}
}

// A burst of Starts admits exactly the active slots plus the queue and rejects
// the rest immediately. The bound is checked at the admission point, before any
// goroutine exists, so a rejected request cannot leave a waiter behind.
func TestConcurrentStartsRespectAdmissionBound(t *testing.T) {
	items := make([]feeds.Item, 0, 20)
	for i := 0; i < 20; i++ {
		items = append(items, item(fmt.Sprintf("burst-%d", i)))
	}
	db := testStore(t, items...)
	var calls int32
	svc := New(db, testProfile(), func(context.Context) (Generator, Provenance, error) {
		return genFunc(func(context.Context, string, string, json.RawMessage, int) (string, error) {
			atomic.AddInt32(&calls, 1)
			return validJSON, nil
		}), Provenance{}, nil
	})
	svc.SetFetcher(stubFetcher(validBody(), nil))

	fillActive(t, svc)
	defer drainSem(svc)

	start := make(chan struct{})
	var wg sync.WaitGroup
	var ok, busy int32
	for _, it := range items {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			<-start
			if _, err := svc.Start(context.Background(), id, false, Options{}); err != nil {
				if errors.Is(err, ErrBriefBusy) {
					atomic.AddInt32(&busy, 1)
					return
				}
				t.Errorf("Start %s: %v", id, err)
				return
			}
			atomic.AddInt32(&ok, 1)
		}(it.ID)
	}
	close(start)
	wg.Wait()

	if gotOK, gotBusy := atomic.LoadInt32(&ok), atomic.LoadInt32(&busy); gotOK != maxQueuedGenerations || gotBusy != 4 {
		t.Fatalf("admitted %d, busy %d; want %d/%d", gotOK, gotBusy, maxQueuedGenerations, 4)
	}
	if _, queued, running := schedulerCounts(svc); queued != maxQueuedGenerations || running != maxQueuedGenerations {
		t.Fatalf("queued %d running %d, want %d/%d", queued, running, maxQueuedGenerations, maxQueuedGenerations)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("calls = %d while all slots were held, want 0", got)
	}

	// Free the held slots; every admitted dive runs to completion and every
	// token is released.
	releaseActive(t, svc)
	waitFor(t, "admitted dives to finish", func() bool {
		_, queued, running := schedulerCounts(svc)
		return queued == 0 && running == 0
	})
	if got := atomic.LoadInt32(&calls); got != maxQueuedGenerations {
		t.Fatalf("calls = %d, want %d", got, maxQueuedGenerations)
	}
	if active, queued, running := schedulerCounts(svc); active != 0 || queued != 0 || running != 0 {
		t.Fatalf("tokens leaked: active %d queued %d running %d", active, queued, running)
	}
}

// A fresh queue accepts work again after a saturated period drains.
func TestAdmissionRecoversAfterSaturation(t *testing.T) {
	db := testStore(t, item("a"))
	var calls int32
	svc := New(db, testProfile(), func(context.Context) (Generator, Provenance, error) {
		return genFunc(func(context.Context, string, string, json.RawMessage, int) (string, error) {
			atomic.AddInt32(&calls, 1)
			return validJSON, nil
		}), Provenance{}, nil
	})
	svc.SetFetcher(stubFetcher(validBody(), nil))

	fillActive(t, svc)
	fillQueue(t, svc)
	if _, err := svc.Start(context.Background(), "a", false, Options{}); !errors.Is(err, ErrBriefBusy) {
		t.Fatalf("Start err = %v, want ErrBriefBusy", err)
	}
	drainSem(svc)
	drainQueue(svc)

	if _, err := svc.Start(context.Background(), "a", false, Options{}); err != nil {
		t.Fatalf("Start after drain: %v", err)
	}
	if snap := waitTerminal(t, svc, "a"); snap.Status != StatusReady {
		t.Fatalf("status = %s (%s: %s)", snap.Status, snap.ErrorCode, snap.ErrorDetail)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("calls = %d, want 1", got)
	}
	if active, queued, running := schedulerCounts(svc); active != 0 || queued != 0 || running != 0 {
		t.Fatalf("tokens leaked: active %d queued %d running %d", active, queued, running)
	}
}
