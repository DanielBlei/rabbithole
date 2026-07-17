package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func openIngestStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, context.Background()
}

// A run's lifecycle: started as 'running' (no finished_at), then finished with
// its outcome and counts. LastIngestRun tracks the newest row throughout.
func TestIngestRunLifecycle(t *testing.T) {
	db, ctx := openIngestStore(t)

	last, err := db.LastIngestRun(ctx)
	if err != nil {
		t.Fatalf("LastIngestRun on fresh db: %v", err)
	}
	if last != nil {
		t.Fatalf("fresh db has a run: %+v", last)
	}

	id, err := db.StartIngestRun(ctx, IngestTriggerManual)
	if err != nil {
		t.Fatalf("StartIngestRun: %v", err)
	}
	last, err = db.LastIngestRun(ctx)
	if err != nil {
		t.Fatalf("LastIngestRun: %v", err)
	}
	if last == nil || last.ID != id || last.Status != IngestStatusRunning || last.FinishedAt != nil {
		t.Fatalf("running row wrong: %+v", last)
	}
	if last.TriggeredBy != IngestTriggerManual {
		t.Errorf("triggered_by = %q, want manual", last.TriggeredBy)
	}

	counts := IngestCounts{Fetched: 214, NewItems: 12, Scored: 12, Skipped: 183, Failed: 0}
	if err := db.FinishIngestRun(ctx, id, IngestStatusOK, counts, ""); err != nil {
		t.Fatalf("FinishIngestRun: %v", err)
	}
	last, err = db.LastIngestRun(ctx)
	if err != nil {
		t.Fatalf("LastIngestRun: %v", err)
	}
	if last.Status != IngestStatusOK || last.FinishedAt == nil || last.Counts != counts {
		t.Fatalf("finished row wrong: %+v", last)
	}

	// Finishing an already-finished run is not found (the WHERE pins 'running').
	if err := db.FinishIngestRun(ctx, id, IngestStatusOK, counts, ""); !errors.Is(err, ErrIngestRunNotFound) {
		t.Errorf("double finish: got %v, want ErrIngestRunNotFound", err)
	}
}

// ListIngestRuns returns newest-first and honors the limit; error runs carry
// their message.
func TestIngestRunListAndErrors(t *testing.T) {
	db, ctx := openIngestStore(t)

	first, err := db.StartIngestRun(ctx, IngestTriggerCron)
	if err != nil {
		t.Fatalf("StartIngestRun: %v", err)
	}
	if err := db.FinishIngestRun(
		ctx,
		first,
		IngestStatusError,
		IngestCounts{},
		"vllm: connection refused",
	); err != nil {
		t.Fatalf("FinishIngestRun: %v", err)
	}
	second, err := db.StartIngestRun(ctx, IngestTriggerManual)
	if err != nil {
		t.Fatalf("StartIngestRun: %v", err)
	}
	if err := db.FinishIngestRun(ctx, second, IngestStatusCancelled, IngestCounts{}, "context canceled"); err != nil {
		t.Fatalf("FinishIngestRun: %v", err)
	}

	runs, hasMore, err := db.ListIngestRuns(ctx, 10, 0)
	if err != nil {
		t.Fatalf("ListIngestRuns: %v", err)
	}
	if hasMore {
		t.Errorf("hasMore true with 2 runs under a limit of 10")
	}
	if len(runs) != 2 || runs[0].ID != second || runs[1].ID != first {
		t.Fatalf("runs not newest-first: %+v", runs)
	}
	if runs[1].Error != "vllm: connection refused" || runs[1].TriggeredBy != IngestTriggerCron {
		t.Errorf("error row wrong: %+v", runs[1])
	}

	limited, hasMore, err := db.ListIngestRuns(ctx, 1, 0)
	if err != nil {
		t.Fatalf("ListIngestRuns limit: %v", err)
	}
	if !hasMore {
		t.Errorf("hasMore false with 2 runs under a limit of 1")
	}
	if len(limited) != 1 || limited[0].ID != second {
		t.Errorf("limit not applied: %+v", limited)
	}

	page2, hasMore, err := db.ListIngestRuns(ctx, 1, 1)
	if err != nil {
		t.Fatalf("ListIngestRuns offset: %v", err)
	}
	if hasMore {
		t.Errorf("hasMore true on the last page")
	}
	if len(page2) != 1 || page2[0].ID != first {
		t.Errorf("offset not applied: %+v", page2)
	}
}

// InterruptStaleIngestRuns flips leftover 'running' rows to error/interrupted
// (crash recovery) and leaves finished rows alone.
func TestInterruptStaleIngestRuns(t *testing.T) {
	db, ctx := openIngestStore(t)

	done, err := db.StartIngestRun(ctx, IngestTriggerManual)
	if err != nil {
		t.Fatalf("StartIngestRun: %v", err)
	}
	if err := db.FinishIngestRun(ctx, done, IngestStatusOK, IngestCounts{Scored: 3}, ""); err != nil {
		t.Fatalf("FinishIngestRun: %v", err)
	}
	stale, err := db.StartIngestRun(ctx, IngestTriggerManual)
	if err != nil {
		t.Fatalf("StartIngestRun: %v", err)
	}

	if err := db.InterruptStaleIngestRuns(ctx); err != nil {
		t.Fatalf("InterruptStaleIngestRuns: %v", err)
	}

	runs, _, err := db.ListIngestRuns(ctx, 10, 0)
	if err != nil {
		t.Fatalf("ListIngestRuns: %v", err)
	}
	for _, r := range runs {
		switch r.ID {
		case stale:
			if r.Status != IngestStatusError || r.Error != "interrupted" || r.FinishedAt == nil {
				t.Errorf("stale row not interrupted: %+v", r)
			}
		case done:
			if r.Status != IngestStatusOK || r.Error != "" {
				t.Errorf("finished row touched: %+v", r)
			}
		}
	}
}
