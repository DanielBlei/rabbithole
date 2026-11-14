// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

// Package decisionbrief generates and persists an on-demand full-text Decision
// Brief for one feed item. Feeding and scoring never call into this package;
// only an explicit user dive starts a fetch or a model call.
package decisionbrief

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/DanielBlei/rabbithole/internal/articleextract"
	"github.com/DanielBlei/rabbithole/internal/articlefetch"
	"github.com/DanielBlei/rabbithole/internal/articletext"
	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/profile"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// ErrBriefUnavailable means the active inference provider cannot read full
// text (the heuristic scorer). Callers render it as a product state.
var ErrBriefUnavailable = errors.New("decision brief needs a model provider")

// ErrBriefBusy means the bounded generation queue is full. Start returns it
// immediately instead of accepting work it cannot schedule, so a burst of
// requests cannot create an unbounded number of waiting goroutines. Callers
// render it as a retryable busy state; no run or persisted state is created.
var ErrBriefBusy = errors.New("decision brief capacity is full")

// Snapshot statuses. queued and running live in memory only; ready and failed
// are persisted.
const (
	StatusIdle    = "idle"
	StatusQueued  = "queued"
	StatusRunning = "running"
	StatusReady   = store.ItemBriefReady
	StatusFailed  = store.ItemBriefFailed
)

// Generation phases shown while a dive is running.
const (
	PhaseQueued    = "queued"
	PhaseFetching  = "fetching"
	PhaseAnalyzing = "analyzing"
)

// Stable product failure codes. They are persisted with a failed brief and
// mapped to user-facing text by the web layer.
const (
	CodeNoLink                 = "no_link"
	CodeFetchBlocked           = "fetch_blocked"
	CodeTooManyRedirects       = "too_many_redirects"
	CodeTimeout                = "timeout"
	CodeTooLarge               = "too_large"
	CodeUnsupportedContentType = "unsupported_content_type"
	CodeEncoding               = "encoding"
	CodeInsufficientContent    = "insufficient_content"
	CodeProviderUnavailable    = "provider_unavailable"
	CodeAnalysisFailed         = "analysis_failed"
	CodeInvalidOutput          = "invalid_output"
	CodeStorageFailed          = "storage_failed"
)

const (
	// maxConcurrentGenerations bounds model and network work across items.
	maxConcurrentGenerations = 2
	// maxQueuedGenerations bounds accepted work that is waiting for an active
	// slot. Start admits at most active+queued dives and rejects the rest with
	// ErrBriefBusy, so a request burst cannot spawn unbounded goroutines.
	maxQueuedGenerations = 16
	// GenerationTimeout bounds one dive from fetch through synthesis.
	GenerationTimeout = 7 * time.Minute
	// failurePersistTimeout bounds the separate context used to record a
	// terminal failure after the generation context has expired.
	failurePersistTimeout = 5 * time.Second
	// resultPersistTimeout bounds the separate context used to store a fully
	// validated result after inference, so a generation deadline that expires
	// during the write cannot discard the result.
	resultPersistTimeout = 5 * time.Second
	// errorDetailLimit bounds the stored diagnostic string.
	errorDetailLimit = 500
)

// Generator is the one model call a brief makes.
type Generator interface {
	ChatJSON(ctx context.Context, system, user string, schema json.RawMessage, maxTokens int) (string, error)
}

// GeneratorFunc resolves the model client and its provenance for one
// generation. Both come from the same configuration snapshot, so a queued dive
// that outlives a config change stores the provider and model that actually
// ran, not the ones active when the request arrived.
type GeneratorFunc func(ctx context.Context) (Generator, Provenance, error)

// Provenance identifies the provider and model that performed a generation.
type Provenance struct {
	Provider string
	Model    string
}

// Fetcher retrieves one article. It is a seam so tests can supply a page
// without opening a network listener.
type Fetcher func(ctx context.Context, rawURL string, limits articlefetch.Limits) (articlefetch.Result, error)

// Store is the persistence surface a Service needs.
type Store interface {
	Get(ctx context.Context, identifier string) (store.ItemRow, error)
	SaveItemBrief(ctx context.Context, b store.ItemBrief) error
	ItemBrief(ctx context.Context, itemID string) (store.ItemBrief, error)
	ItemBriefsByItemIDs(ctx context.Context, ids []string) (map[string]store.ItemBrief, error)
}

