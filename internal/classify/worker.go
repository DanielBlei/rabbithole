// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package classify

import (
	"context"
	"errors"
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
	// defaultClaimLease is how long a claimed wave stays owned by a worker
	// that never reports back. Claims are just-in-time — one wave of
	// batchSize × maxParallel items at a time, reserved immediately before it
	// runs — so the lease only has to outlive the slowest legitimate wave: a
	// batch whose every item needs retryAttempts classifier calls, each
	// bounded by the provider's chat timeout (5 minutes for ollama, 3 for
	// vLLM), plus the database writes. 30 minutes covers 3 × 5 minutes with
	// headroom. A wave that somehow outlives the lease stays correct rather
	// than silent: writes are guarded by the claim token, and the first
	// ErrStaleClaim abandons the wave's remaining items, so an expired lease
	// costs at most one wave of duplicate model work while it recovers a
	// crashed worker's items without operator action.
	defaultClaimLease = 30 * time.Minute
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
	claimLease    time.Duration

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
		claimLease:    defaultClaimLease,
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
		stats, err := w.run(ctx)
		w.logResult(ctx, stats, err)
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
// engine and the seam tests use. The error is non-nil when a database failure
// stopped the pass — outcomes could not be persisted, and a caller that exits
// zero on it would report work that was never recorded. Model failures are not
// errors: they are recorded against the item and retried on a later pass.
func (w *Worker) RunOnce(ctx context.Context) (Stats, error) {
	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		return Stats{Skipped: true, Reason: "a classification pass is already running"}, nil
	}
	w.running = true
	w.mu.Unlock()
	stats, err := w.run(ctx)
	w.finishPass()
	return stats, err
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

// batchRunner carries the mutable state one pass shares across concurrent
// batch processors: the running totals, the flag that a required database
// write failed — which stops the pass and reaches the caller as an error —
// and the flag that the wave's claim was taken over, which abandons the
// claim's remaining items without further model requests.
type batchRunner struct {
	mu        sync.Mutex
	stats     Stats
	stopped   bool
	fatalErr  error
	lostClaim bool
}

func (r *batchRunner) addCandidates(n int) {
	r.mu.Lock()
	r.stats.Candidates += n
	r.mu.Unlock()
}

func (r *batchRunner) record(classified, failed int) {
	r.mu.Lock()
	r.stats.Classified += classified
	r.stats.Failed += failed
	r.mu.Unlock()
}

func (r *batchRunner) progress() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stats.Classified + r.stats.Failed
}

func (r *batchRunner) snapshot() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stats
}

// stop marks the pass as unwinnable: a required write failed and continuing
// would claim items the pass cannot record outcomes for. err is the failure
// that ended the pass; it is what RunOnce reports, so a caller — the CLI's
// --wait above all — can tell a completed pass from a broken one. The first
// failure wins: later ones only add detail to the log.
func (r *batchRunner) stop(err error) {
	r.mu.Lock()
	if !r.stopped {
		r.stopped = true
		r.fatalErr = err
	}
	r.mu.Unlock()
}

// failure returns the error that stopped the pass, if one did.
func (r *batchRunner) failure() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fatalErr
}

func (r *batchRunner) done() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopped
}

// loseClaim records that the wave's claim token was rejected: another worker
// owns the items now. The claim's remaining candidates must not consume
// further model requests — their verdicts could only be discarded.
func (r *batchRunner) loseClaim() {
	r.mu.Lock()
	r.lostClaim = true
	r.mu.Unlock()
}

func (r *batchRunner) claimLost() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lostClaim
}

// beginWave resets the per-claim state for a freshly claimed wave: a new
// claim is new ownership, untouched by the previous wave losing its own.
func (r *batchRunner) beginWave() {
	r.mu.Lock()
	r.lostClaim = false
	r.mu.Unlock()
}

