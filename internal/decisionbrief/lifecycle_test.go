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
	"github.com/DanielBlei/rabbithole/internal/store"
)

// hookedStore wraps the real store and runs a hook after a specific read or
// write returns. Hooks run on the caller's goroutine, so a hook can park a
// service critical section while the test arranges the interleaving under test.
type hookedStore struct {
	*store.Store
	itemBrief  func(context.Context)
	itemBriefs func(context.Context)
	save       func(context.Context, store.ItemBrief)
}

func (h *hookedStore) ItemBrief(ctx context.Context, id string) (store.ItemBrief, error) {
	b, err := h.Store.ItemBrief(ctx, id)
	if h.itemBrief != nil {
		h.itemBrief(ctx)
	}
	return b, err
}

func (h *hookedStore) ItemBriefsByItemIDs(
	ctx context.Context,
	ids []string,
) (map[string]store.ItemBrief, error) {
	briefs, err := h.Store.ItemBriefsByItemIDs(ctx, ids)
	if h.itemBriefs != nil {
		h.itemBriefs(ctx)
	}
	return briefs, err
}

func (h *hookedStore) SaveItemBrief(ctx context.Context, b store.ItemBrief) error {
	err := h.Store.SaveItemBrief(ctx, b)
	if h.save != nil {
		h.save(ctx, b)
	}
	return err
}

// TestStatusesDoesNotMissCompletion is finding #9's core interleaving: the
// persisted read has already returned when the generation commits. The run is
// still present and removal waits on the service lock, so the poll must report
// a non-terminal state and keep polling until the committed result is visible.
func TestStatusesDoesNotMissCompletion(t *testing.T) {
	for _, refresh := range []bool{false, true} {
		t.Run(fmt.Sprintf("refresh=%v", refresh), func(t *testing.T) {
			db := testStore(t, item("a"))
			if refresh {
				score := 3
				if err := db.SaveItemBrief(context.Background(), store.ItemBrief{
					ItemID: "a", Status: store.ItemBriefReady, Score: &score,
					Recommendation: "skim", Why: "previous brief",
					KeyPoints: []string{"old"}, Novelty: "old", Caveats: "old", Focus: "old",
				}); err != nil {
					t.Fatalf("seed previous brief: %v", err)
				}
			}

			readDone := make(chan struct{})
			readRelease := make(chan struct{})
			saved := make(chan struct{})
			var readOnce, saveOnce sync.Once
			h := &hookedStore{Store: db}
			h.itemBriefs = func(context.Context) {
				readOnce.Do(func() {
					close(readDone)
					<-readRelease
				})
			}
			h.save = func(_ context.Context, b store.ItemBrief) {
				if b.Status == store.ItemBriefReady {
					saveOnce.Do(func() { close(saved) })
				}
			}

			genStarted := make(chan struct{})
			genRelease := make(chan struct{})
			var genOnce sync.Once
			svc := New(h, testProfile(), func(context.Context) (Generator, Provenance, error) {
				return genFunc(func(context.Context, string, string, json.RawMessage, int) (string, error) {
					genOnce.Do(func() { close(genStarted) })
					<-genRelease
					return validJSON, nil
				}), Provenance{Provider: "test", Model: "test"}, nil
			})
			svc.SetFetcher(stubFetcher(validBody(), nil))

			if _, err := svc.Start(context.Background(), "a", refresh, Options{}); err != nil {
				t.Fatalf("Start: %v", err)
			}
			<-genStarted

			type pollResult struct {
				snaps map[string]Snapshot
				err   error
			}
			polled := make(chan pollResult, 1)
			go func() {
				snaps, err := svc.Statuses(context.Background(), []string{"a"})
				polled <- pollResult{snaps: snaps, err: err}
			}()

			// Statuses holds the service lock and has completed its persisted
			// read; the snapshot it will build is the pre-save one.
			<-readDone
			close(genRelease)
			// The model result is committed; the run's removal is waiting for
			// the lock Statuses holds.
			<-saved
			close(readRelease)

			got := <-polled
			if got.err != nil {
				t.Fatalf("Statuses: %v", got.err)
			}
			snap := got.snaps["a"]
			if snap.Status != StatusRunning && snap.Status != StatusQueued {
				t.Fatalf("poll during completion returned terminal %q; want running/queued", snap.Status)
			}

			final := waitTerminal(t, svc, "a")
			if final.Status != StatusReady {
				t.Fatalf("final status = %+v", final)
			}
			if final.Brief.Score == nil || *final.Brief.Score != 8 {
				t.Fatalf("final brief = %+v, want the committed result", final.Brief)
			}
			if refresh && final.Brief.Why == "previous brief" {
				t.Fatal("refresh kept the pre-refresh brief")
			}
		})
	}
}