// ProfileResolver resolves the active interest profile.
type ProfileResolver interface {
	Resolve(ctx context.Context) (profile.Snapshot, error)
}

// Options sizes one generation. Provider and model provenance is resolved with
// the generator, not captured here, so it always describes the client that ran.
type Options struct {
	MaxArticleBytes int
	MaxTextBytes    int
	MaxOutputTokens int
	FetchTimeout    time.Duration
}

func (o Options) withDefaults() Options {
	cfg := config.DecisionBriefConfig{
		MaxArticleBytes: o.MaxArticleBytes,
		MaxTextBytes:    o.MaxTextBytes,
		MaxOutputTokens: o.MaxOutputTokens,
	}.WithDefaults()
	o.MaxArticleBytes = cfg.MaxArticleBytes
	o.MaxTextBytes = cfg.MaxTextBytes
	o.MaxOutputTokens = cfg.MaxOutputTokens
	if o.FetchTimeout <= 0 {
		o.FetchTimeout = articlefetch.DefaultTimeout
	}
	return o
}

// Snapshot is the full UI state of one item's brief.
type Snapshot struct {
	ItemID      string
	Status      string
	Phase       string
	ErrorCode   string
	ErrorDetail string
	Brief       store.ItemBrief
}

// Service owns the per-item lifecycle. A dive is single-flight per item and
// at most maxConcurrentGenerations run at once.
type Service struct {
	store     Store
	profiles  ProfileResolver
	generator GeneratorFunc
	fetch     Fetcher

	sem   chan struct{}
	queue chan struct{}

	// generationTimeout is GenerationTimeout in production. Tests shorten it to
	// reach the terminal deadline deterministically.
	generationTimeout time.Duration

	mu       sync.Mutex
	running  map[string]*run
	failures map[string]failure
}

type run struct {
	phase    string
	previous *store.ItemBrief
}

type failure struct {
	code     string
	detail   string
	previous *store.ItemBrief
}

// New returns a Service over st, resolving profiles and generators per dive.
func New(st Store, profiles ProfileResolver, generator GeneratorFunc) *Service {
	return &Service{
		store: st, profiles: profiles, generator: generator,
		fetch:             articlefetch.Fetch,
		sem:               make(chan struct{}, maxConcurrentGenerations),
		queue:             make(chan struct{}, maxQueuedGenerations),
		generationTimeout: GenerationTimeout,
		running:           map[string]*run{},
		failures:          map[string]failure{},
	}
}

// SetFetcher replaces the article fetcher. It exists for tests.
func (s *Service) SetFetcher(f Fetcher) { s.fetch = f }

// Start begins a dive unless refresh is false and a ready brief is already
// stored. It returns immediately; Status or Statuses reports progress.
func (s *Service) Start(ctx context.Context, itemID string, refresh bool, opts Options) (Snapshot, error) {
	item, err := s.store.Get(ctx, itemID)
	if err != nil {
		return Snapshot{}, err
	}

	// The running check, the persisted read and the run insertion share one
	// critical section. A generation removes its run only after its result has
	// been saved, so a just-finished generation can never be mistaken for a
	// cache miss and start a duplicate dive. The store read is a single-row
	// lookup, not a network or model call.
	s.mu.Lock()
	if r, ok := s.running[itemID]; ok {
		s.mu.Unlock()
		snap := Snapshot{ItemID: itemID, Status: r.phase, Phase: r.phase}
		if r.phase == PhaseQueued {
			snap.Status = StatusQueued
		} else {
			snap.Status = StatusRunning
		}
		if r.previous != nil {
			snap.Brief = *r.previous
		}
		return snap, nil
	}
	previous, err := s.store.ItemBrief(ctx, itemID)
	if err != nil && !errors.Is(err, store.ErrItemBriefNotFound) {
		s.mu.Unlock()
		return Snapshot{}, err
	}
	_, retrying := s.failures[itemID]
	if !refresh && !retrying && previous.Status == store.ItemBriefReady {
		s.mu.Unlock()
		return snapshotFromBrief(previous), nil
	}
	// Admit the dive before creating a goroutine. The non-blocking send keeps
	// the bound exact: at most maxConcurrentGenerations active plus
	// maxQueuedGenerations waiting. A full queue is a product state, not an
	// error hidden in a goroutine that would block forever.
	select {
	case s.queue <- struct{}{}:
	default:
		s.mu.Unlock()
		return Snapshot{}, ErrBriefBusy
	}
	delete(s.failures, itemID)
	r := &run{phase: PhaseQueued}
	if previous.Status == store.ItemBriefReady {
		r.previous = &previous
	}
	s.running[itemID] = r
	s.mu.Unlock()

	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.timeout())
	go func() {
		defer cancel()
		s.runWorker(bg, item, r, opts.withDefaults())
	}()
	return Snapshot{ItemID: itemID, Status: StatusQueued, Phase: PhaseQueued, Brief: previousIfReady(previous)}, nil
}

