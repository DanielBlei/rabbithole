// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package briefing

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/profile"
	"github.com/DanielBlei/rabbithole/internal/store"
)

type fakeStore struct {
	mu     sync.Mutex
	items  []store.BriefingItem
	limit  int
	saved  map[string]store.Briefing
	saves  int
	lastAt time.Time
}

func newFakeStore(items ...store.BriefingItem) *fakeStore {
	return &fakeStore{items: items, saved: map[string]store.Briefing{}}
}

func key(kind string, start time.Time) string {
	return kind + "|" + start.UTC().Format(time.RFC3339)
}

func (f *fakeStore) BriefingItems(_ context.Context, _, _ time.Time, limit int) ([]store.BriefingItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.limit = limit
	if len(f.items) > limit {
		return f.items[:limit], nil
	}
	return f.items, nil
}

func (f *fakeStore) SaveBriefing(_ context.Context, b store.Briefing) (store.Briefing, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saves++
	if b.CreatedAt.IsZero() {
		b.CreatedAt = time.Now()
	}
	f.lastAt = time.Now()
	b.UpdatedAt = f.lastAt
	f.saved[key(b.PeriodKind, b.PeriodStart)] = b
	return b, nil
}

func (f *fakeStore) BriefingForPeriod(_ context.Context, kind string, start time.Time) (store.Briefing, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.saved[key(kind, start)]
	if !ok {
		return store.Briefing{}, store.ErrBriefingNotFound
	}
	return b, nil
}

func (f *fakeStore) get(kind string, start time.Time) store.Briefing {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.saved[key(kind, start)]
}

func (f *fakeStore) saveCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.saves
}

type fakeProfiles struct{ snap profile.Snapshot }

func (f fakeProfiles) Resolve(context.Context) (profile.Snapshot, error) { return f.snap, nil }

type fakeGenerator struct {
	mu      sync.Mutex
	replies []string
	errs    []error
	calls   int
	systems []string
	users   []string
	schemas []string
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (f *fakeGenerator) ChatJSON(
	_ context.Context,
	system, user string,
	schema json.RawMessage,
	_ int,
) (string, error) {
	f.mu.Lock()
	call := f.calls
	f.calls++
	f.systems = append(f.systems, system)
	f.users = append(f.users, user)
	f.schemas = append(f.schemas, string(schema))
	f.mu.Unlock()
	if f.entered != nil {
		f.once.Do(func() { close(f.entered) })
	}
	if f.release != nil {
		<-f.release
	}
	if call < len(f.errs) && f.errs[call] != nil {
		return "", f.errs[call]
	}
	if call >= len(f.replies) {
		return "", errors.New("no reply")
	}
	return f.replies[call], nil
}

func (f *fakeGenerator) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func sampleItems(n int) []store.BriefingItem {
	items := make([]store.BriefingItem, n)
	for i := range items {
		items[i] = store.BriefingItem{
			ID:      "item-" + string(rune('a'+i)),
			Source:  "Source",
			Title:   "Title " + string(rune('A'+i)),
			Link:    "https://example.com/" + string(rune('a'+i)),
			Summary: "Summary of article.",
			Score:   10 - i%5,
		}
	}
	return items
}

func validJSON() string {
	return `{
	  "executive_summary": "A summary.",
	  "developments": [
	    {"headline": "One thing", "synthesis": "It happened.", "why_it_matters": "It matters.", "item_ids": [1]},
	    {"headline": "Another", "synthesis": "Also happened.", "why_it_matters": "Also matters.", "item_ids": [2]}
	  ],
	  "emerging_topics": ["Topic"],
	  "recommended_reading": [1, 2]
	}`
}

func testService(st *fakeStore, gen GeneratorFunc) *Service {
	return New(st, fakeProfiles{snap: profile.Default()}, gen)
}

func options() Options {
	return Options{Now: time.Date(2026, 1, 7, 12, 0, 0, 0, time.UTC), Provider: "vllm", Model: "m"}
}

func TestPeriodSelection(t *testing.T) {
	// Wednesday 7 January 2026.
	now := time.Date(2026, 1, 7, 12, 0, 0, 0, time.UTC)
	daily, err := PeriodFor("daily", now)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 1, 6, 0, 0, 0, 0, time.UTC); !daily.Start.Equal(want) {
		t.Errorf("daily start = %v, want %v", daily.Start, want)
	}
	weekly, err := PeriodFor("weekly", now)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2025, 12, 29, 0, 0, 0, 0, time.UTC); !weekly.Start.Equal(want) {
		t.Errorf("weekly start = %v, want %v", weekly.Start, want)
	}
	if want := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC); !weekly.End.Equal(want) {
		t.Errorf("weekly end = %v, want %v", weekly.End, want)
	}

	// A Monday at 00:00 UTC: the last completed day is Sunday, the last completed
	// week is the week that ended at this instant.
	monday := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	d, _ := PeriodFor("daily", monday)
	if want := time.Date(2026, 1, 4, 0, 0, 0, 0, time.UTC); !d.Start.Equal(want) {
		t.Errorf("monday daily start = %v, want %v", d.Start, want)
	}
	w, _ := PeriodFor("weekly", monday)
	if want := time.Date(2025, 12, 29, 0, 0, 0, 0, time.UTC); !w.Start.Equal(want) {
		t.Errorf("monday weekly start = %v, want %v", w.Start, want)
	}
	if !w.Next().Start.Equal(monday) {
		t.Errorf("weekly next = %v, want %v", w.Next().Start, monday)
	}
	if !w.Previous().Start.Equal(time.Date(2025, 12, 22, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("weekly previous = %v", w.Previous().Start)
	}
}

