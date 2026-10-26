// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/profile"
	"github.com/DanielBlei/rabbithole/internal/rank"
	"github.com/DanielBlei/rabbithole/internal/store"
)

func openFeedbackStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func seedRatedItems(t *testing.T, db *store.Store, items []feeds.Item, ratings map[string]int) {
	t.Helper()
	ctx := context.Background()
	if err := db.Record(ctx, items, nil, time.Now()); err != nil {
		t.Fatalf("Record: %v", err)
	}
	for id, rating := range ratings {
		value := rating
		if err := db.UpdateUserState(ctx, id, store.UserPatch{UserScore: &value}); err != nil {
			t.Fatalf("rate %s: %v", id, err)
		}
	}
}

func feedbackItems() []feeds.Item {
	return []feeds.Item{
		{
			ID: "p1", Source: "Systems", Title: "KV-cache benchmark",
			Summary: "benchmark methodology", Link: "https://x.test/p1",
		},
		{
			ID: "p2", Source: "Lab", Title: "Inference postmortem",
			Summary: "production incident", Link: "https://x.test/p2",
		},
		{
			ID: "p3", Source: "Notes", Title: "Memory management",
			Summary: "context handling", Link: "https://x.test/p3",
		},
		{ID: "n1", Source: "News", Title: "Funding round", Summary: "Series A", Link: "https://x.test/n1"},
		{ID: "n2", Source: "News", Title: "Product launch", Summary: "new release", Link: "https://x.test/n2"},
		{
			ID: "n3", Source: "Opinion", Title: "Why AI will change everything",
			Summary: "no evidence", Link: "https://x.test/n3",
		},
	}
}

func TestLoadScoringContextIncludesExplicitRatings(t *testing.T) {
	db := openFeedbackStore(t)
	items := feedbackItems()
	seedRatedItems(t, db, items, map[string]int{
		"p1": 10, "p2": 10, "p3": 10,
		"n1": 0, "n2": 0, "n3": 0,
	})
	note := "especially useful benchmark"
	if err := db.UpdateUserState(t.Context(), "p1", store.UserPatch{UserNote: &note}); err != nil {
		t.Fatalf("note p1: %v", err)
	}
	activeProfile := testProfile("top tier: local inference")

	scoringContext, err := loadScoringContext(t.Context(), db, activeProfile, nil)
	if err != nil {
		t.Fatalf("loadScoringContext: %v", err)
	}
	if scoringContext.Profile != activeProfile.Content {
		t.Errorf("profile = %q, want %q", scoringContext.Profile, activeProfile.Content)
	}
	if !scoringContext.Feedback.Active() {
		t.Fatalf("feedback = %+v, want an active selection", scoringContext.Feedback)
	}
	if len(scoringContext.Feedback.Positive) != 3 || len(scoringContext.Feedback.Negative) != 3 {
		t.Errorf("feedback counts = %d positive, %d negative; want 3 each",
			len(scoringContext.Feedback.Positive), len(scoringContext.Feedback.Negative))
	}
	byID := map[string]string{}
	for _, example := range append(scoringContext.Feedback.Positive, scoringContext.Feedback.Negative...) {
		byID[example.ItemID] = example.Title
		if example.ItemID == "p1" && (example.UserNote == nil || *example.UserNote != note) {
			t.Errorf("p1 note = %v, want %q", example.UserNote, note)
		}
	}
	for _, id := range []string{"p1", "p2", "p3", "n1", "n2", "n3"} {
		if byID[id] == "" {
			t.Errorf("explicit rating %s missing from feedback selection", id)
		}
	}
}

func TestLoadScoringContextIgnoresModelScores(t *testing.T) {
	db := openFeedbackStore(t)
	items := feedbackItems()
	entries := make([]store.DigestEntry, 0, len(items))
	for i, item := range items {
		entries = append(entries, store.DigestEntry{Item: item, Score: 10 - i, Reason: "model score"})
	}
	if err := db.Record(t.Context(), items, entries, time.Now()); err != nil {
		t.Fatalf("Record: %v", err)
	}

	scoringContext, err := loadScoringContext(t.Context(), db, testProfile("profile"), nil)
	if err != nil {
		t.Fatalf("loadScoringContext: %v", err)
	}
	if scoringContext.Feedback.Active() ||
		len(scoringContext.Feedback.Positive) != 0 || len(scoringContext.Feedback.Negative) != 0 {
		t.Fatalf("model scores leaked into explicit feedback: %+v", scoringContext.Feedback)
	}
}

