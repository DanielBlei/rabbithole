// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package classify

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/retry"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// ClassifierFactory resolves the configured backend. It is injected so the
// provider wiring lives in inference.ResolveClassifier and this package stays
// independent of any specific backend.
type ClassifierFactory func(ctx context.Context, cfg config.Config, vocabulary []string) (Classifier, error)

// Worker tuning defaults. They match the approved design and scoring's proven
// retry shape.
const (
	defaultClaimLimit    = 100
	defaultMaxAttempts   = 5
	defaultSlowRetry     = 30 * time.Minute
	defaultRetryAttempts = 3
)

// Stats summarises one classification pass.
type Stats struct {
	Candidates int
	Classified int
	Failed     int
	Skipped    bool
	Reason     string
}

// Worker classifies scored items in the serve process. It deliberately does not
// share ingest's single-flight slot: a slow or rate-limited classification pass
// must never block feed fetching.
type Worker struct {
	db      *store.Store
	src     config.Source
	factory ClassifierFactory
	version int

	interval      time.Duration
	claimLimit    int
	maxAttempts   int
	slowRetry     time.Duration
	retryAttempts int
	retryBackoff  time.Duration

	mu      sync.Mutex
	running bool
	pending bool
	closed  bool
	started bool
	root    context.Context
	stop    context.CancelFunc
	wg      sync.WaitGroup
}

// NewWorker returns a worker for db, reading its config from src. factory is
// normally inference.ResolveClassifier; tests substitute a fake.
func NewWorker(db *store.Store, src config.Source, factory ClassifierFactory) *Worker {
	return &Worker{
		db:            db,
		src:           src,
		factory:       factory,
		version:       Version,
		interval:      config.DefaultClassificationInterval,
		claimLimit:    defaultClaimLimit,
		maxAttempts:   defaultMaxAttempts,
		slowRetry:     defaultSlowRetry,
		retryAttempts: defaultRetryAttempts,
		retryBackoff:  time.Second,
	}
}

// Start begins the periodic self-healing scan. Call it once, before serving.
func (w *Worker) Start() {
	w.mu.Lock()
	if w.started || w.closed {
		w.mu.Unlock()
		return
	}
	w.started = true
	w.root, w.stop = context.WithCancel(context.Background())
	ctx := w.root
	w.wg.Add(1)
	w.mu.Unlock()
	go w.loop(ctx)
}

func (w *Worker) loop(ctx context.Context) {
	defer w.wg.Done()
	for {
		// Re-read the interval each round, so a config change applies without a
		// restart. w.interval is the fallback and the test seam.
		interval := w.tickInterval(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
			w.notify(false)
		}
	}
}

func (w *Worker) tickInterval(ctx context.Context) time.Duration {
	if cfg, _, err := w.src.Active(ctx); err == nil {
		if configured := time.Duration(cfg.Classification.Interval); configured > 0 {
			return configured
		}
	}
	if w.interval > 0 {
		return w.interval
	}
	return config.DefaultClassificationInterval
}

// Notify asks for a pass now, typically after an ingest run records items. A
// request that arrives while a pass is running schedules one more.
func (w *Worker) Notify() { w.notify(true) }

func (w *Worker) notify(queue bool) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	if w.running {
		if queue {
			w.pending = true
		}
		w.mu.Unlock()
		return
	}
	w.running = true
	ctx := w.rootLocked()
	w.wg.Add(1)
	w.mu.Unlock()

	go func() {
		defer w.wg.Done()
		w.logStats(w.run(ctx))
		w.finishPass()
	}()
}

// rootLocked returns the worker context, creating it on first use.
func (w *Worker) rootLocked() context.Context {
	if w.root == nil {
		w.root, w.stop = context.WithCancel(context.Background())
	}
	return w.root
}

func (w *Worker) finishPass() {
	w.mu.Lock()
	w.running = false
	again := w.pending && !w.closed
	if again {
		w.pending = false
	}
	w.mu.Unlock()
	if again {
		w.Notify()
	}
}