// timeout is the generation budget, defaulting to GenerationTimeout for a
// Service constructed without New (tests set the field directly).
func (s *Service) timeout() time.Duration {
	if s.generationTimeout > 0 {
		return s.generationTimeout
	}
	return GenerationTimeout
}

// Status reports one item's current state.
func (s *Service) Status(ctx context.Context, itemID string) (Snapshot, error) {
	got, err := s.Statuses(ctx, []string{itemID})
	if err != nil {
		return Snapshot{}, err
	}
	return got[itemID], nil
}

// Statuses batch-loads stored briefs and overlays in-memory running and failed
// states, so a feed page costs one query rather than one per row.
func (s *Service) Statuses(ctx context.Context, itemIDs []string) (map[string]Snapshot, error) {
	// The persisted read and the in-memory overlay are one critical section. A
	// generation saves its result before it removes its run, and removal needs
	// this same lock; therefore a terminal snapshot can never be built from a
	// read taken before a completed run's save. The query is an indexed
	// single-table read, not a network or model call.
	s.mu.Lock()
	defer s.mu.Unlock()
	briefs, err := s.store.ItemBriefsByItemIDs(ctx, itemIDs)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Snapshot, len(itemIDs))
	for _, id := range itemIDs {
		snap := Snapshot{ItemID: id, Status: StatusIdle}
		if b, ok := briefs[id]; ok {
			snap = snapshotFromBrief(b)
		}
		if f, ok := s.failures[id]; ok {
			snap.Status = StatusFailed
			snap.Phase = ""
			snap.ErrorCode = f.code
			snap.ErrorDetail = f.detail
			if f.previous != nil {
				snap.Brief = *f.previous
			}
		}
		if r, ok := s.running[id]; ok {
			snap.Status = StatusRunning
			snap.Phase = r.phase
			if r.phase == PhaseQueued {
				snap.Status = StatusQueued
			}
			if r.previous != nil {
				snap.Brief = *r.previous
			}
		}
		out[id] = snap
	}
	return out, nil
}

func previousIfReady(b store.ItemBrief) store.ItemBrief {
	if b.Status == store.ItemBriefReady {
		return b
	}
	return store.ItemBrief{}
}

func snapshotFromBrief(b store.ItemBrief) Snapshot {
	return Snapshot{
		ItemID: b.ItemID, Status: b.Status, ErrorCode: b.ErrorCode,
		ErrorDetail: b.ErrorDetail, Brief: b,
	}
}

func (s *Service) setPhase(r *run, phase string) {
	s.mu.Lock()
	r.phase = phase
	s.mu.Unlock()
}

// runWorker owns one admitted dive from the queue through the active-slot
// wait and the run's lifetime. The queue token is held only while waiting, so
// it bounds accepted-but-waiting work; once a slot is acquired it is released
// and the active semaphore bounds network and model work. Both tokens are
// released exactly once on every path, including the generation deadline while
// still queued.
func (s *Service) runWorker(ctx context.Context, item store.ItemRow, r *run, opts Options) {
	defer func() {
		s.mu.Lock()
		if s.running[item.ID] == r {
			delete(s.running, item.ID)
		}
		s.mu.Unlock()
	}()

	select {
	case s.sem <- struct{}{}:
		<-s.queue
		defer func() { <-s.sem }()
		s.generate(ctx, item, r, opts)
	case <-ctx.Done():
		<-s.queue
		s.fail(ctx, item, r, opts, Provenance{}, CodeTimeout, ctx.Err())
	}
}