func (w *Worker) run(ctx context.Context) (Stats, error) {
	logger := zerolog.Ctx(ctx)
	cfg, _, err := w.src.Active(ctx)
	if err != nil {
		logger.Warn().Err(err).Msg("classification: config unavailable, skipping pass")
		return Stats{Skipped: true, Reason: "config unavailable"}, nil
	}
	cl := cfg.Classification
	if cl.Enabled != nil && !*cl.Enabled {
		return Stats{Skipped: true, Reason: "classification disabled"}, nil
	}
	if cl.Provider == config.ProviderHeuristic {
		return Stats{Skipped: true, Reason: "heuristic classifier is a no-op"}, nil
	}
	if w.factory == nil {
		return Stats{Skipped: true, Reason: "no classifier factory configured"}, nil
	}

	var classifier Classifier
	r := &batchRunner{}
	batchSize := max(cl.BatchSize, 1)
	maxParallel := max(cl.MaxParallel, 1)
	// Just-in-time claiming: one wave of at most batchSize × maxParallel items
	// is reserved immediately before it runs, never a whole claimLimit-sized
	// backlog. The lease therefore covers work that is about to happen, and a
	// lease that expires mid-pass can strand at most one wave's worth of
	// duplicate model work — the token check and the stale-claim abandon below
	// keep even that bounded.
	wave := min(w.claimLimit, batchSize*maxParallel)
	for {
		if err := ctx.Err(); err != nil {
			break
		}
		// Probe before claiming. A claim installs an ownership lease, and an
		// unavailable backend must not leave claimed items stranded behind
		// one. The probe is a read, so an idle install still never resolves —
		// let alone dials — the model on every tick.
		eligible, err := w.db.EligibleClassificationItems(ctx, time.Time{}, 1, w.version)
		if err != nil {
			logger.Warn().Err(err).Msg("classification: probing the queue failed")
			r.stop(err)
			break
		}
		if len(eligible) == 0 {
			break
		}
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
				return Stats{Skipped: true, Reason: "backend unavailable"}, nil
			}
		}
		candidates, err := w.db.ClaimClassification(
			ctx, w.version, time.Now(), wave, w.maxAttempts, w.claimLease)
		if err != nil {
			logger.Warn().Err(err).Msg("classification: claiming items failed")
			r.stop(err)
			break
		}
		if len(candidates) == 0 {
			break
		}
		r.addCandidates(len(candidates))
		before := r.progress()
		r.beginWave()
		w.processCandidates(ctx, classifier, candidates, cl, r)
		if r.done() {
			// A required write failed. Stop instead of looping over items the
			// pass cannot record outcomes for.
			break
		}
		if r.progress() == before {
			logger.Warn().Int("candidates", len(candidates)).
				Msg("classification: pass made no progress, stopping to avoid a loop")
			break
		}
	}
	stats := r.snapshot()
	event := logger.Info()
	if stats.Candidates == 0 {
		event = logger.Debug()
	}
	event.
		Int("candidates", stats.Candidates).
		Int("classified", stats.Classified).
		Int("failed", stats.Failed).
		Msg("classification pass complete")
	return stats, r.failure()
}

// processCandidates classifies the claimed items with at most
// max_parallel batches in flight. A max_parallel of one keeps the sequential
// path — one batch at a time, in claim order — that a single-request install
// expects; anything higher fans batches out while the semaphore holds the
// number of concurrent model requests at exactly the configured bound.
func (w *Worker) processCandidates(
	ctx context.Context,
	classifier Classifier,
	candidates []store.ClassificationCandidate,
	cl config.ClassificationConfig,
	r *batchRunner,
) {
	batchSize := max(cl.BatchSize, 1)
	maxParallel := max(cl.MaxParallel, 1)
	if maxParallel == 1 {
		for start := 0; start < len(candidates); start += batchSize {
			if ctx.Err() != nil || r.done() || r.claimLost() {
				return
			}
			w.processBatch(ctx, classifier,
				candidates[start:min(start+batchSize, len(candidates))], r)
		}
		return
	}
	sem := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup
feed:
	for start := 0; start < len(candidates); start += batchSize {
		// Acquire before spawning: both the in-flight requests and the
		// goroutines stay bounded, and a cancelled context stops the feed
		// instead of queueing behind the semaphore.
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			break feed
		}
		if r.done() || r.claimLost() {
			<-sem
			break feed
		}
		batch := candidates[start:min(start+batchSize, len(candidates))]
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			w.processBatch(ctx, classifier, batch, r)
		}()
	}
	wg.Wait()
}

func (w *Worker) processBatch(
	ctx context.Context,
	classifier Classifier,
	candidates []store.ClassificationCandidate,
	r *batchRunner,
) {
	logger := zerolog.Ctx(ctx)
	if len(candidates) == 1 {
		candidate := candidates[0]
		verdict, err := w.classifyOne(ctx, classifier, candidate.Item)
		if err != nil {
			w.recordFailure(ctx, candidate, err, r)
			return
		}
		w.storeVerdict(ctx, candidate, verdict, r)
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
		if ctx.Err() != nil || r.done() || r.claimLost() {
			return
		}
		verdict, ok := byID[candidate.Item.ID]
		if !ok {
			verdict, err = w.classifyOne(ctx, classifier, candidate.Item)
			if err != nil {
				w.recordFailure(ctx, candidate, err, r)
				continue
			}
		}
		w.storeVerdict(ctx, candidate, verdict, r)
	}
}