// RunOnce drains the claimable queue synchronously. It is the CLI backfill's
// engine and the seam tests use.
func (w *Worker) RunOnce(ctx context.Context) Stats {
	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		return Stats{Skipped: true, Reason: "a classification pass is already running"}
	}
	w.running = true
	w.mu.Unlock()
	stats := w.run(ctx)
	w.finishPass()
	return stats
}

// Shutdown stops the loop and cancels the in-flight pass, waiting for it or for
// ctx, whichever comes first.
func (w *Worker) Shutdown(ctx context.Context) {
	w.mu.Lock()
	w.closed = true
	if w.stop != nil {
		w.stop()
	}
	w.mu.Unlock()

	done := make(chan struct{})
	go func() {
		w.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func (w *Worker) run(ctx context.Context) Stats {
	logger := zerolog.Ctx(ctx)
	cfg, _, err := w.src.Active(ctx)
	if err != nil {
		logger.Warn().Err(err).Msg("classification: config unavailable, skipping pass")
		return Stats{Skipped: true, Reason: "config unavailable"}
	}
	cl := cfg.Classification
	if cl.Enabled != nil && !*cl.Enabled {
		return Stats{Skipped: true, Reason: "classification disabled"}
	}
	if cl.Provider == config.ProviderHeuristic {
		return Stats{Skipped: true, Reason: "heuristic classifier is a no-op"}
	}
	if w.factory == nil {
		return Stats{Skipped: true, Reason: "no classifier factory configured"}
	}

	var classifier Classifier
	stats := Stats{}
	for {
		if err := ctx.Err(); err != nil {
			break
		}
		candidates, err := w.db.ClaimClassification(
			ctx, w.version, time.Now(), w.claimLimit, w.maxAttempts)
		if err != nil {
			logger.Warn().Err(err).Msg("classification: claiming items failed")
			break
		}
		if len(candidates) == 0 {
			break
		}
		// Resolve the backend lazily: an idle install must not dial the model
		// on every tick just to find an empty queue.
		if classifier == nil {
			var vocabulary []string
			if cl.VocabularyHint != nil && *cl.VocabularyHint {
				tags, err := w.db.TopTags(ctx, time.Time{}, []string{store.TagKindKeyword}, 50)
				if err != nil {
					logger.Warn().Err(err).Msg("classification: vocabulary hint unavailable, continuing without it")
				} else {
					vocabulary = make([]string, len(tags))
					for i, tag := range tags {
						vocabulary[i] = tag.Slug
					}
				}
			}
			classifier, err = w.factory(ctx, cfg, vocabulary)
			if err != nil {
				logger.Warn().Err(err).Msg("classification: backend unavailable, items stay pending")
				return Stats{Skipped: true, Reason: "backend unavailable"}
			}
		}
		stats.Candidates += len(candidates)
		before := stats.Classified + stats.Failed
		batchSize := max(cl.BatchSize, 1)
		for start := 0; start < len(candidates); start += batchSize {
			end := min(start+batchSize, len(candidates))
			w.processBatch(ctx, classifier, candidates[start:end], &stats)
		}
		if stats.Classified+stats.Failed == before {
			logger.Warn().Int("candidates", len(candidates)).
				Msg("classification: pass made no progress, stopping to avoid a loop")
			break
		}
	}
	event := logger.Info()
	if stats.Candidates == 0 {
		event = logger.Debug()
	}
	event.
		Int("candidates", stats.Candidates).
		Int("classified", stats.Classified).
		Int("failed", stats.Failed).
		Msg("classification pass complete")
	return stats
}

func (w *Worker) processBatch(
	ctx context.Context,
	classifier Classifier,
	candidates []store.ClassificationCandidate,
	stats *Stats,
) {
	logger := zerolog.Ctx(ctx)
	if len(candidates) == 1 {
		candidate := candidates[0]
		verdict, err := w.classifyOne(ctx, classifier, candidate.Item)
		if err != nil {
			w.recordFailure(ctx, candidate, err)
			stats.Failed++
			return
		}
		w.storeVerdict(ctx, candidate, verdict, stats)
		return
	}

	verdicts, err := w.classifyBatch(ctx, classifier, candidates)
	if err != nil {
		logger.Warn().Err(err).Int("items", len(candidates)).
			Msg("classification batch failed, retrying individually")
		verdicts = nil
	}
	byID := make(map[string]ItemVerdict, len(verdicts))
	for _, verdict := range verdicts {
		byID[verdict.ID] = verdict
	}
	for _, candidate := range candidates {
		verdict, ok := byID[candidate.Item.ID]
		if !ok {
			verdict, err = w.classifyOne(ctx, classifier, candidate.Item)
			if err != nil {
				w.recordFailure(ctx, candidate, err)
				stats.Failed++
				continue
			}
		}
		w.storeVerdict(ctx, candidate, verdict, stats)
	}
}

func (w *Worker) storeVerdict(
	ctx context.Context,
	candidate store.ClassificationCandidate,
	verdict ItemVerdict,
	stats *Stats,
) {
	if _, err := w.db.ReplaceItemTags(
		ctx, candidate.Item.ID, w.version, verdict.Keywords, verdict.Assumptions); err != nil {
		w.recordFailure(ctx, candidate, err)
		stats.Failed++
		return
	}
	stats.Classified++
}

// classifyBatch asks for a whole batch and accepts it only when every item has
// exactly one verdict. Partial or malformed answers fall back to per-item calls.
func (w *Worker) classifyBatch(
	ctx context.Context,
	classifier Classifier,
	candidates []store.ClassificationCandidate,
) ([]ItemVerdict, error) {
	items := make([]feeds.Item, len(candidates))
	for i, candidate := range candidates {
		items[i] = candidate.Item
	}
	verdicts, err := classifier.Classify(ctx, items)
	if err != nil {
		return nil, err
	}
	if len(verdicts) != len(items) {
		return nil, fmt.Errorf("classifier returned %d verdicts for %d items", len(verdicts), len(items))
	}
	for _, verdict := range verdicts {
		if verdict.ID == "" {
			return nil, fmt.Errorf("classifier returned a verdict without an item id")
		}
	}
	return verdicts, nil
}

// classifyOne is the single retry point: a transient timeout or a one-shot
// unusable response usually clears on a re-ask.
func (w *Worker) classifyOne(
	ctx context.Context,
	classifier Classifier,
	item feeds.Item,
) (ItemVerdict, error) {
	logger := zerolog.Ctx(ctx)
	var verdict ItemVerdict
	err := retry.Do(ctx, w.retryAttempts, w.retryBackoff, func() error {
		out, err := classifier.Classify(ctx, []feeds.Item{item})
		if err != nil {
			return err
		}
		if len(out) != 1 {
			return fmt.Errorf("classifier returned %d verdicts for one item", len(out))
		}
		if out[0].ID != "" && out[0].ID != item.ID {
			return fmt.Errorf("classifier returned a verdict for %q, want %q", out[0].ID, item.ID)
		}
		out[0].ID = item.ID
		verdict = out[0]
		return nil
	}, func(attempt int, err error, delay time.Duration) {
		logger.Warn().Str("item", truncate(item.Title, 80)).Int("attempt", attempt).
			Str("retry_in", delay.String()).Err(err).
			Msg("classification failed, retrying item")
	})
	return verdict, err
}

func (w *Worker) recordFailure(ctx context.Context, candidate store.ClassificationCandidate, err error) {
	logger := zerolog.Ctx(ctx)
	attempts := candidate.Attempts + 1
	next := time.Now().Add(w.slowRetry)
	if attempts >= w.maxAttempts {
		// Terminal for the queue: only a version bump or a manual retry resets it.
		next = time.Now().Add(365 * 24 * time.Hour)
	}
	if markErr := w.db.MarkClassificationFailed(
		ctx, candidate.Item.ID, w.version, attempts, err.Error(), next); markErr != nil {
		logger.Warn().Err(markErr).Str("item", candidate.Item.ID).
			Msg("classification: recording failure failed")
	}
	logger.Warn().Str("item", truncate(candidate.Item.Title, 80)).Int("attempts", attempts).
		Err(err).Msg("classification failed after retries")
}

func (w *Worker) logStats(stats Stats) {
	if stats.Skipped {
		zerolog.Ctx(context.Background()).Debug().Str("reason", stats.Reason).
			Msg("classification pass skipped")
	}
}
