// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/profile"
	"github.com/DanielBlei/rabbithole/internal/rank"
	"github.com/DanielBlei/rabbithole/internal/store"
)

type rescoreTestScorer struct {
	mu       sync.Mutex
	profiles []string
	failID   string
	onScore  func()
}

func (s *rescoreTestScorer) Validate(context.Context) error { return nil }

func (s *rescoreTestScorer) Score(
	_ context.Context,
	profileText string,
	items []feeds.Item,
) ([]rank.ItemScore, error) {
	s.mu.Lock()
	s.profiles = append(s.profiles, profileText)
	onScore := s.onScore
	s.onScore = nil
	s.mu.Unlock()
	if onScore != nil {
		onScore()
	}
	var scores []rank.ItemScore
	for _, item := range items {
		if item.ID == s.failID {
			continue
		}
		scores = append(scores, rank.ItemScore{ID: item.ID, Score: 9, Reason: "rescored"})
	}
	return scores, nil
}

func TestRescoreItemsReplacesSuccessAndPreservesFailure(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	items := []feeds.Item{
		{ID: "ok", Source: "S", Title: "OK", Link: "https://x/ok"},
		{ID: "fail", Source: "S", Title: "Fail", Link: "https://x/fail"},
	}
	if err := db.Record(ctx, items, []store.DigestEntry{
		{Item: items[0], Score: 2, Reason: "old ok", Model: "old"},
		{Item: items[1], Score: 3, Reason: "old fail", Model: "old"},
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	rating := 10
	if err := db.UpdateUserState(ctx, "ok", store.UserPatch{UserScore: &rating}); err != nil {
		t.Fatal(err)
	}

	think := false
	cfg := &config.Config{}
	cfg.Inference.Think = &think
	cfg.Inference.Model = "new-model"
	cfg.Inference.BatchSize = 2
	cfg.Inference.MaxParallel = 1
	snapshot := profile.NewSnapshot("new-profile", "New", "# New", false)
	outcome, err := rescoreItems(
		ctx, cfg, snapshot, db, items, &rescoreTestScorer{failID: "fail"},
	)
	if err != nil {
		t.Fatalf("rescoreItems: %v", err)
	}
	if outcome != (RescoreOutcome{Candidates: 2, Scored: 1, Failed: 1}) {
		t.Fatalf("outcome = %+v", outcome)
	}
	ok, err := db.Get(ctx, "ok")
	if err != nil {
		t.Fatal(err)
	}
	if ok.LLMScore == nil || *ok.LLMScore != 9 ||
		ok.LLMProfileHash == nil || *ok.LLMProfileHash != snapshot.Hash ||
		ok.UserScore == nil || *ok.UserScore != rating {
		t.Fatalf("successful replacement = %+v", ok)
	}
	failed, err := db.Get(ctx, "fail")
	if err != nil {
		t.Fatal(err)
	}
	if failed.LLMScore == nil || *failed.LLMScore != 3 ||
		failed.LLMScoreReason == nil || *failed.LLMScoreReason != "old fail" {
		t.Fatalf("failed item lost old score: %+v", failed)
	}
}

func TestRescoreUsesOneProfileSnapshot(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	items := []feeds.Item{
		{ID: "a", Source: "S", Title: "A", Link: "https://x/a"},
		{ID: "b", Source: "S", Title: "B", Link: "https://x/b"},
	}
	if err := db.Record(ctx, items, []store.DigestEntry{
		{Item: items[0], Score: 2}, {Item: items[1], Score: 3},
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	local, err := db.CreateProfile(ctx, "First", "# First")
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.CreateProfile(ctx, "Second", "# Second")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetActiveProfile(ctx, local.ID); err != nil {
		t.Fatal(err)
	}
	snapshot := profile.NewSnapshot(local.ID, local.Name, local.Content, false)
	scorer := &rescoreTestScorer{onScore: func() {
		if _, err := db.UpdateProfile(ctx, local.ID, "Edited", "# Edited"); err != nil {
			t.Error(err)
		}
		if err := db.SetActiveProfile(ctx, other.ID); err != nil {
			t.Error(err)
		}
	}}
	think := false
	cfg := &config.Config{}
	cfg.Inference.Think = &think
	cfg.Inference.Model = "m"
	cfg.Inference.BatchSize = 1
	cfg.Inference.MaxParallel = 1

	if _, err := rescoreItems(ctx, cfg, snapshot, db, items, scorer); err != nil {
		t.Fatal(err)
	}
	scorer.mu.Lock()
	defer scorer.mu.Unlock()
	if len(scorer.profiles) != 2 {
		t.Fatalf("profile calls = %q", scorer.profiles)
	}
	for _, got := range scorer.profiles {
		if got != snapshot.Content {
			t.Fatalf("mixed profile snapshot: got %q, want %q", got, snapshot.Content)
		}
	}
	for _, item := range items {
		row, err := db.Get(ctx, item.ID)
		if err != nil {
			t.Fatal(err)
		}
		if row.LLMProfileHash == nil || *row.LLMProfileHash != snapshot.Hash {
			t.Fatalf("item %s provenance = %+v", item.ID, row)
		}
	}
}

func TestRescoreBackendInitFailureLeavesScoresUntouched(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	item := feeds.Item{ID: "a", Source: "S", Title: "A", Link: "https://x/a"}
	if err := db.Record(ctx, []feeds.Item{item}, []store.DigestEntry{{
		Item: item, Score: 4, Reason: "old",
	}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	think := false
	cfg := &config.Config{}
	cfg.Inference.Think = &think
	cfg.Inference.Provider = "not-a-provider"
	_, err = RescoreRecent(
		ctx, cfg, profile.Default(), db, time.Now(), ProfileRescoreWindow,
	)
	if err == nil {
		t.Fatal("RescoreRecent succeeded with invalid provider")
	}
	row, getErr := db.Get(ctx, item.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if row.LLMScore == nil || *row.LLMScore != 4 ||
		row.LLMScoreReason == nil || *row.LLMScoreReason != "old" {
		t.Fatalf("backend init failure changed score: %+v", row)
	}
}

func TestSelectingProfileAloneDoesNotChangeStoredScore(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	item := feeds.Item{ID: "a", Source: "S", Title: "A", Link: "https://x/a"}
	if err := db.Record(ctx, []feeds.Item{item}, []store.DigestEntry{{
		Item: item, Score: 4, Reason: "historical", Model: "old",
		ProfileID: "old-profile", ProfileName: "Old", ProfileHash: "old-hash",
	}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	next, err := db.CreateProfile(ctx, "Next", "# Next")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetActiveProfile(ctx, next.ID); err != nil {
		t.Fatal(err)
	}
	row, err := db.Get(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.LLMScore == nil || *row.LLMScore != 4 ||
		row.LLMProfileID == nil || *row.LLMProfileID != "old-profile" {
		t.Fatalf("profile selection rewrote score: %+v", row)
	}
}

func TestRescoreRejectsInvalidWindow(t *testing.T) {
	_, err := RescoreRecent(
		context.Background(), &config.Config{}, profile.Default(), nil, time.Now(), 0,
	)
	if err == nil {
		t.Fatalf("invalid window error = %v", err)
	}
}