// storeVerdict persists one verdict under the claim that produced it, with the
// response chosen by the error's origin:
//
//   - store.ErrStaleClaim: ownership was lost. The verdict is discarded —
//     another worker owns the item and its result stands — without counting it
//     either way, and the claim's remaining items are abandoned.
//   - store.ErrInvalidTag: the model produced a tag the store refuses. An
//     ordinary model failure: recorded per item, retried on a later pass.
//   - context.Canceled / context.DeadlineExceeded: the pass was cut short
//     mid-write. Not a model failure and not counted; stopping is the response.
//   - anything else: a persistence failure. The verdict exists but cannot be
//     recorded, and recording that as an ordinary model failure — which a
//     still-working failure mark would happily do — would let a broken
//     database pass as a completed pass. The pass stops and the error reaches
//     the caller; classifications already committed stand.
func (w *Worker) storeVerdict(
	ctx context.Context,
	candidate store.ClassificationCandidate,
	verdict ItemVerdict,
	r *batchRunner,
) {
	logger := zerolog.Ctx(ctx)
	_, err := w.db.ReplaceItemTags(
		ctx, candidate.Item.ID, w.version, candidate.ClaimToken,
		verdict.Keywords, verdict.Assumptions)
	switch {
	case err == nil:
		r.record(1, 0)
	case errors.Is(err, store.ErrStaleClaim):
		logger.Debug().Str("item", candidate.Item.ID).
			Msg("classification: claim lost before the verdict was stored, discarding it")
		r.loseClaim()
	case errors.Is(err, store.ErrInvalidTag):
		w.recordFailure(ctx, candidate, err, r)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		logger.Debug().Str("item", candidate.Item.ID).Err(err).
			Msg("classification: verdict write cut short by the pass ending")
		r.stop(err)
	default:
		logger.Error().Err(err).Str("item", candidate.Item.ID).
			Msg("classification: storing the verdict failed, stopping the pass")
		r.stop(err)
	}
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

// recordFailure persists one item's failure under the claim that produced it.
// The item only counts as failed once the write lands: an unpersisted failure
// is not progress, or a pass against a broken database would claim the same
// items forever. A stale claim discards the failure the same way a stale
// verdict is discarded, and abandons the claim's remaining items.
func (w *Worker) recordFailure(
	ctx context.Context,
	candidate store.ClassificationCandidate,
	err error,
	r *batchRunner,
) {
	logger := zerolog.Ctx(ctx)
	attempts := candidate.Attempts + 1
	next := time.Now().Add(w.slowRetry)
	if attempts >= w.maxAttempts {
		// Terminal for the queue: only a version bump or a manual retry resets it.
		next = time.Now().Add(365 * 24 * time.Hour)
	}
	markErr := w.db.MarkClassificationFailed(
		ctx, candidate.Item.ID, w.version, attempts, err.Error(), next, candidate.ClaimToken)
	switch {
	case markErr == nil:
		logger.Warn().Str("item", truncate(candidate.Item.Title, 80)).Int("attempts", attempts).
			Err(err).Msg("classification failed after retries")
		r.record(0, 1)
	case errors.Is(markErr, store.ErrStaleClaim):
		logger.Debug().Str("item", candidate.Item.ID).
			Msg("classification: claim lost before the failure was recorded, discarding it")
		r.loseClaim()
	default:
		logger.Error().Err(markErr).Str("item", candidate.Item.ID).
			Msg("classification: recording the failure failed, stopping the pass")
		r.stop(markErr)
	}
}

// logResult reports a background pass's outcome. A pass stopped by a database
// failure is an error worth an operator's attention — its items are still
// queued, but nothing is classifying them — while a skipped pass is routine.
func (w *Worker) logResult(ctx context.Context, stats Stats, err error) {
	logger := zerolog.Ctx(ctx)
	if err != nil {
		logger.Error().Err(err).Msg("classification pass stopped by a failure")
		return
	}
	if stats.Skipped {
		logger.Debug().Str("reason", stats.Reason).Msg("classification pass skipped")
	}
}
