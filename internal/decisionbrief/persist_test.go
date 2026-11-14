// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package decisionbrief

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/DanielBlei/rabbithole/internal/store"
)

// generationExpiryStore delays a ready write until the generation context has
// expired, reproducing the post-inference deadline window.
type generationExpiryStore struct {
	*store.Store
	expired <-chan struct{}
	entered chan struct{}
	once    sync.Once
}

func (s *generationExpiryStore) SaveItemBrief(ctx context.Context, b store.ItemBrief) error {
	if b.Status == store.ItemBriefReady {
		s.once.Do(func() { close(s.entered) })
		<-s.expired
	}
	return s.Store.SaveItemBrief(ctx, b)
}

// resultStore can fail a ready write and records the deadline it was given.
type resultStore struct {
	*store.Store
	failReady     bool
	readyDeadline chan time.Time
}

func (s *resultStore) SaveItemBrief(ctx context.Context, b store.ItemBrief) error {
	if b.Status == store.ItemBriefReady {
		if deadline, ok := ctx.Deadline(); ok {
			select {
			case s.readyDeadline <- deadline:
			default:
			}
		}
		if s.failReady {
			return context.DeadlineExceeded
		}
	}
	return s.Store.SaveItemBrief(ctx, b)
}

func readyGenerator() GeneratorFunc {
	return func(context.Context) (Generator, Provenance, error) {
		return genFunc(func(context.Context, string, string, json.RawMessage, int) (string, error) {
			return validJSON, nil
		}), Provenance{Provider: "test", Model: "test"}, nil
	}
}

// storageFailureStore injects a database failure into brief writes. A ready
// write is the successful-result path; failAll also breaks the terminal-failure
// write so the both-writes-fail case can be exercised.
type storageFailureStore struct {
	*store.Store
	err     error
	failAll bool
}

func (s *storageFailureStore) SaveItemBrief(ctx context.Context, b store.ItemBrief) error {
	if s.failAll || b.Status == store.ItemBriefReady {
		return s.err
	}
	return s.Store.SaveItemBrief(ctx, b)
}

