// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/rank"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// lockedBuffer makes the zerolog writer safe when warnings from scoring
// goroutines and progress callbacks write concurrently.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

// decodeLogLines decodes zerolog's JSON output, one object per line.
func decodeLogLines(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil
	}
	var lines []map[string]any
	for _, line := range bytes.Split(trimmed, []byte("\n")) {
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		lines = append(lines, m)
	}
	return lines
}

// runWithScorer runs one ingest with a deterministic scorer injected through
// run's resolver seam and returns the decoded log lines.
func runWithScorer(
	t *testing.T,
	ctx context.Context,
	cfg *testCfg,
	db *store.Store,
	resolve scorerResolver,
	opts Options,
) (Outcome, []map[string]any, error) {
	t.Helper()
	var buf lockedBuffer
	ctx = zerolog.New(&buf).Level(zerolog.InfoLevel).WithContext(ctx)
	out, err := run(ctx, cfg.Config, testProfile("test"), db, time.Now(), opts, resolve)
	return out, decodeLogLines(t, buf.Bytes()), err
}

// fixedResolver returns one scorer and counts how many times Run resolved it.
func fixedResolver(scorer rank.Scorer, calls *atomic.Int32) scorerResolver {
	return func(context.Context, config.Config, bool) (rank.Scorer, error) {
		if calls != nil {
			calls.Add(1)
		}
		return scorer, nil
	}
}

// staticScorer immediately returns a valid verdict for every item and records
// how many scoring requests it saw.
type staticScorer struct {
	mu     sync.Mutex
	calls  int
	items  []feeds.Item
	peak   int
	active int
}

func (s *staticScorer) Validate(context.Context) error { return nil }

func (s *staticScorer) Score(_ context.Context, _ string, items []feeds.Item) ([]rank.ItemScore, error) {
	s.mu.Lock()
	s.calls++
	s.items = append(s.items, items...)
	s.active++
	if s.active > s.peak {
		s.peak = s.active
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.active--
		s.mu.Unlock()
	}()
	return validScores(items), nil
}

func (s *staticScorer) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *staticScorer) Peak() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peak
}

// gatedScorer blocks every Score call until the test closes release (or the
// context is cancelled). It closes full once want calls are running at the
// same time, so a test can prove overlap without sleeping.
type gatedScorer struct {
	mu       sync.Mutex
	active   int
	peak     int
	want     int
	started  chan feeds.Item
	full     chan struct{}
	fullOnce sync.Once
	release  chan struct{}
}