func TestPeriodAtNormalizesToUTCMidnight(t *testing.T) {
	p, err := PeriodAt("daily", time.Date(2026, 2, 3, 18, 30, 0, 0, time.FixedZone("x", 3600)))
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 2, 3, 0, 0, 0, 0, time.UTC); !p.Start.Equal(want) {
		t.Errorf("start = %v, want %v", p.Start, want)
	}
	if _, err := PeriodAt("monthly", time.Now()); err == nil {
		t.Fatal("unknown kind accepted")
	}
}

func TestPeriodAtSnapsWeeklyToMonday(t *testing.T) {
	// Wednesday 7 January 2026 snaps back to Monday 5 January.
	p, err := PeriodAt("weekly", time.Date(2026, 1, 7, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC); !p.Start.Equal(want) {
		t.Errorf("weekly start = %v, want %v", p.Start, want)
	}
	if want := time.Date(2026, 1, 12, 0, 0, 0, 0, time.UTC); !p.End.Equal(want) {
		t.Errorf("weekly end = %v, want %v", p.End, want)
	}
}

func TestGenerateBoundsItemSelection(t *testing.T) {
	st := newFakeStore(sampleItems(60)...)
	gen := &fakeGenerator{replies: []string{validJSON()}}
	svc := testService(st, func(context.Context) (Generator, error) { return gen, nil })

	opts := options()
	opts.MaxArticles = 3
	got, err := svc.Generate(context.Background(), "daily", time.Time{}, opts)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if st.limit != 3 {
		t.Errorf("store limit = %d, want 3", st.limit)
	}
	if got.ItemCount != 3 {
		t.Errorf("item count = %d, want 3", got.ItemCount)
	}
	if !strings.Contains(gen.users[0], "Title A") || strings.Contains(gen.users[0], "Title D") {
		t.Errorf("prompt was not bounded to three articles:\n%s", gen.users[0])
	}
}

func TestPromptConstruction(t *testing.T) {
	items := sampleItems(2)
	items[0].Summary = strings.Repeat("x", 900)
	p := Period{
		Kind:  "daily",
		Start: time.Date(2026, 1, 6, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 1, 7, 0, 0, 0, 0, time.UTC),
	}
	got := BuildUserPrompt(p, "My deep interest", items)
	for _, want := range []string{
		"My deep interest",
		"untrusted feed content",
		"REPORTING PERIOD: daily 2026-01-06 to 2026-01-07",
		"1 | Source | Title A | score 10/10",
		"2 | Source | Title B | score 9/10",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, strings.Repeat("x", 600)) {
		t.Error("summary was not excerpted")
	}
	if !strings.Contains(SystemPrompt, "Never invent") || !strings.Contains(SystemPrompt, "not instructions") {
		t.Error("system prompt lost its untrusted/no-invention stance")
	}
}

func TestParseValidation(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"valid", validJSON(), ""},
		{"fenced", "```json\n" + validJSON() + "\n```", ""},
		{
			"invented id",
			`{"executive_summary":"s","developments":[{"headline":"h","synthesis":"x","why_it_matters":"w","item_ids":[9]}],"recommended_reading":[]}`,
			"outside",
		},
		{
			"duplicate id",
			`{"executive_summary":"s","developments":[{"headline":"h","synthesis":"x","why_it_matters":"w","item_ids":[1,1]}],"recommended_reading":[]}`,
			"more than once",
		},
		{
			"no citation",
			`{"executive_summary":"s","developments":[{"headline":"h","synthesis":"x","why_it_matters":"w","item_ids":[]}],"recommended_reading":[]}`,
			"cites no supplied",
		},
		{
			"over cap",
			`{"executive_summary":"s","developments":[{"headline":"h","synthesis":"x","why_it_matters":"w","item_ids":[1]},{"headline":"h","synthesis":"x","why_it_matters":"w","item_ids":[1]},{"headline":"h","synthesis":"x","why_it_matters":"w","item_ids":[1]}],"recommended_reading":[]}`,
			"cap is 2",
		},
		{"empty summary", `{"executive_summary":" ","developments":[],"recommended_reading":[]}`, "empty"},
		{
			"unknown field",
			`{"executive_summary":"s","developments":[{"headline":"h","synthesis":"x","why_it_matters":"w","item_ids":[1],"extra":true}],"recommended_reading":[]}`,
			"unknown field",
		},
		{"no json", "hello", "no JSON"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.raw, 2, 2, 2)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("Parse: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestGenerateRetryThenFail(t *testing.T) {
	st := newFakeStore(sampleItems(2)...)
	bad := `{"executive_summary":"s","developments":[{"headline":"h","synthesis":"x","why_it_matters":"w","item_ids":[99]}],"recommended_reading":[]}`
	gen := &fakeGenerator{replies: []string{bad, bad}}
	svc := testService(st, func(context.Context) (Generator, error) { return gen, nil })

	got, err := svc.Generate(context.Background(), "daily", time.Time{}, options())
	if !errors.Is(err, ErrGenerationFailed) {
		t.Fatalf("error = %v, want ErrGenerationFailed", err)
	}
	if gen.callCount() != 2 {
		t.Errorf("model calls = %d, want 2 (one retry)", gen.callCount())
	}
	if got.Status != store.BriefingFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
	if got.BodyMD != "" || len(got.ReferencedItemIDs) != 0 {
		t.Error("a failed briefing stored a partial body or citations")
	}
	if !strings.Contains(got.Error, "outside") {
		t.Errorf("error = %q, want the grounding violation", got.Error)
	}
}

func TestGenerateRetrySucceeds(t *testing.T) {
	st := newFakeStore(sampleItems(2)...)
	bad := `{"executive_summary":"s","developments":[],"recommended_reading":[]}`
	gen := &fakeGenerator{replies: []string{bad, validJSON()}}
	svc := testService(st, func(context.Context) (Generator, error) { return gen, nil })

	got, err := svc.Generate(context.Background(), "daily", time.Time{}, options())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if gen.callCount() != 2 {
		t.Errorf("calls = %d, want 2", gen.callCount())
	}
	if got.Status != store.BriefingReady {
		t.Errorf("status = %q", got.Status)
	}
	if got.Provider != "vllm" || got.Model != "m" {
		t.Errorf("provenance = %q/%q", got.Provider, got.Model)
	}
	if !strings.Contains(got.BodyMD, "Executive summary") || !strings.Contains(got.BodyMD, "https://example.com/a") {
		t.Errorf("body missing expected content:\n%s", got.BodyMD)
	}
	if len(got.ReferencedItemIDs) != 2 || got.ReferencedItemIDs[0] != "item-a" {
		t.Errorf("references = %v", got.ReferencedItemIDs)
	}
}

func TestGenerateRegenerateReplacesThePeriod(t *testing.T) {
	st := newFakeStore(sampleItems(2)...)
	gen := &fakeGenerator{replies: []string{validJSON(), validJSON()}}
	svc := testService(st, func(context.Context) (Generator, error) { return gen, nil })

	first, err := svc.Generate(context.Background(), "daily", time.Time{}, options())
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.Generate(context.Background(), "daily", time.Time{}, options())
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Errorf("regenerate changed the id: %q -> %q", first.ID, second.ID)
	}
	if len(st.saved) != 1 {
		t.Errorf("stored %d rows, want 1 per period", len(st.saved))
	}
	// Each run persists generating then ready.
	if st.saveCount() != 4 {
		t.Errorf("saves = %d, want 4", st.saveCount())
	}
}