func (s *Service) generate(ctx context.Context, item store.ItemRow, r *run, opts Options) {
	var prov Provenance
	logger := zerolog.Ctx(ctx).With().Str("item", item.ID).Logger()
	prof, err := s.profiles.Resolve(ctx)
	if err != nil {
		s.fail(ctx, item, r, opts, prov, CodeAnalysisFailed, fmt.Errorf("resolve profile: %w", err))
		return
	}
	gen, resolved, err := s.generator(ctx)
	if err != nil {
		code := CodeAnalysisFailed
		if errors.Is(err, ErrBriefUnavailable) {
			code = CodeProviderUnavailable
		}
		s.fail(ctx, item, r, opts, prov, code, err)
		return
	}
	prov = resolved

	s.setPhase(r, PhaseFetching)
	// A feed link can carry userinfo. It must not reach a log line.
	logger.Debug().Str("link", articlefetch.WithoutUserinfo(item.Link)).
		Msg("decision brief: fetching article")
	res, err := s.fetch(ctx, item.Link, articlefetch.Limits{
		MaxArticleBytes: int64(opts.MaxArticleBytes),
		Timeout:         opts.FetchTimeout,
	})
	if err != nil {
		s.fail(ctx, item, r, opts, prov, fetchCode(err), err)
		return
	}
	body, err := io.ReadAll(res.Body)
	closeErr := res.Body.Close()
	if err != nil {
		s.fail(ctx, item, r, opts, prov, fetchCode(err), err)
		return
	}
	if closeErr != nil {
		logger.Debug().Err(closeErr).Msg("decision brief: closing article body")
	}

	art, err := articleextract.Extract(bytes.NewReader(body), res.ContentType, opts.MaxTextBytes)
	if err != nil {
		code := CodeAnalysisFailed
		switch {
		case errors.Is(err, articleextract.ErrEncoding):
			code = CodeEncoding
		case errors.Is(err, articleextract.ErrInsufficientContent):
			code = CodeInsufficientContent
		case errors.Is(err, articleextract.ErrInputTooLarge):
			code = CodeTooLarge
		}
		s.fail(ctx, item, r, opts, prov, code, err)
		return
	}
	prepared, err := articletext.Prepare(art.Text, opts.MaxTextBytes)
	if err != nil {
		s.fail(ctx, item, r, opts, prov, CodeEncoding, err)
		return
	}
	prepared.Truncated = prepared.Truncated || art.Truncated
	title := art.Title
	if strings.TrimSpace(title) == "" {
		title = item.Title
	}

	feedScore := item.LLMScore
	// The fetcher strips userinfo from a redirect chain, but a custom Fetcher or
	// an older stored value could still carry it; sanitize once more before the
	// URL enters the prompt or the persisted brief.
	finalURL := articlefetch.WithoutUserinfo(res.FinalURL)
	userPrompt := BuildUserPrompt(prof.Content, item, prepared, finalURL, feedScore)
	s.setPhase(r, PhaseAnalyzing)
	logger.Debug().Int("words", prepared.WordCount).Bool("truncated", prepared.Truncated).
		Msg("decision brief: analyzing full text")

	brief, code, err := s.analyze(ctx, gen, userPrompt, opts)
	if err != nil {
		s.fail(ctx, item, r, opts, prov, code, err)
		return
	}

	score := brief.Score
	b := store.ItemBrief{
		ItemID:         item.ID,
		Status:         store.ItemBriefReady,
		Score:          &score,
		Recommendation: brief.Recommendation,
		Why:            brief.Why,
		KeyPoints:      brief.KeyPoints,
		Novelty:        brief.Novelty,
		Caveats:        brief.Caveats,
		Focus:          brief.Focus,
		ReadingMinutes: prepared.ReadingMinutes,
		WordCount:      prepared.WordCount,
		Truncated:      prepared.Truncated,
		ContentHash:    textHash(prepared.Text),
		FinalURL:       finalURL,
		FeedScore:      feedScore,
		ProfileID:      prof.ID,
		ProfileName:    prof.Name,
		ProfileHash:    prof.Hash,
		Provider:       prov.Provider,
		Model:          prov.Model,
		CreatedAt:      time.Now().UTC(),
	}
	if s.persistResult(ctx, item, r, opts, prov, b) {
		logger.Debug().Str("title", truncateRunes(title, 80)).Str("recommendation", brief.Recommendation).
			Int("score", brief.Score).Msg("decision brief: ready")
	}
}