func newGatedScorer(want int) *gatedScorer {
	return &gatedScorer{
		want:    want,
		started: make(chan feeds.Item, 64),
		full:    make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (s *gatedScorer) Validate(context.Context) error { return nil }

func (s *gatedScorer) Score(ctx context.Context, _ string, items []feeds.Item) ([]rank.ItemScore, error) {
	s.mu.Lock()
	s.active++
	current := s.active
	if current > s.peak {
		s.peak = current
	}
	s.mu.Unlock()

	if len(items) > 0 {
		select {
		case s.started <- items[0]:
		default:
		}
	}
	if s.want > 0 && current >= s.want {
		s.fullOnce.Do(func() { close(s.full) })
	}

	select {
	case <-s.release:
	case <-ctx.Done():
		s.done()
		return nil, ctx.Err()
	}
	s.done()
	return validScores(items), nil
}

func (s *gatedScorer) done() {
	s.mu.Lock()
	s.active--
	s.mu.Unlock()
}

func (s *gatedScorer) Peak() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peak
}

// failSourceScorer fails every item from one named feed and scores the rest.
type failSourceScorer struct {
	source string
}

func (s *failSourceScorer) Validate(context.Context) error { return nil }

func (s *failSourceScorer) Score(_ context.Context, _ string, items []feeds.Item) ([]rank.ItemScore, error) {
	for _, item := range items {
		if item.Source == s.source {
			return nil, errors.New("model refuses to score this source")
		}
	}
	return validScores(items), nil
}

func validScores(items []feeds.Item) []rank.ItemScore {
	scores := make([]rank.ItemScore, 0, len(items))
	for _, item := range items {
		scores = append(scores, rank.ItemScore{ID: item.ID, Score: 6, Reason: "Relevant to the reader."})
	}
	return scores
}

// waitClosed waits for ch or fails a generous deadline; used only as a safety
// net, never as the concurrency signal itself.
func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// Four one-item feeds with max_parallel=2: the old per-feed ScoreAll loop can
// only ever run one feed at a time (each feed has a single batch), so at most
// one request is in flight. The shared pool starts two batches from two
// different feeds, which is the #67 fix.
func TestRunScoresFeedsThroughOneRunWidePool(t *testing.T) {
	cfg := testConfig(t,
		config.Feed{Name: "A", URL: serveRSS(t, multiItemRSS(t, "pool-a", 1))},
		config.Feed{Name: "B", URL: serveRSS(t, multiItemRSS(t, "pool-b", 1))},
		config.Feed{Name: "C", URL: serveRSS(t, multiItemRSS(t, "pool-c", 1))},
		config.Feed{Name: "D", URL: serveRSS(t, multiItemRSS(t, "pool-d", 1))},
	)
	cfg.Inference.BatchSize = 1
	cfg.Inference.MaxParallel = 2
	db := openStore(t, cfg)
	scorer := newGatedScorer(2)

	type runResult struct {
		out Outcome
		raw []byte
		err error
	}
	resultCh := make(chan runResult, 1)
	var buf lockedBuffer
	ctx := zerolog.New(&buf).Level(zerolog.InfoLevel).WithContext(t.Context())
	go func() {
		out, err := run(
			ctx, cfg.Config, testProfile("test"), db, time.Now(),
			Options{Record: true, Client: loopbackClient}, fixedResolver(scorer, nil),
		)
		resultCh <- runResult{out: out, raw: buf.Bytes(), err: err}
	}()

	// Two different feeds must overlap. With the old per-feed limiter the
	// second started event never arrives, so the deadline catches it.
	sources := map[string]bool{}
	for range 2 {
		select {
		case item := <-scorer.started:
			sources[item.Source] = true
		case <-time.After(5 * time.Second):
			t.Fatal("scoring never ran two batches concurrently")
		}
	}
	if len(sources) != 2 {
		t.Fatalf("concurrent batches came from %v, want two different feeds", sources)
	}
	close(scorer.release)

	var res runResult
	select {
	case res = <-resultCh:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not finish after the scoring gate opened")
	}
	if res.err != nil {
		t.Fatalf("Run: %v", res.err)
	}
	if got := scorer.Peak(); got != 2 {
		t.Errorf("scorer peak in-flight = %d, want 2", got)
	}
	if res.out.Scored != 4 {
		t.Errorf("Scored = %d, want 4", res.out.Scored)
	}

	run := linesWith(decodeLogLines(t, res.raw), "run scoring progress")
	if len(run) != 4 {
		t.Fatalf("run scoring progress lines = %d, want 4", len(run))
	}
	for i, line := range run {
		if done := int(line["done"].(float64)); done != i+1 {
			t.Errorf("run line %d done = %d, want %d", i, done, i+1)
		}
		if of := int(line["of"].(float64)); of != 4 {
			t.Errorf("run line %d of = %d, want 4", i, of)
		}
		peak := int(line["peak"].(float64))
		inFlight := int(line["in_flight"].(float64))
		if peak != 2 {
			t.Errorf("run line %d peak = %d, want 2", i, peak)
		}
		if inFlight < 0 || inFlight > peak {
			t.Errorf("run line %d in_flight = %d, want 0..%d", i, inFlight, peak)
		}
	}
}

// A single large feed still uses the full max_parallel ceiling.
func TestRunScoringPeakForSingleFeed(t *testing.T) {
	cfg := testConfig(t,
		config.Feed{Name: "Firehose", URL: serveRSS(t, multiItemRSS(t, "one-feed", 6))},
	)
	cfg.Inference.BatchSize = 1
	cfg.Inference.MaxParallel = 3
	db := openStore(t, cfg)
	scorer := newGatedScorer(3)

	type runResult struct {
		out Outcome
		err error
	}
	resultCh := make(chan runResult, 1)
	go func() {
		out, err := run(
			t.Context(), cfg.Config, testProfile("test"), db, time.Now(),
			Options{Record: true, Client: loopbackClient}, fixedResolver(scorer, nil),
		)
		resultCh <- runResult{out: out, err: err}
	}()

	waitClosed(t, scorer.full, "three concurrent scoring requests")
	close(scorer.release)

	select {
	case res := <-resultCh:
		if res.err != nil {
			t.Fatalf("Run: %v", res.err)
		}
		if res.out.Scored != 6 {
			t.Errorf("Scored = %d, want 6", res.out.Scored)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not finish after the scoring gate opened")
	}
	if got := scorer.Peak(); got != 3 {
		t.Errorf("scorer peak in-flight = %d, want 3", got)
	}
}

// The run-wide pool must never exceed max_parallel even when several feeds
// contribute batches that could run at once.
func TestRunScoringPeakNeverExceedsMaxParallel(t *testing.T) {
	cfg := testConfig(t,
		config.Feed{Name: "A", URL: serveRSS(t, multiItemRSS(t, "cap-a", 2))},
		config.Feed{Name: "B", URL: serveRSS(t, multiItemRSS(t, "cap-b", 2))},
		config.Feed{Name: "C", URL: serveRSS(t, multiItemRSS(t, "cap-c", 2))},
		config.Feed{Name: "D", URL: serveRSS(t, multiItemRSS(t, "cap-d", 2))},
	)
	cfg.Inference.BatchSize = 1
	cfg.Inference.MaxParallel = 2
	db := openStore(t, cfg)
	scorer := newGatedScorer(2)

	type runResult struct {
		raw []byte
		err error
	}
	resultCh := make(chan runResult, 1)
	var buf lockedBuffer
	ctx := zerolog.New(&buf).Level(zerolog.InfoLevel).WithContext(t.Context())
	go func() {
		_, err := run(
			ctx, cfg.Config, testProfile("test"), db, time.Now(),
			Options{Record: true, Client: loopbackClient}, fixedResolver(scorer, nil),
		)
		resultCh <- runResult{raw: buf.Bytes(), err: err}
	}()

	waitClosed(t, scorer.full, "two concurrent scoring requests")
	close(scorer.release)
	var res runResult
	select {
	case res = <-resultCh:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not finish after the scoring gate opened")
	}
	if res.err != nil {
		t.Fatalf("Run: %v", res.err)
	}
	if got := scorer.Peak(); got > 2 {
		t.Errorf("scorer peak in-flight = %d, want <= 2", got)
	}
	for _, line := range linesWith(decodeLogLines(t, res.raw), "run scoring progress") {
		peak := int(line["peak"].(float64))
		inFlight := int(line["in_flight"].(float64))
		if peak > 2 {
			t.Errorf("logged peak = %d, want <= 2", peak)
		}
		if inFlight > 2 || inFlight < 0 {
			t.Errorf("logged in_flight = %d, want 0..2", inFlight)
		}
	}
}

// A cancel that arrives while batches are in flight returns promptly (not
// after retry exhaustion) and persists nothing from the cancelled run. Rows
// committed by the earlier successful run stay durable.
func TestRunCancelDuringScoringLeavesCommittedFeedsDurable(t *testing.T) {
	cfg := testConfig(t,
		config.Feed{Name: "A", URL: serveRSS(t, multiItemRSS(t, "cancel-a", 1))},
		config.Feed{Name: "B", URL: serveRSS(t, multiItemRSS(t, "cancel-b", 1))},
	)
	db := openStore(t, cfg)

	if _, _, err := runWithScorer(t, t.Context(), cfg, db, fixedResolver(&staticScorer{}, nil),
		Options{Record: true, Client: loopbackClient}); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if got := sourceCounts(t, db); got["A"] != 1 || got["B"] != 1 {
		t.Fatalf("first run source counts = %v, want A:1 B:1", got)
	}

	// Add a third feed whose items are the only candidates in the next run.
	wide := config.Duration(365 * 24 * time.Hour)
	if _, err := db.SeedFeeds(t.Context(), config.FeedsDoc{
		Defaults: config.FeedDefaults{Since: &wide},
		Feeds: []config.Feed{
			{Name: "C", URL: serveRSS(t, multiItemRSS(t, "cancel-c", 1))},
		},
	}); err != nil {
		t.Fatalf("seed feed C: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	scorer := newGatedScorer(0) // never releases: cancel is the only exit
	type runResult struct {
		out Outcome
		err error
	}
	resultCh := make(chan runResult, 1)
	go func() {
		out, err := run(
			ctx, cfg.Config, testProfile("test"), db, time.Now(),
			Options{Record: true, Client: loopbackClient}, fixedResolver(scorer, nil),
		)
		resultCh <- runResult{out: out, err: err}
	}()

	select {
	case item := <-scorer.started:
		if item.Source != "C" {
			t.Fatalf("cancelled run scored %q, want only the new feed C", item.Source)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled run never started scoring")
	}
	cancel()

	select {
	case res := <-resultCh:
		if !errors.Is(res.err, context.Canceled) {
			t.Fatalf("Run error = %v, want context.Canceled", res.err)
		}
		if res.out.Scored != 0 {
			t.Errorf("cancelled run Scored = %d, want 0", res.out.Scored)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return promptly after cancellation")
	}

	// The earlier feeds' rows are still there; the cancelled feed has none.
	got := sourceCounts(t, db)
	if got["A"] != 1 || got["B"] != 1 {
		t.Errorf("source counts after cancel = %v, want earlier A:1 B:1 preserved", got)
	}
	if got["C"] != 0 {
		t.Errorf("cancelled feed C recorded %d items, want 0", got["C"])
	}
}

// A feed whose items all fail to score contributes Failed only to itself; the
// other feed's scores, results and persistence are unaffected, and accounting
// is still emitted for both.
func TestRunAttributesPartialScoringFailureToFeed(t *testing.T) {
	cfg := testConfig(t,
		config.Feed{Name: "Good", URL: serveRSS(t, multiItemRSS(t, "fail-good", 2))},
		config.Feed{Name: "Bad", URL: serveRSS(t, multiItemRSS(t, "fail-bad", 1))},
	)
	cfg.Inference.BatchSize = 1
	cfg.Inference.MaxParallel = 2
	db := openStore(t, cfg)

	out, lines, err := runWithScorer(t, t.Context(), cfg, db,
		fixedResolver(&failSourceScorer{source: "Bad"}, nil),
		Options{Record: true, Client: loopbackClient})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Failed != 1 {
		t.Errorf("Failed = %d, want 1 (only the Bad feed)", out.Failed)
	}
	if out.Scored != 2 {
		t.Errorf("Scored = %d, want 2 (the Good feed)", out.Scored)
	}
	if len(out.Results) != 2 {
		t.Fatalf("Results = %d, want 2", len(out.Results))
	}
	for _, result := range out.Results {
		if result.Item.Source != "Good" {
			t.Errorf("result from %q, want only Good", result.Item.Source)
		}
	}

	// The Bad item is recorded seen-only (no score), exactly as a per-feed
	// failure did before: it is retried by a later run, not silently scored.
	var badID string
	for _, item := range out.Unseen {
		if item.Source == "Bad" {
			badID = item.ID
		}
	}
	if badID == "" {
		t.Fatal("Bad feed's candidate missing from Outcome.Unseen")
	}
	row, err := db.Get(t.Context(), badID)
	if err != nil {
		t.Fatalf("Get(%s): %v", badID, err)
	}
	if row.LLMScore != nil {
		t.Errorf("Bad item score = %v, want none", *row.LLMScore)
	}

	if accounts := linesWith(lines, "feed accounting"); len(accounts) != 2 {
		t.Errorf("feed accounting lines = %d, want 2", len(accounts))
	}
	processed := linesWith(lines, "items processed")
	byFeed := map[string]map[string]any{}
	for _, line := range processed {
		byFeed[line["feed"].(string)] = line
	}
	if got := int(byFeed["Good"]["failed"].(float64)); got != 0 {
		t.Errorf("Good failed = %d, want 0", got)
	}
	if got := int(byFeed["Bad"]["failed"].(float64)); got != 1 {
		t.Errorf("Bad failed = %d, want 1", got)
	}
	if got := int(byFeed["Bad"]["processed"].(float64)); got != 0 {
		t.Errorf("Bad processed = %d, want 0", got)
	}
}

// B-1 (CDR-52): two feeds can carry one syndicated article under the same GUID
// but different links, so both copies derive the same Item.ID. The pre-pass
// claims IDs first-wins like links: the first feed's copy is scored and
// persisted once, the later copy never reaches the model, and no record
// collision discards the score.
func TestRunDuplicateGUIDScoresFirstFeedOnce(t *testing.T) {
	const guid = "urn:uuid:0b7f42e2-1c1d-4f2e-9a3b-5c6d7e8f9a0b"
	cfg := testConfig(t,
		config.Feed{Name: "Alpha", URL: serveRSS(t, feedRSSWithGUID("Same article", guid, "https://alpha.test/a42?utm=rss"))},
		config.Feed{Name: "Beta", URL: serveRSS(t, feedRSSWithGUID("Same article", guid, "https://beta.test/a42"))},
	)
	db := openStore(t, cfg)
	scorer := &staticScorer{}

	out, lines, err := runWithScorer(t, t.Context(), cfg, db, fixedResolver(scorer, nil),
		Options{Record: true, Client: loopbackClient})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Scored != 1 || out.Failed != 0 {
		t.Errorf("Scored/Failed = %d/%d, want 1/0", out.Scored, out.Failed)
	}
	if len(out.Unseen) != 1 || len(out.Results) != 1 {
		t.Fatalf("Unseen/Results = %d/%d, want 1/1", len(out.Unseen), len(out.Results))
	}
	if got := out.Results[0].Item.Source; got != "Alpha" {
		t.Errorf("surviving result is from %q, want Alpha (first in config order)", got)
	}
	if got := scorer.Calls(); got != 1 {
		t.Errorf("scoring calls = %d, want 1 (the duplicate ID never reaches the model)", got)
	}

	row, err := db.GetByID(t.Context(), out.Results[0].Item.ID)
	if err != nil {
		t.Fatalf("GetByID(%s): %v", out.Results[0].Item.ID, err)
	}
	if row.LLMScore == nil || *row.LLMScore != 6 {
		t.Errorf("persisted row score = %v, want 6", row.LLMScore)
	}
	if row.Source != "Alpha" {
		t.Errorf("persisted row source = %q, want Alpha", row.Source)
	}

	if warns := linesWith(lines, "recording feed failed, skipping"); len(warns) != 0 {
		t.Errorf("record failure warnings = %v, want none", warns)
	}

	accounts := map[string]map[string]any{}
	for _, line := range linesWith(lines, "feed accounting") {
		accounts[line["feed"].(string)] = line
	}
	if got := int(accounts["Alpha"]["new"].(float64)); got != 1 {
		t.Errorf("Alpha new = %d, want 1", got)
	}
	if got := int(accounts["Alpha"]["skipped"].(float64)); got != 0 {
		t.Errorf("Alpha skipped = %d, want 0", got)
	}
	if got := int(accounts["Beta"]["new"].(float64)); got != 0 {
		t.Errorf("Beta new = %d, want 0 (duplicate ID claimed by Alpha)", got)
	}
	if got := int(accounts["Beta"]["skipped"].(float64)); got != 1 {
		t.Errorf("Beta skipped = %d, want 1 (duplicate ID dropped in the pre-pass)", got)
	}
}

// N-1 (CDR-52): a record failure on a middle feed leaves an earlier feed's
// committed rows intact and does not stop a later feed from being recorded. A
// row already owning the middle feed's Item.ID under a different link makes its
// insert fail on the items.id primary key — the same store-level collision a
// duplicate GUID produced before the pre-pass claimed IDs.
func TestRunRecordFailureIsolatesFeeds(t *testing.T) {
	const guid = "urn:uuid:5d2a9c31-7b6e-4f0a-8c1d-2e3f4a5b6c7d"
	warmCfg := testConfig(t,
		config.Feed{Name: "Warm", URL: serveRSS(t, feedRSSWithGUID("Warm", guid, "https://x.test/warm"))},
	)
	warmDB := openStore(t, warmCfg)
	warm, _, err := runWithScorer(t, t.Context(), warmCfg, warmDB, fixedResolver(&staticScorer{}, nil),
		Options{Record: true, Client: loopbackClient})
	if err != nil {
		t.Fatalf("warm Run: %v", err)
	}
	if len(warm.Unseen) != 1 {
		t.Fatalf("warm Unseen = %d, want 1", len(warm.Unseen))
	}
	colliding := warm.Unseen[0]

	cfg := testConfig(t,
		config.Feed{Name: "Earlier", URL: serveRSS(t, feedRSSWithGUID("Earlier", "urn:uuid:earlier", "https://x.test/earlier"))},
		config.Feed{Name: "Failing", URL: serveRSS(t, feedRSSWithGUID("Failing", guid, "https://x.test/failing"))},
		config.Feed{Name: "Later", URL: serveRSS(t, feedRSSWithGUID("Later", "urn:uuid:later", "https://x.test/later"))},
	)
	db := openStore(t, cfg)
	if err := db.Record(t.Context(), []feeds.Item{colliding}, nil, time.Now()); err != nil {
		t.Fatalf("seed colliding row: %v", err)
	}

	out, lines, err := runWithScorer(t, t.Context(), cfg, db, fixedResolver(&staticScorer{}, nil),
		Options{Record: true, Client: loopbackClient})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Scored != 2 || out.Failed != 0 {
		t.Errorf("Scored/Failed = %d/%d, want 2/0 (the two feeds that recorded)", out.Scored, out.Failed)
	}
	if len(out.Results) != 2 {
		t.Errorf("Results = %d, want 2", len(out.Results))
	}

	warns := linesWith(lines, "recording feed failed, skipping")
	if len(warns) != 1 || warns[0]["feed"] != "Failing" {
		t.Fatalf("record failure warnings = %v, want exactly one for Failing", warns)
	}
	recorded := map[string]bool{}
	for _, line := range linesWith(lines, "ingested to db") {
		recorded[line["feed"].(string)] = true
	}
	if !recorded["Earlier"] || !recorded["Later"] {
		t.Errorf("feed completion lines = %v, want Earlier and Later committed", recorded)
	}
	if recorded["Failing"] {
		t.Errorf("failing feed logged a successful ingest: %v", recorded)
	}

	counts := sourceCounts(t, db)
	if counts["Earlier"] != 1 || counts["Later"] != 1 {
		t.Errorf("source counts = %v, want Earlier:1 Later:1", counts)
	}
	if counts["Failing"] != 0 {
		t.Errorf("failing feed rows = %d, want 0 (its transaction rolled back)", counts["Failing"])
	}
	if _, err := db.Get(t.Context(), "https://x.test/failing"); !errors.Is(err, store.ErrItemNotFound) {
		t.Errorf("Get(failing link) error = %v, want ErrItemNotFound", err)
	}
}

// Empty and failed feeds contribute no batches; only the nonempty feed drives
// progress, and batchCount(0, …) contributes zero rather than dividing by zero.
func TestRunScoresOnlyNonemptyFeeds(t *testing.T) {
	cfg := testConfig(t,
		config.Feed{Name: "Full", URL: serveRSS(t, multiItemRSS(t, "mix-full", 4))},
		config.Feed{Name: "Empty", URL: serveRSS(t, multiItemRSS(t, "mix-empty", 0))},
		config.Feed{Name: "Dead", URL: "http://127.0.0.1:1/feed"},
	)
	cfg.Inference.BatchSize = 2
	cfg.Inference.MaxParallel = 2
	db := openStore(t, cfg)
	scorer := &staticScorer{}

	out, lines, err := runWithScorer(t, t.Context(), cfg, db, fixedResolver(scorer, nil),
		Options{Record: true, Client: loopbackClient})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Scored != 4 || out.Failed != 0 {
		t.Errorf("Scored/Failed = %d/%d, want 4/0", out.Scored, out.Failed)
	}
	if got := scorer.Calls(); got != 2 {
		t.Errorf("scoring calls = %d, want 2 (only Full's two batches)", got)
	}
	if accounts := linesWith(lines, "feed accounting"); len(accounts) != 3 {
		t.Errorf("feed accounting lines = %d, want 3", len(accounts))
	}
	run := linesWith(lines, "run scoring progress")
	if len(run) != 2 {
		t.Fatalf("run scoring progress lines = %d, want 2", len(run))
	}
	for _, line := range run {
		if of := int(line["of"].(float64)); of != 2 {
			t.Errorf("run of = %d, want 2", of)
		}
	}
	perFeed := linesWith(lines, "scoring progress")
	if len(perFeed) != 2 {
		t.Fatalf("per-feed progress lines = %d, want 2", len(perFeed))
	}
	for _, line := range perFeed {
		if line["feed"] != "Full" {
			t.Errorf("progress line for %v, want only Full", line["feed"])
		}
	}
}

// A run with no candidates never resolves (or connects to) a model and emits
// no scoring progress at all.
func TestRunAllFeedsEmptyNeverResolvesScorer(t *testing.T) {
	cfg := testConfig(t,
		config.Feed{Name: "A", URL: serveRSS(t, multiItemRSS(t, "none-a", 0))},
		config.Feed{Name: "B", URL: serveRSS(t, multiItemRSS(t, "none-b", 0))},
	)
	// A non-heuristic provider makes the "connecting to model" line observable;
	// resolving a scorer at all would be a regression for an empty run.
	cfg.Inference.Provider = "vllm"
	db := openStore(t, cfg)
	var calls atomic.Int32

	out, lines, err := runWithScorer(t, t.Context(), cfg, db,
		fixedResolver(&staticScorer{}, &calls), Options{Record: true, Client: loopbackClient})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("scorer resolved %d times, want 0 for an empty run", got)
	}
	if got := linesWith(lines, "connecting to model"); len(got) != 0 {
		t.Errorf("empty run logged a model connect line: %v", got)
	}
	if out.Scored != 0 {
		t.Errorf("Scored = %d, want 0", out.Scored)
	}
	if got := linesWith(lines, "run scoring progress"); len(got) != 0 {
		t.Errorf("run scoring progress lines = %d, want 0", len(got))
	}
	if got := linesWith(lines, "scoring progress"); len(got) != 0 {
		t.Errorf("per-feed scoring progress lines = %d, want 0", len(got))
	}
	complete := linesWith(lines, "ingest complete")
	if len(complete) != 1 {
		t.Fatalf("ingest complete lines = %d, want 1", len(complete))
	}
	if got := int(complete[0]["scored"].(float64)); got != 0 {
		t.Errorf("ingest complete scored = %d, want 0", got)
	}
}

// Run-wide progress totals equal the sum of the per-feed batch totals, and
// every line carries sane in_flight/peak fields.
func TestRunProgressTotalsAndPoolFields(t *testing.T) {
	cfg := testConfig(t,
		config.Feed{Name: "A", URL: serveRSS(t, multiItemRSS(t, "totals-a", 5))},
		config.Feed{Name: "B", URL: serveRSS(t, multiItemRSS(t, "totals-b", 3))},
	)
	lines := runLines(t, cfg)

	run := linesWith(lines, "run scoring progress")
	if len(run) != 5 {
		t.Fatalf("run scoring progress lines = %d, want 5", len(run))
	}
	perFeedOf := map[string]int{}
	for _, line := range linesWith(lines, "scoring progress") {
		perFeedOf[line["feed"].(string)] = int(line["of"].(float64))
	}
	perFeedTotal := 0
	for _, of := range perFeedOf {
		perFeedTotal += of
	}
	for i, line := range run {
		if got := int(line["of"].(float64)); got != perFeedTotal {
			t.Errorf("run line %d of = %d, want sum of per-feed totals %d", i, got, perFeedTotal)
		}
		if got := int(line["done"].(float64)); got != i+1 {
			t.Errorf("run line %d done = %d, want %d", i, got, i+1)
		}
		peak, ok := line["peak"].(float64)
		if !ok {
			t.Fatalf("run line %d has no numeric peak: %v", i, line)
		}
		inFlight, ok := line["in_flight"].(float64)
		if !ok {
			t.Fatalf("run line %d has no numeric in_flight: %v", i, line)
		}
		if peak < 1 || peak > float64(cfg.Inference.MaxParallel) {
			t.Errorf("run line %d peak = %v, want 1..%d", i, peak, cfg.Inference.MaxParallel)
		}
		if inFlight < 0 || inFlight > peak {
			t.Errorf("run line %d in_flight = %v, want 0..%v", i, inFlight, peak)
		}
	}
}

// batchCount must predict exactly how many batches rank.Chunk will produce for
// every normalization case the two helpers share.
func TestBatchCountMirrorsChunk(t *testing.T) {
	for _, size := range []int{0, 1, 2, 5} {
		for _, n := range []int{0, 1, 2, 3, 6, 7, 100} {
			items := make([]feeds.Item, n)
			if got, want := batchCount(n, size), len(rank.Chunk(items, size)); got != want {
				t.Errorf("batchCount(%d, %d) = %d, want %d chunks", n, size, got, want)
			}
		}
	}
}