func TestLoadScoringContextExcludesCandidates(t *testing.T) {
	db := openFeedbackStore(t)
	items := feedbackItems()
	seedRatedItems(t, db, items, map[string]int{
		"p1": 10, "p2": 10, "p3": 10,
		"n1": 0, "n2": 0, "n3": 0,
	})
	candidates := []feeds.Item{items[0], items[1], items[3]}
	excluded := candidateKeys(candidates)

	scoringContext, err := loadScoringContext(t.Context(), db, testProfile("profile"), candidates)
	if err != nil {
		t.Fatalf("loadScoringContext: %v", err)
	}
	if !scoringContext.Feedback.Active() {
		t.Fatalf("feedback = %+v, want active after excluding only part of it", scoringContext.Feedback)
	}
	got := map[string]bool{}
	for _, example := range append(scoringContext.Feedback.Positive, scoringContext.Feedback.Negative...) {
		if _, skip := excluded[example.ItemID]; skip {
			t.Errorf("candidate %s remained in feedback", example.ItemID)
		}
		got[example.ItemID] = true
	}
	for _, id := range []string{"p3", "n2", "n3"} {
		if !got[id] {
			t.Errorf("non-candidate feedback %s missing from selection", id)
		}
	}
}

func TestLoadScoringContextDeactivatesAfterCandidateExclusion(t *testing.T) {
	db := openFeedbackStore(t)
	items := feedbackItems()
	seedRatedItems(t, db, items, map[string]int{"p1": 10, "p2": 10, "p3": 10})
	candidates := []feeds.Item{items[0], items[1]}

	scoringContext, err := loadScoringContext(t.Context(), db, testProfile("profile"), candidates)
	if err != nil {
		t.Fatalf("loadScoringContext: %v", err)
	}
	if scoringContext.Feedback.Active() {
		t.Errorf("feedback = %+v, want inactive: only one one-sided rating remains",
			scoringContext.Feedback)
	}
	if len(scoringContext.Feedback.Positive) != 1 ||
		scoringContext.Feedback.Positive[0].ItemID != "p3" {
		t.Errorf("feedback = %+v, want only the non-candidate p3", scoringContext.Feedback)
	}
}

func datedPositiveItems(n int) []feeds.Item {
	items := make([]feeds.Item, 0, n)
	for i := 1; i <= n; i++ {
		items = append(items, feeds.Item{
			ID:        "hist-" + strconv.Itoa(i),
			Source:    "History",
			Title:     "Rated history " + strconv.Itoa(i),
			Link:      "https://x.test/history/" + strconv.Itoa(i),
			Published: time.Date(2026, time.October, i, 12, 0, 0, 0, time.UTC),
		})
	}
	return items
}

func TestLoadScoringContextBackfillsOlderFeedback(t *testing.T) {
	db := openFeedbackStore(t)
	items := datedPositiveItems(8)
	seedRatedItems(t, db, items, map[string]int{
		"hist-1": 10, "hist-2": 10, "hist-3": 10, "hist-4": 10,
		"hist-5": 10, "hist-6": 10, "hist-7": 10, "hist-8": 10,
	})
	candidates := items[len(items)-5:] // the five newest ratings are all being scored

	scoringContext, err := loadScoringContext(t.Context(), db, testProfile("profile"), candidates)
	if err != nil {
		t.Fatalf("loadScoringContext: %v", err)
	}
	if !scoringContext.Feedback.Active() {
		t.Fatalf("feedback = %+v, want the three older ratings to backfill the selection",
			scoringContext.Feedback)
	}
	var got []string
	for _, example := range scoringContext.Feedback.Positive {
		got = append(got, example.ItemID)
	}
	want := []string{"hist-3", "hist-2", "hist-1"}
	if !slices.Equal(got, want) {
		t.Errorf("positive feedback = %v, want older examples %v", got, want)
	}
}