// persistResult stores a fully validated brief with a bounded context that is
// independent of the generation deadline. Inference may finish just as the
// generation context expires; the result is still worth keeping. A failed write
// goes through the established terminal-failure path so a storage error is
// reported as storage_failed (and a write deadline as timeout), not downgraded
// to an in-memory model/analysis failure.
func (s *Service) persistResult(
	ctx context.Context,
	item store.ItemRow,
	r *run,
	opts Options,
	prov Provenance,
	b store.ItemBrief,
) bool {
	if s.superseded(item.ID, r) {
		zerolog.Ctx(ctx).Warn().Str("item", item.ID).
			Msg("decision brief: discarding result from a superseded run")
		return false
	}
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), resultPersistTimeout)
	defer cancel()
	if err := s.store.SaveItemBrief(saveCtx, b); err != nil {
		// The write is a storage operation; an ordinary database error is not a
		// model failure. A deadline still maps to the established timeout code.
		code := CodeStorageFailed
		if errors.Is(err, context.DeadlineExceeded) ||
			errors.Is(saveCtx.Err(), context.DeadlineExceeded) {
			code = CodeTimeout
		}
		zerolog.Ctx(ctx).Warn().
			Str("item", item.ID).Str("code", code).
			Str("error_class", storageErrorClass(err)).
			Str("error_type", fmt.Sprintf("%T", err)).
			Msg("decision brief: storing result failed; recording terminal failure")
		s.fail(ctx, item, r, opts, prov, code, err)
		return false
	}
	return true
}

// superseded reports whether a different run has replaced r. It is a
// best-effort guard for late writes: if the lock is busy, a reader or a Start
// is in progress, and single-flight guarantees r is still current. A new run
// can only be inserted after r is removed, which happens after the write
// returns, so skipping the check while the lock is busy cannot let a late
// result replace a newer one.
func (s *Service) superseded(itemID string, r *run) bool {
	if !s.mu.TryLock() {
		return false
	}
	defer s.mu.Unlock()
	return s.running[itemID] != nil && s.running[itemID] != r
}

// analyze makes one model call and retries once on any call or validation
// failure. The returned code is the stable product failure code on error. A
// terminal generation context is never retried or labeled a generic analysis
// failure: an expired deadline is the established timeout state, and an
// explicit cancellation is the generic analysis failure.
func (s *Service) analyze(ctx context.Context, gen Generator, userPrompt string, opts Options) (Brief, string, error) {
	raw, err := gen.ChatJSON(ctx, SystemPrompt, userPrompt, Schema(), opts.MaxOutputTokens)
	if err == nil {
		brief, parseErr := ParseBrief(raw)
		if parseErr == nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return Brief{}, contextFailureCode(ctxErr), fmt.Errorf("model call interrupted: %w", ctxErr)
			}
			return brief, "", nil
		}
		err = parseErr
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return Brief{}, contextFailureCode(ctxErr), fmt.Errorf("model call interrupted: %w", ctxErr)
	}
	zerolog.Ctx(ctx).Warn().Err(err).Msg("decision brief: model output unusable, retrying once")

	raw, retryErr := gen.ChatJSON(ctx, SystemPrompt, userPrompt, Schema(), opts.MaxOutputTokens)
	if retryErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Brief{}, contextFailureCode(ctxErr), fmt.Errorf("model call on retry interrupted: %w", ctxErr)
		}
		return Brief{}, CodeAnalysisFailed, fmt.Errorf("model call on retry: %w", retryErr)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return Brief{}, contextFailureCode(ctxErr), fmt.Errorf("model call on retry interrupted: %w", ctxErr)
	}
	brief, parseErr := ParseBrief(raw)
	if parseErr != nil {
		return Brief{}, CodeInvalidOutput, fmt.Errorf("model output invalid after retry: %w", parseErr)
	}
	return brief, "", nil
}

// contextFailureCode maps a terminal generation context to the product code.
// The product model has one established timeout state; an explicit
// cancellation, which a generation context does not normally produce, falls
// back to the generic analysis failure.
func contextFailureCode(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return CodeTimeout
	}
	return CodeAnalysisFailed
}

