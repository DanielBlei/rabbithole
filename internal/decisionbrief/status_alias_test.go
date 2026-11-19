// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package decisionbrief

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/articlefetch"
	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/store"
)

func waitTerminalByID(t *testing.T, svc *Service, id string) Snapshot {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		snap, err := svc.StatusByID(context.Background(), id)
		if err != nil {
			t.Fatalf("StatusByID: %v", err)
		}
		switch snap.Status {
		case StatusReady, StatusFailed, StatusIdle:
			return snap
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("brief %s did not finish", id)
	return Snapshot{}
}

func collisionStore(t *testing.T) *store.Store {
	t.Helper()
	return testStore(t,
		feeds.Item{ID: "alias-item", Source: "S", Title: "A", Link: "x"},
		feeds.Item{ID: "x", Source: "S", Title: "B", Link: "https://example.com/x"},
	)
}

func blockingBriefService(t *testing.T, db *store.Store) (*Service, chan struct{}, chan struct{}) {
	t.Helper()
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	svc := New(db, testProfile(), func(context.Context) (Generator, Provenance, error) {
		return genFunc(func(context.Context, string, string, json.RawMessage, int) (string, error) {
			started <- struct{}{}
			<-release
			return validJSON, nil
		}), Provenance{}, nil
	})
	svc.SetFetcher(stubFetcher(validBody(), nil))
	return svc, started, release
}

func TestStatusResolvesAliasWhileRunning(t *testing.T) {
	link := item("a").Link
	tests := []struct {
		name   string
		start  string
		status string
	}{
		{"alias start, alias status", link, link},
		{"alias start, canonical status", link, "a"},
		{"canonical start, alias status", "a", link},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := testStore(t, item("a"))
			svc, started, release := blockingBriefService(t, db)
			if _, err := svc.Start(context.Background(), tc.start, false, Options{}); err != nil {
				t.Fatalf("Start: %v", err)
			}
			<-started
			snap, err := svc.Status(context.Background(), tc.status)
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			if snap.Status != StatusRunning || snap.ItemID != "a" {
				t.Fatalf("snapshot = %+v, want running canonical a", snap)
			}
			close(release)
			waitTerminal(t, svc, "a")
		})
	}
}

func TestIdentifierCollisionPrecedence(t *testing.T) {
	db := collisionStore(t)
	svc, started, release := blockingBriefService(t, db)
	if _, err := svc.Start(context.Background(), "x", false, Options{}); err != nil {
		t.Fatalf("Start(x): %v", err)
	}
	<-started

	snap, err := svc.Status(context.Background(), "x")
	if err != nil {
		t.Fatalf("Status(x): %v", err)
	}
	if snap.Status != StatusRunning || snap.ItemID != "alias-item" {
		t.Fatalf("Status(x) = %+v, want running link owner alias-item", snap)
	}
	snaps, err := svc.Statuses(context.Background(), []string{"x"})
	if err != nil {
		t.Fatalf("Statuses(x): %v", err)
	}
	if len(snaps) != 1 || snaps["alias-item"].Status != StatusRunning {
		t.Fatalf("Statuses(x) = %v, want canonical alias-item running", keys(snaps))
	}
	// The exact-ID path is distinct and must still address item B.
	if byID, err := svc.StatusByID(context.Background(), "x"); err != nil || byID.Status != StatusIdle {
		t.Fatalf("StatusByID(x) = %+v, %v; want idle item B", byID, err)
	}
	close(release)
	waitTerminal(t, svc, "x")
}

func TestExactIDLookupAvoidsLinkCollision(t *testing.T) {
	db := collisionStore(t)
	svc, started, release := blockingBriefService(t, db)
	if _, err := svc.StartByID(context.Background(), "x", false, Options{}); err != nil {
		t.Fatalf("StartByID(x): %v", err)
	}
	<-started

	byID, err := svc.StatusByID(context.Background(), "x")
	if err != nil {
		t.Fatalf("StatusByID(x): %v", err)
	}
	if byID.Status != StatusRunning || byID.ItemID != "x" {
		t.Fatalf("StatusByID(x) = %+v, want running item B", byID)
	}
	alias, err := svc.Status(context.Background(), "x")
	if err != nil {
		t.Fatalf("Status(x): %v", err)
	}
	if alias.Status != StatusIdle || alias.ItemID != "alias-item" {
		t.Fatalf("Status(x) = %+v, want idle link owner alias-item", alias)
	}
	close(release)
	if snap := waitTerminalByID(t, svc, "x"); snap.Status != StatusReady {
		t.Fatalf("StatusByID(x) after completion = %+v, want ready", snap)
	}
}