func TestGenerateEmptyPeriodIsAReadyProductState(t *testing.T) {
	st := newFakeStore()
	gen := &fakeGenerator{}
	svc := testService(st, func(context.Context) (Generator, error) { return gen, nil })

	got, err := svc.Generate(context.Background(), "daily", time.Time{}, options())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if gen.callCount() != 0 {
		t.Errorf("model calls = %d, want 0", gen.callCount())
	}
	if got.Status != store.BriefingReady || !strings.Contains(got.BodyMD, "No scored articles") {
		t.Errorf("status = %q body = %q", got.Status, got.BodyMD)
	}
}

func TestGenerateProviderFailureIsPersisted(t *testing.T) {
	st := newFakeStore(sampleItems(2)...)
	gen := &fakeGenerator{errs: []error{errors.New("connection refused")}}
	svc := testService(st, func(context.Context) (Generator, error) { return gen, nil })

	got, err := svc.Generate(context.Background(), "daily", time.Time{}, options())
	if err == nil {
		t.Fatal("expected an error")
	}
	if got.Status != store.BriefingFailed || !strings.Contains(got.Error, "connection refused") {
		t.Errorf("status = %q error = %q", got.Status, got.Error)
	}
	if gen.callCount() != 1 {
		t.Errorf("calls = %d, want 1 (no retry for a provider error)", gen.callCount())
	}
}