// newScoringServer stands in for vLLM and records every scoring prompt. Its
// probe reply is plain text; scoring replies carry the structured-output shape.
func newScoringServer(t *testing.T) (string, func() []string) {
	t.Helper()
	var (
		mu      sync.Mutex
		prompts []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case http.MethodGet + " /v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"test-model"}]}`))
		case http.MethodPost + " /v1/chat/completions":
			var req struct {
				Messages []struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"messages"`
				ResponseFormat json.RawMessage `json:"response_format"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if len(req.ResponseFormat) == 0 {
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
				return
			}
			mu.Lock()
			for _, message := range req.Messages {
				if message.Role == "user" {
					prompts = append(prompts, message.Content)
				}
			}
			mu.Unlock()
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":` +
				`"{\"scores\":[{\"index\":1,\"score\":7,\"reason\":\"Relevant.\"}]}"},` +
				`"finish_reason":"stop"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), prompts...)
	}
}

func feedItemID(link string) string {
	sum := sha256.Sum256([]byte(link))
	return hex.EncodeToString(sum[:])
}

func TestRunExcludesRatedCandidateFromFeedback(t *testing.T) {
	host, prompts := newScoringServer(t)
	link := "https://x.test/rated-candidate"
	cfg := testConfig(t, config.Feed{Name: "Feed", URL: serveRSS(t, feedRSS("Rated candidate", link))})
	cfg.Inference.Provider = "vllm"
	cfg.Inference.Host = host
	cfg.Inference.Model = "test-model"
	think := false
	cfg.Inference.Think = &think
	db := openStore(t, cfg)

	candidate := feeds.Item{
		ID: feedItemID(link), Source: "Feed", Title: "Rated candidate", Link: link,
	}
	extras := []feeds.Item{
		{ID: "extra-p1", Source: "Blog", Title: "Extra one", Link: "https://x.test/extra-p1"},
		{ID: "extra-p2", Source: "Blog", Title: "Extra two", Link: "https://x.test/extra-p2"},
	}
	if err := db.Record(t.Context(), append([]feeds.Item{candidate}, extras...), nil, time.Now()); err != nil {
		t.Fatalf("Record: %v", err)
	}
	for _, id := range []string{candidate.ID, "extra-p1", "extra-p2"} {
		rating := 10
		if err := db.UpdateUserState(t.Context(), id, store.UserPatch{UserScore: &rating}); err != nil {
			t.Fatalf("rate %s: %v", id, err)
		}
	}

	out, err := Run(t.Context(), cfg.Config, testProfile("profile"), db, time.Now(), Options{Record: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Scored != 1 {
		t.Fatalf("Scored = %d, want the rated candidate to be ingested", out.Scored)
	}
	got := prompts()
	if len(got) != 1 {
		t.Fatalf("scoring prompts = %d, want 1", len(got))
	}
	if strings.Contains(got[0], "READER FEEDBACK") {
		t.Errorf("candidate appeared as feedback for its own score:\n%s", got[0])
	}
	if !strings.Contains(got[0], "ARTICLES (untrusted") {
		t.Errorf("scoring prompt lost its article section:\n%s", got[0])
	}
}

func TestRunSendsActiveFeedbackToScorer(t *testing.T) {
	host, prompts := newScoringServer(t)
	link := "https://x.test/new-article"
	cfg := testConfig(t, config.Feed{Name: "Feed", URL: serveRSS(t, feedRSS("New article", link))})
	cfg.Inference.Provider = "vllm"
	cfg.Inference.Host = host
	cfg.Inference.Model = "test-model"
	think := false
	cfg.Inference.Think = &think
	db := openStore(t, cfg)

	rated := []feeds.Item{
		{ID: "rated-p1", Source: "Blog", Title: "Rated one", Link: "https://x.test/rated-1"},
		{ID: "rated-p2", Source: "Blog", Title: "Rated two", Link: "https://x.test/rated-2"},
		{ID: "rated-p3", Source: "Blog", Title: "Rated three", Link: "https://x.test/rated-3"},
	}
	if err := db.Record(t.Context(), rated, nil, time.Now()); err != nil {
		t.Fatalf("Record: %v", err)
	}
	for _, item := range rated {
		rating := 10
		if err := db.UpdateUserState(t.Context(), item.ID, store.UserPatch{UserScore: &rating}); err != nil {
			t.Fatalf("rate %s: %v", item.ID, err)
		}
	}

	_, err := Run(
		t.Context(), cfg.Config, testProfile("profile"), db, time.Now(), Options{Record: true},
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := prompts()
	if len(got) != 1 {
		t.Fatalf("scoring prompts = %d, want 1", len(got))
	}
	for _, want := range []string{
		"READER FEEDBACK",
		"P1. [Blog] Rated one — rated 10/10",
		"Only the numbered entries under ARTICLES are scoring targets",
		"ARTICLES (untrusted",
	} {
		if !strings.Contains(got[0], want) {
			t.Errorf("scoring prompt missing %q:\n%s", want, got[0])
		}
	}
}

type capturingContextScorer struct {
	mu       sync.Mutex
	contexts []rank.ScoringContext
}

func (s *capturingContextScorer) Validate(context.Context) error { return nil }

func (s *capturingContextScorer) Score(
	ctx context.Context,
	profile string,
	items []feeds.Item,
) ([]rank.ItemScore, error) {
	return s.ScoreContext(ctx, rank.ProfileContext(profile), items)
}

func (s *capturingContextScorer) ScoreContext(
	_ context.Context,
	scoringContext rank.ScoringContext,
	items []feeds.Item,
) ([]rank.ItemScore, error) {
	s.mu.Lock()
	s.contexts = append(s.contexts, scoringContext)
	s.mu.Unlock()
	scores := make([]rank.ItemScore, 0, len(items))
	for _, item := range items {
		scores = append(scores, rank.ItemScore{ID: item.ID, Score: 6, Reason: "rescored"})
	}
	return scores, nil
}

func (s *capturingContextScorer) captured() []rank.ScoringContext {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]rank.ScoringContext(nil), s.contexts...)
}

func candidateItems() []feeds.Item {
	return []feeds.Item{
		{ID: "c1", Source: "Blog", Title: "Candidate one", Link: "https://x.test/c1"},
		{ID: "c2", Source: "Blog", Title: "Candidate two", Link: "https://x.test/c2"},
		{ID: "c3", Source: "Blog", Title: "Candidate three", Link: "https://x.test/c3"},
		{ID: "c4", Source: "Blog", Title: "Candidate four", Link: "https://x.test/c4"},
		{ID: "c5", Source: "Blog", Title: "Candidate five", Link: "https://x.test/c5"},
		{ID: "c6", Source: "Blog", Title: "Candidate six", Link: "https://x.test/c6"},
	}
}

func rescoreCapture(
	t *testing.T,
	rated []feeds.Item,
	ratings map[string]int,
	candidates []feeds.Item,
) []rank.ScoringContext {
	t.Helper()
	db := openFeedbackStore(t)
	if len(rated) > 0 {
		seedRatedItems(t, db, rated, ratings)
	}
	if err := db.Record(t.Context(), candidates, nil, time.Now()); err != nil {
		t.Fatalf("Record candidates: %v", err)
	}

	think := false
	cfg := &config.Config{}
	cfg.Inference.Think = &think
	cfg.Inference.BatchSize = 3
	cfg.Inference.MaxParallel = 1
	snapshot := profile.NewSnapshot("p", "Profile", "top tier: local inference", false)
	scorer := &capturingContextScorer{}
	if _, err := rescoreItems(t.Context(), *cfg, snapshot, db, candidates, scorer); err != nil {
		t.Fatalf("rescoreItems: %v", err)
	}
	return scorer.captured()
}

func TestRescorePassesExplicitFeedbackToScorer(t *testing.T) {
	ratings := map[string]int{"p1": 10, "p2": 10, "p3": 10, "n1": 0, "n2": 0, "n3": 0}
	contexts := rescoreCapture(t, feedbackItems(), ratings, candidateItems())
	if len(contexts) != 2 {
		t.Fatalf("scorer contexts = %d, want 2 batches", len(contexts))
	}
	for _, scoringContext := range contexts {
		if scoringContext.Profile != "top tier: local inference" {
			t.Errorf("profile = %q", scoringContext.Profile)
		}
		if !scoringContext.Feedback.Active() {
			t.Errorf("feedback is inactive: %+v", scoringContext.Feedback)
		}
		if len(scoringContext.Feedback.Positive) != 3 || len(scoringContext.Feedback.Negative) != 3 {
			t.Errorf("feedback counts = %d positive, %d negative; want 3 each",
				len(scoringContext.Feedback.Positive), len(scoringContext.Feedback.Negative))
		}
	}
}

func TestRescoreWithoutFeedbackKeepsProfileOnlyContext(t *testing.T) {
	contexts := rescoreCapture(t, feedbackItems(), nil, candidateItems())
	if len(contexts) == 0 {
		t.Fatal("scorer received no context")
	}
	for _, scoringContext := range contexts {
		if scoringContext.Profile != "top tier: local inference" {
			t.Errorf("profile = %q", scoringContext.Profile)
		}
		if scoringContext.Feedback.Active() {
			t.Errorf("feedback is active without ratings: %+v", scoringContext.Feedback)
		}
		got := rank.BuildUserPromptContext(scoringContext, feedbackItems())
		want := rank.BuildUserPrompt(scoringContext.Profile, feedbackItems())
		if got != want {
			t.Errorf("prompt changed without feedback:\ngot:\n%s\nwant:\n%s", got, want)
		}
	}
}

func TestRescoreExcludesEveryCandidateFromFeedback(t *testing.T) {
	ratings := map[string]int{"p1": 10, "p2": 10, "p3": 10, "n1": 0, "n2": 0, "n3": 0}
	contexts := rescoreCapture(t, feedbackItems(), ratings, feedbackItems())
	if len(contexts) != 2 {
		t.Fatalf("scorer contexts = %d, want 2 batches", len(contexts))
	}
	for _, scoringContext := range contexts {
		if scoringContext.Feedback.Active() {
			t.Errorf("feedback used the rescore candidates as examples: %+v", scoringContext.Feedback)
		}
	}
}

func TestRescoreKeepsFeedbackThatIsNotACandidate(t *testing.T) {
	items := feedbackItems()
	ratings := map[string]int{"p1": 10, "p2": 10, "p3": 10, "n1": 0, "n2": 0, "n3": 0}
	candidates := []feeds.Item{items[0], items[3]} // p1 and n1 are being rescored
	contexts := rescoreCapture(t, items, ratings, candidates)
	if len(contexts) != 1 {
		t.Fatalf("scorer contexts = %d, want 1 batch", len(contexts))
	}
	selection := contexts[0].Feedback
	if !selection.Active() {
		t.Fatalf("feedback = %+v, want active from the remaining ratings", selection)
	}
	excluded := candidateKeys(candidates)
	for _, example := range append(selection.Positive, selection.Negative...) {
		if _, skip := excluded[example.ItemID]; skip {
			t.Errorf("candidate %s remained in feedback", example.ItemID)
		}
	}
	if len(selection.Positive) != 2 || len(selection.Negative) != 2 {
		t.Errorf("feedback counts = %d positive, %d negative; want 2 each",
			len(selection.Positive), len(selection.Negative))
	}
}

func TestRescoreOneSidedFeedbackBelowThresholdAfterExclusion(t *testing.T) {
	items := feedbackItems()
	ratings := map[string]int{"p1": 10, "p2": 10, "p3": 10}
	candidates := []feeds.Item{items[0], items[1]} // two of the three positives are rescored
	contexts := rescoreCapture(t, items, ratings, candidates)
	if len(contexts) != 1 {
		t.Fatalf("scorer contexts = %d, want 1 batch", len(contexts))
	}
	if contexts[0].Feedback.Active() {
		t.Errorf("feedback = %+v, want inactive with one one-sided rating left",
			contexts[0].Feedback)
	}
}

func TestRescoreBackfillsOlderFeedbackWhenNewestAreCandidates(t *testing.T) {
	rated := datedPositiveItems(8)
	ratings := map[string]int{
		"hist-1": 10, "hist-2": 10, "hist-3": 10, "hist-4": 10,
		"hist-5": 10, "hist-6": 10, "hist-7": 10, "hist-8": 10,
	}
	candidates := rated[len(rated)-5:] // the five newest ratings are all being rescored

	contexts := rescoreCapture(t, rated, ratings, candidates)
	if len(contexts) != 2 {
		t.Fatalf("scorer contexts = %d, want 2 batches", len(contexts))
	}
	for _, scoringContext := range contexts {
		if !scoringContext.Feedback.Active() {
			t.Errorf("feedback = %+v, want older ratings to backfill", scoringContext.Feedback)
		}
		var got []string
		for _, example := range scoringContext.Feedback.Positive {
			got = append(got, example.ItemID)
		}
		if want := []string{"hist-3", "hist-2", "hist-1"}; !slices.Equal(got, want) {
			t.Errorf("positive feedback = %v, want older examples %v", got, want)
		}
	}
}