func TestStatusesCanonicalizesMixedAliases(t *testing.T) {
	db := testStore(t, item("a"), item("b"), item("c"))
	if err := db.SaveItemBrief(context.Background(), store.ItemBrief{
		ItemID: "b", Status: store.ItemBriefReady, Why: "stored", Recommendation: "skim",
	}); err != nil {
		t.Fatalf("SaveItemBrief: %v", err)
	}
	svc := New(db, testProfile(), nil)
	svc.mu.Lock()
	svc.running["a"] = &run{phase: PhaseAnalyzing}
	svc.failures["c"] = failure{code: CodeFetchBlocked, detail: "blocked"}
	svc.mu.Unlock()

	got, err := svc.Statuses(context.Background(), []string{
		item("a").Link, "a", "b", item("c").Link, "missing", item("a").Link,
	})
	if err != nil {
		t.Fatalf("Statuses: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("result keys = %v, want canonical a, b, c and the unknown raw id", keys(got))
	}
	if snap := got["a"]; snap.Status != StatusRunning || snap.ItemID != "a" {
		t.Errorf("a = %+v, want running", snap)
	}
	if snap := got["b"]; snap.Status != StatusReady || snap.ItemID != "b" {
		t.Errorf("b = %+v, want ready", snap)
	}
	if snap := got["c"]; snap.Status != StatusFailed || snap.ErrorCode != CodeFetchBlocked || snap.ItemID != "c" {
		t.Errorf("c = %+v, want failed", snap)
	}
	if snap := got["missing"]; snap.Status != StatusIdle || snap.ItemID != "missing" {
		t.Errorf("missing = %+v, want idle under the supplied id", snap)
	}
	for _, alias := range []string{item("a").Link, item("c").Link} {
		if _, ok := got[alias]; ok {
			t.Errorf("result is also keyed by alias %q; contract is canonical keys: %v", alias, keys(got))
		}
	}
}

func keys(m map[string]Snapshot) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestStatusUnknownAndDeleted(t *testing.T) {
	db := testStore(t, item("gone"))
	svc := New(db, testProfile(), nil)

	snap, err := svc.Status(context.Background(), "missing")
	if err != nil {
		t.Fatalf("Status unknown: %v", err)
	}
	if snap.Status != StatusIdle || snap.ItemID != "missing" {
		t.Fatalf("unknown snapshot = %+v, want idle under the supplied id", snap)
	}
	if _, err := svc.Start(context.Background(), "missing", false, Options{}); !errors.Is(err, store.ErrItemNotFound) {
		t.Fatalf("Start unknown err = %v, want ErrItemNotFound", err)
	}

	if _, err := db.PruneItems(context.Background(), store.PruneFilter{All: true}); err != nil {
		t.Fatalf("PruneItems: %v", err)
	}
	for _, id := range []string{"gone", item("gone").Link} {
		snap, err := svc.Status(context.Background(), id)
		if err != nil {
			t.Fatalf("Status deleted %q: %v", id, err)
		}
		if snap.Status != StatusIdle || snap.ItemID != id {
			t.Fatalf("deleted %q snapshot = %+v, want idle under the supplied id", id, snap)
		}
	}
}

func TestRefreshAndFailureByAlias(t *testing.T) {
	link := item("a").Link

	t.Run("refresh", func(t *testing.T) {
		db := testStore(t, item("a"))
		svc := New(db, testProfile(), readyGenerator())
		svc.SetFetcher(stubFetcher(validBody(), nil))
		if _, err := svc.Start(context.Background(), link, false, Options{MaxTextBytes: 900}); err != nil {
			t.Fatalf("initial Start by alias: %v", err)
		}
		if snap := waitTerminal(t, svc, link); snap.Status != StatusReady || snap.ItemID != "a" {
			t.Fatalf("initial snapshot = %+v, want ready canonical a", snap)
		}
		if _, err := svc.Start(context.Background(), link, true, Options{MaxTextBytes: 900}); err != nil {
			t.Fatalf("refresh Start by alias: %v", err)
		}
		if snap := waitTerminal(t, svc, link); snap.Status != StatusReady || snap.ItemID != "a" {
			t.Fatalf("refresh snapshot = %+v, want ready canonical a", snap)
		}
	})

	t.Run("failure", func(t *testing.T) {
		db := testStore(t, item("a"))
		svc := New(db, testProfile(), readyGenerator())
		svc.SetFetcher(stubFetcher("", articlefetch.ErrBlocked))
		if _, err := svc.Start(context.Background(), link, false, Options{}); err != nil {
			t.Fatalf("Start by alias: %v", err)
		}
		snap := waitTerminal(t, svc, link)
		if snap.Status != StatusFailed || snap.ErrorCode != CodeFetchBlocked || snap.ItemID != "a" {
			t.Fatalf("failure snapshot = %+v, want failed canonical a", snap)
		}
	})
}

func TestConcurrentAliasStatus(t *testing.T) {
	db := testStore(t, item("a"))
	svc, started, release := blockingBriefService(t, db)
	if _, err := svc.Start(context.Background(), item("a").Link, false, Options{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	<-started

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_, _ = svc.Status(context.Background(), item("a").Link)
					_, _ = svc.Status(context.Background(), "a")
					_, _ = svc.Statuses(context.Background(), []string{item("a").Link, "a"})
				}
			}
		}()
	}
	time.Sleep(10 * time.Millisecond)
	close(release)
	waitTerminal(t, svc, "a")
	close(stop)
	wg.Wait()
}