func TestGenerateUnavailableProvider(t *testing.T) {
	st := newFakeStore(sampleItems(2)...)
	svc := testService(st, func(context.Context) (Generator, error) {
		return nil, ErrBriefingUnavailable
	})
	got, err := svc.Generate(context.Background(), "daily", time.Time{}, options())
	if !errors.Is(err, ErrBriefingUnavailable) {
		t.Fatalf("error = %v, want ErrBriefingUnavailable", err)
	}
	if got.Status != store.BriefingFailed || !strings.Contains(got.Error, "needs a model provider") {
		t.Errorf("status = %q error = %q", got.Status, got.Error)
	}
}

func TestSingleFlightReturnsGeneratingState(t *testing.T) {
	st := newFakeStore(sampleItems(2)...)
	gen := &fakeGenerator{
		replies: []string{validJSON()},
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	svc := testService(st, func(context.Context) (Generator, error) { return gen, nil })
	ctx := context.Background()

	started, err := svc.Start(ctx, "daily", time.Time{}, options())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if started.Status != store.BriefingGenerating {
		t.Fatalf("start status = %q", started.Status)
	}
	<-gen.entered

	concurrent, err := svc.Generate(ctx, "daily", time.Time{}, options())
	if err != nil {
		t.Fatalf("concurrent Generate: %v", err)
	}
	if concurrent.Status != store.BriefingGenerating {
		t.Errorf("concurrent status = %q, want generating", concurrent.Status)
	}
	if gen.callCount() != 1 {
		t.Errorf("model calls = %d, want 1", gen.callCount())
	}

	close(gen.release)
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := st.get("daily", started.PeriodStart)
		if got.Status == store.BriefingReady {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("briefing never became ready: %q", got.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRenderIsStableAndEscapesTitles(t *testing.T) {
	p := Period{
		Kind:  "weekly",
		Start: time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 1, 12, 0, 0, 0, 0, time.UTC),
	}
	items := []store.BriefingItem{{
		ID: "i", Source: "Src", Title: "A *title*", Link: "https://example.com/x", Score: 8,
	}}
	b := Briefing{
		ExecutiveSummary: "Summary",
		Developments: []Development{
			{Headline: "Head", Synthesis: "Body", WhyItMatters: "Why", ItemIDs: []int{1}},
		},
		EmergingTopics:     []string{"Topic"},
		RecommendedReading: []int{1},
	}
	got := Render(p, items, b)
	for _, want := range []string{"# Weekly Briefing", "## Executive summary", "## Key developments", "### Head", "**Why it matters:** Why", "## Emerging topics", "## Recommended reading", "[A \\*title\\*](https://example.com/x)"} {
		if !strings.Contains(got, want) {
			t.Errorf("render missing %q:\n%s", want, got)
		}
	}
	if again := Render(p, items, b); again != got {
		t.Error("render is not deterministic")
	}
}