// TestStartWhileCompletionCommits races two Starts against the window where the
// result is committed but the run is still present. Single-flight must hold:
// no second generation, and a later Start sees the ready brief.
func TestStartWhileCompletionCommits(t *testing.T) {
	db := testStore(t, item("a"))
	var genCalls int32
	genStarted := make(chan struct{})
	var genOnce sync.Once
	saved := make(chan struct{})
	release := make(chan struct{})
	var saveOnce sync.Once
	h := &hookedStore{Store: db}
	h.save = func(_ context.Context, b store.ItemBrief) {
		if b.Status == store.ItemBriefReady {
			saveOnce.Do(func() {
				close(saved)
				<-release
			})
		}
	}
	svc := New(h, testProfile(), func(context.Context) (Generator, Provenance, error) {
		return genFunc(func(context.Context, string, string, json.RawMessage, int) (string, error) {
			atomic.AddInt32(&genCalls, 1)
			genOnce.Do(func() { close(genStarted) })
			return validJSON, nil
		}), Provenance{Provider: "test", Model: "test"}, nil
	})
	svc.SetFetcher(stubFetcher(validBody(), nil))

	if _, err := svc.Start(context.Background(), "a", false, Options{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	<-genStarted
	<-saved // the result is committed; the service is still inside SaveItemBrief

	var wg sync.WaitGroup
	results := make(chan Snapshot, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snap, err := svc.Start(context.Background(), "a", false, Options{})
			if err != nil {
				t.Errorf("racing Start: %v", err)
				return
			}
			results <- snap
		}()
	}
	wg.Wait()
	close(results)
	for snap := range results {
		if snap.Status != StatusRunning && snap.Status != StatusQueued {
			t.Errorf("racing Start returned %q, want running/queued", snap.Status)
		}
	}
	close(release)

	final := waitTerminal(t, svc, "a")
	if final.Status != StatusReady {
		t.Fatalf("final status = %+v", final)
	}
	if got := atomic.LoadInt32(&genCalls); got != 1 {
		t.Fatalf("model calls = %d, want 1", got)
	}
	// After completion a normal start is a cache hit, not another dive.
	snap, err := svc.Start(context.Background(), "a", false, Options{})
	if err != nil {
		t.Fatalf("post-completion Start: %v", err)
	}
	if snap.Status != StatusReady {
		t.Fatalf("post-completion snapshot = %+v", snap)
	}
	if got := atomic.LoadInt32(&genCalls); got != 1 {
		t.Fatalf("model calls after completion = %d, want 1", got)
	}
}

// TestConcurrentPollingDoesNotDeadlock exercises the service lock with many
// concurrent readers while a generation is in flight.
func TestConcurrentPollingDoesNotDeadlock(t *testing.T) {
	db := testStore(t, item("a"))
	genStarted := make(chan struct{})
	genRelease := make(chan struct{})
	var genOnce sync.Once
	svc := New(db, testProfile(), func(context.Context) (Generator, Provenance, error) {
		return genFunc(func(context.Context, string, string, json.RawMessage, int) (string, error) {
			genOnce.Do(func() { close(genStarted) })
			<-genRelease
			return validJSON, nil
		}), Provenance{}, nil
	})
	svc.SetFetcher(stubFetcher(validBody(), nil))
	if _, err := svc.Start(context.Background(), "a", false, Options{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	<-genStarted

	const pollers = 16
	const rounds = 10
	var wg sync.WaitGroup
	wg.Add(pollers)
	for i := 0; i < pollers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < rounds; j++ {
				snap, err := svc.Status(context.Background(), "a")
				if err != nil {
					t.Errorf("poll: %v", err)
					return
				}
				if snap.Status != StatusRunning && snap.Status != StatusQueued {
					t.Errorf("poll status = %q, want running/queued", snap.Status)
					return
				}
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent polling deadlocked")
	}
	close(genRelease)
	if snap := waitTerminal(t, svc, "a"); snap.Status != StatusReady {
		t.Fatalf("final status = %+v", snap)
	}
}

// TestStartTransientBriefLookupError is finding #8: a failed persisted read is
// not a cache miss and must not fetch or generate.
func TestStartTransientBriefLookupError(t *testing.T) {
	sentinel := errors.New("brief table unavailable")
	for _, refresh := range []bool{false, true} {
		t.Run(fmt.Sprintf("refresh=%v", refresh), func(t *testing.T) {
			db := testStore(t, item("a"))
			score := 5
			if err := db.SaveItemBrief(context.Background(), store.ItemBrief{
				ItemID: "a", Status: store.ItemBriefReady, Score: &score,
				Recommendation: "read", Why: "stored", KeyPoints: []string{"one"},
				Novelty: "new", Caveats: "small", Focus: "setup",
			}); err != nil {
				t.Fatalf("seed ready brief: %v", err)
			}
			h := &failingBriefStore{Store: db, err: sentinel}
			var fetchCalls, genCalls int32
			svc := New(h, testProfile(), func(context.Context) (Generator, Provenance, error) {
				return genFunc(func(context.Context, string, string, json.RawMessage, int) (string, error) {
					atomic.AddInt32(&genCalls, 1)
					return validJSON, nil
				}), Provenance{}, nil
			})
			svc.SetFetcher(func(context.Context, string, articlefetch.Limits) (articlefetch.Result, error) {
				atomic.AddInt32(&fetchCalls, 1)
				return articlefetch.Result{}, errors.New("must not fetch")
			})

			if _, err := svc.Start(context.Background(), "a", refresh, Options{}); !errors.Is(err, sentinel) {
				t.Fatalf("Start error = %v, want %v", err, sentinel)
			}
			if got := atomic.LoadInt32(&fetchCalls); got != 0 {
				t.Fatalf("fetch calls = %d, want 0", got)
			}
			if got := atomic.LoadInt32(&genCalls); got != 0 {
				t.Fatalf("model calls = %d, want 0", got)
			}
			svc.mu.Lock()
			running := len(svc.running)
			svc.mu.Unlock()
			if running != 0 {
				t.Fatalf("running entries = %d, want 0", running)
			}
			stored, err := db.ItemBrief(context.Background(), "a")
			if err != nil || stored.Status != store.ItemBriefReady || stored.Why != "stored" {
				t.Fatalf("stored ready brief changed: %+v, %v", stored, err)
			}
		})
	}

	// Concurrent callers must not bypass the error handling either.
	db := testStore(t, item("a"))
	h := &failingBriefStore{Store: db, err: sentinel}
	var genCalls int32
	svc := New(h, testProfile(), func(context.Context) (Generator, Provenance, error) {
		return genFunc(func(context.Context, string, string, json.RawMessage, int) (string, error) {
			atomic.AddInt32(&genCalls, 1)
			return validJSON, nil
		}), Provenance{}, nil
	})
	var wg sync.WaitGroup
	wg.Add(8)
	for i := 0; i < 8; i++ {
		go func() {
			defer wg.Done()
			if _, err := svc.Start(context.Background(), "a", false, Options{}); !errors.Is(err, sentinel) {
				t.Errorf("concurrent Start error = %v, want %v", err, sentinel)
			}
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt32(&genCalls); got != 0 {
		t.Fatalf("concurrent model calls = %d, want 0", got)
	}
}

// failingBriefStore returns a fixed error from the persisted read.
type failingBriefStore struct {
	*store.Store
	err error
}

func (s *failingBriefStore) ItemBrief(context.Context, string) (store.ItemBrief, error) {
	return store.ItemBrief{}, s.err
}

// deadlineStore records the deadline of the context used to persist a failed
// brief.
type deadlineStore struct {
	*store.Store
	failureDeadline chan time.Time
}

func (d *deadlineStore) SaveItemBrief(ctx context.Context, b store.ItemBrief) error {
	if b.Status == store.ItemBriefFailed {
		if deadline, ok := ctx.Deadline(); ok {
			select {
			case d.failureDeadline <- deadline:
			default:
			}
		}
	}
	return d.Store.SaveItemBrief(ctx, b)
}

// TestTimeoutFailurePersistsAndSurvivesRestart is finding #10: the generation
// context expires, the failure write uses a fresh bounded context, exactly one
// model call is made, and a new Service sees the persisted failure.
func TestTimeoutFailurePersistsAndSurvivesRestart(t *testing.T) {
	db := testStore(t, item("a"))
	deadlines := make(chan time.Time, 1)
	h := &deadlineStore{Store: db, failureDeadline: deadlines}
	var calls int32
	gen := func(context.Context) (Generator, Provenance, error) {
		return genFunc(func(ctx context.Context, _, _ string, _ json.RawMessage, _ int) (string, error) {
			atomic.AddInt32(&calls, 1)
			<-ctx.Done()
			return "", ctx.Err()
		}), Provenance{Provider: "test", Model: "test"}, nil
	}
	svc := New(h, testProfile(), gen)
	svc.generationTimeout = 500 * time.Millisecond
	svc.SetFetcher(stubFetcher(validBody(), nil))

	if _, err := svc.Start(context.Background(), "a", false, Options{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	snap := waitTerminal(t, svc, "a")
	if snap.Status != StatusFailed || snap.ErrorCode != CodeTimeout {
		t.Fatalf("snapshot = %+v, want failed/timeout", snap)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("model calls = %d, want 1 (no retry after deadline)", got)
	}
	select {
	case deadline := <-deadlines:
		if deadline.IsZero() {
			t.Fatal("failure persistence had no deadline")
		}
		if remaining := time.Until(deadline); remaining <= 0 || remaining > failurePersistTimeout {
			t.Fatalf("failure persistence deadline = %v, want a finite positive bound", remaining)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("failure persistence did not use a bounded context")
	}

	stored, err := db.ItemBrief(context.Background(), "a")
	if err != nil || stored.Status != store.ItemBriefFailed || stored.ErrorCode != CodeTimeout {
		t.Fatalf("stored failure = %+v, %v", stored, err)
	}

	// A new Service has no in-memory failure; the status comes from the store.
	restarted := New(db, testProfile(), gen)
	again, err := restarted.Status(context.Background(), "a")
	if err != nil {
		t.Fatalf("restarted Status: %v", err)
	}
	if again.Status != StatusFailed || again.ErrorCode != CodeTimeout {
		t.Fatalf("restarted snapshot = %+v, want failed/timeout", again)
	}
}

// TestAnalyzeDoesNotRetryTerminalContext pins Additional A directly.
func TestAnalyzeDoesNotRetryTerminalContext(t *testing.T) {
	t.Run("deadline reports timeout with one call", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		var calls int32
		gen := genFunc(func(ctx context.Context, _, _ string, _ json.RawMessage, _ int) (string, error) {
			atomic.AddInt32(&calls, 1)
			return "", ctx.Err()
		})
		svc := New(nil, nil, nil)
		_, code, err := svc.analyze(ctx, gen, "prompt", Options{MaxOutputTokens: 64})
		if err == nil || code != CodeTimeout {
			t.Fatalf("code = %q, err = %v; want timeout", code, err)
		}
		if got := atomic.LoadInt32(&calls); got != 1 {
			t.Fatalf("model calls = %d, want 1", got)
		}
	})

	t.Run("cancellation reports analysis failure with one call", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var calls int32
		gen := genFunc(func(ctx context.Context, _, _ string, _ json.RawMessage, _ int) (string, error) {
			atomic.AddInt32(&calls, 1)
			return "", ctx.Err()
		})
		svc := New(nil, nil, nil)
		_, code, err := svc.analyze(ctx, gen, "prompt", Options{MaxOutputTokens: 64})
		if err == nil || code != CodeAnalysisFailed {
			t.Fatalf("code = %q, err = %v; want analysis failure", code, err)
		}
		if got := atomic.LoadInt32(&calls); got != 1 {
			t.Fatalf("model calls = %d, want 1", got)
		}
	})

	t.Run("ordinary failure still retries once", func(t *testing.T) {
		var calls int32
		gen := genFunc(func(context.Context, string, string, json.RawMessage, int) (string, error) {
			atomic.AddInt32(&calls, 1)
			return "", errors.New("transient provider error")
		})
		svc := New(nil, nil, nil)
		_, code, err := svc.analyze(context.Background(), gen, "prompt", Options{MaxOutputTokens: 64})
		if err == nil || code != CodeAnalysisFailed {
			t.Fatalf("code = %q, err = %v; want analysis failure", code, err)
		}
		if got := atomic.LoadInt32(&calls); got != 2 {
			t.Fatalf("model calls = %d, want 2", got)
		}
	})

	t.Run("retry can succeed", func(t *testing.T) {
		var calls int32
		gen := genFunc(func(context.Context, string, string, json.RawMessage, int) (string, error) {
			if atomic.AddInt32(&calls, 1) == 1 {
				return "", errors.New("transient provider error")
			}
			return validJSON, nil
		})
		svc := New(nil, nil, nil)
		brief, code, err := svc.analyze(context.Background(), gen, "prompt", Options{MaxOutputTokens: 64})
		if err != nil || code != "" {
			t.Fatalf("code = %q, err = %v", code, err)
		}
		if brief.Score != 8 || calls != 2 {
			t.Fatalf("brief = %+v, calls = %d", brief, calls)
		}
	})
}

// TestLateFailureCannotReplaceNewerBrief pins the superseded-run guard in fail.
func TestLateFailureCannotReplaceNewerBrief(t *testing.T) {
	db := testStore(t, item("a"))
	score := 7
	if err := db.SaveItemBrief(context.Background(), store.ItemBrief{
		ItemID: "a", Status: store.ItemBriefReady, Score: &score,
		Recommendation: "read", Why: "current", KeyPoints: []string{"one"},
		Novelty: "new", Caveats: "small", Focus: "setup",
	}); err != nil {
		t.Fatalf("seed ready brief: %v", err)
	}
	svc := New(db, testProfile(), nil)
	oldRun := &run{phase: PhaseAnalyzing}
	newRun := &run{phase: PhaseAnalyzing}
	svc.mu.Lock()
	svc.running["a"] = newRun
	svc.mu.Unlock()

	svc.fail(context.Background(), store.ItemRow{ID: "a"}, oldRun, Options{},
		Provenance{}, CodeTimeout, errors.New("late failure"))

	stored, err := db.ItemBrief(context.Background(), "a")
	if err != nil || stored.Status != store.ItemBriefReady || stored.Why != "current" {
		t.Fatalf("late failure overwrote a newer brief: %+v, %v", stored, err)
	}
	svc.mu.Lock()
	_, failed := svc.failures["a"]
	svc.mu.Unlock()
	if failed {
		t.Fatal("late failure was recorded in memory")
	}
}