// fail records a terminal failure. A failed refresh never replaces a ready
// brief; the failure stays in memory so the UI can offer the old brief.
func (s *Service) fail(
	ctx context.Context,
	item store.ItemRow,
	r *run,
	opts Options,
	prov Provenance,
	code string,
	cause error,
) {
	// A failure from a superseded run must not overwrite a newer generation's
	// result, in memory or in the store. Under the current single-flight
	// lifecycle the run is still current here, so this guard is defensive.
	if s.superseded(item.ID, r) {
		zerolog.Ctx(ctx).Warn().Str("item", item.ID).Str("code", code).
			Msg("decision brief: discarding failure from a superseded run")
		return
	}

	detail := truncateRunes(cause.Error(), errorDetailLimit)
	logEvent := zerolog.Ctx(ctx).Warn()
	if code == CodeStorageFailed {
		// Raw database errors can carry connection or credential details. Keep
		// the user-facing detail and the log free of them.
		detail = ""
		logEvent = logEvent.
			Str("error_class", storageErrorClass(cause)).
			Str("error_type", fmt.Sprintf("%T", cause))
	} else {
		logEvent = logEvent.Err(cause)
	}
	logEvent.Str("item", item.ID).Str("code", code).Msg("decision brief failed")
	s.recordFailure(item.ID, code, detail, r.previous)
	if r.previous != nil && r.previous.Status == store.ItemBriefReady {
		return
	}
	b := store.ItemBrief{
		ItemID:      item.ID,
		Status:      store.ItemBriefFailed,
		ErrorCode:   code,
		ErrorDetail: detail,
		Provider:    prov.Provider,
		Model:       prov.Model,
		CreatedAt:   time.Now().UTC(),
	}
	// The generation context is already expired in the timeout case. Record the
	// terminal state with a bounded context that preserves the logger values
	// but not the deadline, so a restart still sees the failure.
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), failurePersistTimeout)
	defer cancel()
	if err := s.store.SaveItemBrief(saveCtx, b); err != nil {
		zerolog.Ctx(ctx).Error().
			Str("item", item.ID).Str("code", code).
			Str("error_class", storageErrorClass(err)).
			Str("error_type", fmt.Sprintf("%T", err)).
			Msg("decision brief: storing terminal failure failed")
	}
}

// storageErrorClass labels a storage failure without copying its text; database
// errors can embed connection strings or credentials.
func storageErrorClass(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return "storage_error"
	}
}

func (s *Service) recordFailure(itemID, code, detail string, previous *store.ItemBrief) {
	s.mu.Lock()
	s.failures[itemID] = failure{code: code, detail: detail, previous: previous}
	s.mu.Unlock()
}

func fetchCode(err error) string {
	switch {
	case errors.Is(err, articlefetch.ErrNoLink):
		return CodeNoLink
	case errors.Is(err, articlefetch.ErrBlocked):
		return CodeFetchBlocked
	case errors.Is(err, articlefetch.ErrTooManyRedirects):
		return CodeTooManyRedirects
	case errors.Is(err, articlefetch.ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		return CodeTimeout
	case errors.Is(err, articlefetch.ErrTooLarge):
		return CodeTooLarge
	case errors.Is(err, articlefetch.ErrUnsupportedContentType):
		return CodeUnsupportedContentType
	case errors.Is(err, articleextract.ErrEncoding):
		return CodeEncoding
	case errors.Is(err, articleextract.ErrInsufficientContent):
		return CodeInsufficientContent
	default:
		return CodeAnalysisFailed
	}
}

func textHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// ProductMessage maps a stable failure code to the product-tone line the
// brief panel shows. Unknown codes fall back to the generic analysis failure.
func ProductMessage(code string) string {
	switch code {
	case CodeNoLink:
		return "This item has no link to fetch."
	case CodeFetchBlocked:
		return "Deep Dive unavailable: the destination was refused by the fetch policy."
	case CodeTooManyRedirects:
		return "Too many redirects while fetching the article."
	case CodeTimeout:
		return "The publisher took too long to respond."
	case CodeTooLarge:
		return "The article was too large to fetch safely."
	case CodeUnsupportedContentType:
		return "This page is not HTML and cannot be analyzed (PDFs are not supported yet)."
	case CodeEncoding:
		return "The page's character encoding could not be handled."
	case CodeInsufficientContent:
		return "Could not extract enough article text. Open the original article instead."
	case CodeProviderUnavailable:
		return "Deep Dive needs a model provider — the heuristic scorer cannot read full text."
	case CodeInvalidOutput:
		return "The model's answer was not usable. Try again."
	case CodeStorageFailed:
		return "Decision Brief couldn't be saved. Please try again."
	case CodeAnalysisFailed:
		return "The analysis call failed. Try again later."
	default:
		return "The analysis call failed. Try again later."
	}
}