// TestResultPersistsAfterGenerationDeadline is the QA blocker: inference
// finishes, the generation deadline expires while the result is being written,
// and the validated result must still be persisted with its own bounded
// context.
func TestResultPersistsAfterGenerationDeadline(t *testing.T) {
	db := testStore(t, item("a"))
	expired := make(chan struct{})
	entered := make(chan struct{})
	h := &generationExpiryStore{Store: db, expired: expired, entered: entered}

	var closeOnce sync.Once
	svc := New(h, testProfile(), func(context.Context) (Generator, Provenance, error) {
		return genFunc(func(ctx context.Context, _, _ string, _ json.RawMessage, _ int) (string, error) {
			go func() {
				<-ctx.Done()
				closeOnce.Do(func() { close(expired) })
			}()
			return validJSON, nil
		}), Provenance{Provider: "test", Model: "test"}, nil
	})
	svc.generationTimeout = 100 * time.Millisecond
	svc.SetFetcher(stubFetcher(validBody(), nil))

	if _, err := svc.Start(context.Background(), "a", false, Options{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("result save was not attempted")
	}

	snap := waitTerminal(t, svc, "a")
	if snap.Status != StatusReady {
		t.Fatalf("status = %s (%s: %s), want ready after the deadline",
			snap.Status, snap.ErrorCode, snap.ErrorDetail)
	}
	stored, err := db.ItemBrief(context.Background(), "a")
	if err != nil || stored.Status != store.ItemBriefReady {
		t.Fatalf("stored brief = %+v, %v; want ready", stored, err)
	}

	// Restart durability: a new Service reads the persisted ready brief.
	restarted := New(db, testProfile(), nil)
	again, err := restarted.Status(context.Background(), "a")
	if err != nil {
		t.Fatalf("restarted Status: %v", err)
	}
	if again.Status != StatusReady {
		t.Fatalf("restarted status = %+v, want ready", again)
	}
}

// TestResultPersistenceFailureFallsBackToTimeout pins the fallback: a result
// write that fails with a deadline must be classified as timeout and persisted
// through the terminal-failure path, not downgraded to an in-memory analysis
// failure.
func TestResultPersistenceFailureFallsBackToTimeout(t *testing.T) {
	db := testStore(t, item("a"))
	h := &resultStore{Store: db, failReady: true, readyDeadline: make(chan time.Time, 1)}
	svc := New(h, testProfile(), readyGenerator())
	svc.SetFetcher(stubFetcher(validBody(), nil))

	if _, err := svc.Start(context.Background(), "a", false, Options{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	snap := waitTerminal(t, svc, "a")
	if snap.Status != StatusFailed || snap.ErrorCode != CodeTimeout {
		t.Fatalf("snapshot = %+v, want failed/timeout", snap)
	}
	stored, err := db.ItemBrief(context.Background(), "a")
	if err != nil || stored.Status != store.ItemBriefFailed || stored.ErrorCode != CodeTimeout {
		t.Fatalf("stored failure = %+v, %v; want failed/timeout", stored, err)
	}

	restarted := New(db, testProfile(), nil)
	again, err := restarted.Status(context.Background(), "a")
	if err != nil {
		t.Fatalf("restarted Status: %v", err)
	}
	if again.Status != StatusFailed || again.ErrorCode != CodeTimeout {
		t.Fatalf("restarted snapshot = %+v, want failed/timeout", again)
	}
}

// TestResultPersistenceFailurePreservesPreviousReady pins the refresh rule: a
// failed result write never replaces the previous ready brief.
func TestResultPersistenceFailurePreservesPreviousReady(t *testing.T) {
	db := testStore(t, item("a"))
	score := 3
	if err := db.SaveItemBrief(context.Background(), store.ItemBrief{
		ItemID: "a", Status: store.ItemBriefReady, Score: &score,
		Recommendation: "skim", Why: "previous brief",
		KeyPoints: []string{"old"}, Novelty: "old", Caveats: "old", Focus: "old",
	}); err != nil {
		t.Fatalf("seed previous brief: %v", err)
	}
	h := &resultStore{Store: db, failReady: true, readyDeadline: make(chan time.Time, 1)}
	svc := New(h, testProfile(), readyGenerator())
	svc.SetFetcher(stubFetcher(validBody(), nil))

	if _, err := svc.Start(context.Background(), "a", true, Options{}); err != nil {
		t.Fatalf("refresh Start: %v", err)
	}
	snap := waitTerminal(t, svc, "a")
	if snap.Status != StatusFailed || snap.ErrorCode != CodeTimeout {
		t.Fatalf("refresh snapshot = %+v, want failed/timeout", snap)
	}
	if snap.Brief.Status != store.ItemBriefReady || snap.Brief.Why != "previous brief" {
		t.Fatalf("previous brief not offered: %+v", snap.Brief)
	}
	stored, err := db.ItemBrief(context.Background(), "a")
	if err != nil || stored.Status != store.ItemBriefReady || stored.Why != "previous brief" {
		t.Fatalf("stored ready brief changed: %+v, %v", stored, err)
	}

	// The failure was intentionally not persisted for a failed refresh; a
	// restart sees the preserved ready brief.
	restarted := New(db, testProfile(), nil)
	again, err := restarted.Status(context.Background(), "a")
	if err != nil {
		t.Fatalf("restarted Status: %v", err)
	}
	if again.Status != StatusReady || again.Brief.Why != "previous brief" {
		t.Fatalf("restarted snapshot = %+v, want the previous ready brief", again)
	}
}

// TestResultPersistenceUsesBoundedContext pins the separate, finite write
// deadline on the successful path.
func TestResultPersistenceUsesBoundedContext(t *testing.T) {
	db := testStore(t, item("a"))
	deadlines := make(chan time.Time, 1)
	h := &resultStore{Store: db, readyDeadline: deadlines}
	svc := New(h, testProfile(), readyGenerator())
	svc.SetFetcher(stubFetcher(validBody(), nil))

	if _, err := svc.Start(context.Background(), "a", false, Options{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if snap := waitTerminal(t, svc, "a"); snap.Status != StatusReady {
		t.Fatalf("status = %+v, want ready", snap)
	}
	select {
	case deadline := <-deadlines:
		if deadline.IsZero() {
			t.Fatal("result persistence had no deadline")
		}
		if remaining := time.Until(deadline); remaining <= 0 || remaining > resultPersistTimeout {
			t.Fatalf("result persistence deadline = %v, want a finite positive bound", remaining)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("result persistence did not use a bounded context")
	}
}

// TestLateSuccessCannotReplaceNewerBrief pins the superseded-run guard on the
// success path.
func TestLateSuccessCannotReplaceNewerBrief(t *testing.T) {
	db := testStore(t, item("a"))
	svc := New(db, testProfile(), nil)
	oldRun := &run{phase: PhaseAnalyzing}
	newRun := &run{phase: PhaseAnalyzing}
	svc.mu.Lock()
	svc.running["a"] = newRun
	svc.mu.Unlock()

	score := 8
	b := store.ItemBrief{
		ItemID: "a", Status: store.ItemBriefReady, Score: &score,
		Recommendation: "read", Why: "superseded", KeyPoints: []string{"one"},
		Novelty: "new", Caveats: "small", Focus: "setup",
	}
	if svc.persistResult(context.Background(), store.ItemRow{ID: "a"}, oldRun,
		Options{}, Provenance{}, b) {
		t.Fatal("superseded result was persisted")
	}
	if _, err := db.ItemBrief(context.Background(), "a"); !errors.Is(err, store.ErrItemBriefNotFound) {
		t.Fatalf("superseded brief row = %v, want ErrItemBriefNotFound", err)
	}
}

// TestResultPersistenceStorageFailureIsNotAnalysisFailure is the regression:
// inference and validation succeed, only the database write fails, and the
// failure must be attributed to storage rather than to the model.
func TestResultPersistenceStorageFailureIsNotAnalysisFailure(t *testing.T) {
	db := testStore(t, item("a"))
	dbErr := errors.New("disk I/O error")
	h := &storageFailureStore{Store: db, err: dbErr}
	svc := New(h, testProfile(), readyGenerator())
	svc.SetFetcher(stubFetcher(validBody(), nil))

	if _, err := svc.Start(context.Background(), "a", false, Options{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	snap := waitTerminal(t, svc, "a")
	if snap.Status != StatusFailed || snap.ErrorCode != CodeStorageFailed {
		t.Fatalf("snapshot = %+v, want failed/%s, not a model failure",
			snap, CodeStorageFailed)
	}
	if snap.ErrorDetail != "" {
		t.Fatalf("snapshot detail = %q, want no raw database error", snap.ErrorDetail)
	}

	stored, err := db.ItemBrief(context.Background(), "a")
	if err != nil || stored.Status != store.ItemBriefFailed ||
		stored.ErrorCode != CodeStorageFailed || stored.ErrorDetail != "" {
		t.Fatalf("stored failure = %+v, %v; want failed/%s with no detail",
			stored, err, CodeStorageFailed)
	}

	// The storage failure is durable: a restart still reports it, and it is
	// still attributed to storage.
	restarted := New(db, testProfile(), nil)
	again, err := restarted.Status(context.Background(), "a")
	if err != nil {
		t.Fatalf("restarted Status: %v", err)
	}
	if again.Status != StatusFailed || again.ErrorCode != CodeStorageFailed {
		t.Fatalf("restarted snapshot = %+v, want failed/%s", again, CodeStorageFailed)
	}
}

// TestStorageAndFailureWritesBothFail pins the honest degraded state: the
// failure is visible in memory, the raw database error is logged, no durable
// row is claimed, and a restart does not pretend the failure was persisted.
func TestStorageAndFailureWritesBothFail(t *testing.T) {
	db := testStore(t, item("a"))
	var logs bytes.Buffer
	logger := zerolog.New(&logs).Level(zerolog.DebugLevel)
	ctx := logger.WithContext(context.Background())
	dbErr := errors.New("pq: password authentication failed for user admin " +
		"(dsn=postgres://admin:hunter2@db.example/app)")
	h := &storageFailureStore{Store: db, err: dbErr, failAll: true}
	svc := New(h, testProfile(), readyGenerator())
	svc.SetFetcher(stubFetcher(validBody(), nil))

	if _, err := svc.Start(ctx, "a", false, Options{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	snap := waitTerminal(t, svc, "a")
	if snap.Status != StatusFailed || snap.ErrorCode != CodeStorageFailed {
		t.Fatalf("snapshot = %+v, want failed/%s from memory", snap, CodeStorageFailed)
	}
	if snap.ErrorDetail != "" {
		t.Fatalf("snapshot detail = %q, want no raw database error", snap.ErrorDetail)
	}

	if _, err := db.ItemBrief(context.Background(), "a"); !errors.Is(err, store.ErrItemBriefNotFound) {
		t.Fatalf("durable brief = %v, want ErrItemBriefNotFound", err)
	}
	out := logs.String()
	if !strings.Contains(out, "storing result failed") {
		t.Errorf("logs do not record the result-write failure: %s", out)
	}
	if !strings.Contains(out, "storing terminal failure failed") {
		t.Errorf("logs do not record the terminal-write failure: %s", out)
	}
	if strings.Contains(out, "hunter2") || strings.Contains(out, "password") ||
		strings.Contains(out, "postgres://") {
		t.Errorf("logs leaked database credentials: %s", out)
	}
	if !strings.Contains(out, `"error_class":"storage_error"`) {
		t.Errorf("logs lack safe storage classification: %s", out)
	}

	// Nothing durable was written, so a restarted Service is honestly idle.
	restarted := New(db, testProfile(), nil)
	again, err := restarted.Status(context.Background(), "a")
	if err != nil {
		t.Fatalf("restarted Status: %v", err)
	}
	if again.Status != StatusIdle {
		t.Fatalf("restarted snapshot = %+v, want idle without a durable row", again)
	}
}

// TestStorageFailurePreservesPreviousReadyBrief pins the refresh rule for the
// storage classification: the old ready brief survives and the new failure is
// reported as a storage failure.
func TestStorageFailurePreservesPreviousReadyBrief(t *testing.T) {
	db := testStore(t, item("a"))
	score := 3
	if err := db.SaveItemBrief(context.Background(), store.ItemBrief{
		ItemID: "a", Status: store.ItemBriefReady, Score: &score,
		Recommendation: "skim", Why: "previous brief",
		KeyPoints: []string{"old"}, Novelty: "old", Caveats: "old", Focus: "old",
	}); err != nil {
		t.Fatalf("seed previous brief: %v", err)
	}
	h := &storageFailureStore{Store: db, err: errors.New("disk I/O error")}
	svc := New(h, testProfile(), readyGenerator())
	svc.SetFetcher(stubFetcher(validBody(), nil))

	if _, err := svc.Start(context.Background(), "a", true, Options{}); err != nil {
		t.Fatalf("refresh Start: %v", err)
	}
	snap := waitTerminal(t, svc, "a")
	if snap.Status != StatusFailed || snap.ErrorCode != CodeStorageFailed {
		t.Fatalf("refresh snapshot = %+v, want failed/%s", snap, CodeStorageFailed)
	}
	if snap.Brief.Status != store.ItemBriefReady || snap.Brief.Why != "previous brief" {
		t.Fatalf("previous brief not offered: %+v", snap.Brief)
	}
	stored, err := db.ItemBrief(context.Background(), "a")
	if err != nil || stored.Status != store.ItemBriefReady || stored.Why != "previous brief" {
		t.Fatalf("stored ready brief changed: %+v, %v", stored, err)
	}

	restarted := New(db, testProfile(), nil)
	again, err := restarted.Status(context.Background(), "a")
	if err != nil {
		t.Fatalf("restarted Status: %v", err)
	}
	if again.Status != StatusReady || again.Brief.Why != "previous brief" {
		t.Fatalf("restarted snapshot = %+v, want the previous ready brief", again)
	}
}

// TestInferenceFailureKeepsAnalysisCode guards the boundary: a genuine model
// failure still carries analysis_failed and is not relabeled as storage.
func TestInferenceFailureKeepsAnalysisCode(t *testing.T) {
	db := testStore(t, item("a"))
	gen := func(context.Context) (Generator, Provenance, error) {
		return genFunc(func(context.Context, string, string, json.RawMessage, int) (string, error) {
			return "", errors.New("model exploded")
		}), Provenance{Provider: "test", Model: "test"}, nil
	}
	svc := New(db, testProfile(), gen)
	svc.SetFetcher(stubFetcher(validBody(), nil))

	if _, err := svc.Start(context.Background(), "a", false, Options{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	snap := waitTerminal(t, svc, "a")
	if snap.Status != StatusFailed || snap.ErrorCode != CodeAnalysisFailed {
		t.Fatalf("snapshot = %+v, want failed/%s", snap, CodeAnalysisFailed)
	}
	stored, err := db.ItemBrief(context.Background(), "a")
	if err != nil || stored.Status != store.ItemBriefFailed ||
		stored.ErrorCode != CodeAnalysisFailed {
		t.Fatalf("stored failure = %+v, %v; want failed/%s", stored, err, CodeAnalysisFailed)
	}
}
