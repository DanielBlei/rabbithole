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
)

const (
	// maxConcurrentGenerations bounds model and network work across items.
	maxConcurrentGenerations = 2
	// GenerationTimeout bounds one dive from fetch through synthesis.
	GenerationTimeout = 7 * time.Minute
	// errorDetailLimit bounds the stored diagnostic string.
	errorDetailLimit = 500
)

// Generator is the one model call a brief makes.
type Generator interface {
	ChatJSON(ctx context.Context, system, user string, schema json.RawMessage, maxTokens int) (string, error)
}

// GeneratorFunc resolves the active provider for one generation.
type GeneratorFunc func(ctx context.Context) (Generator, error)

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

// Options sizes one generation and records its provenance.
type Options struct {
	MaxArticleBytes int
	MaxTextBytes    int
	MaxOutputTokens int
	FetchTimeout    time.Duration
	Provider        string
	Model           string
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

	sem chan struct{}

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
		fetch:    articlefetch.Fetch,
		sem:      make(chan struct{}, maxConcurrentGenerations),
		running:  map[string]*run{},
		failures: map[string]failure{},
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
	previous, _ := s.store.ItemBrief(ctx, itemID)
	s.mu.Lock()
	_, retrying := s.failures[itemID]
	s.mu.Unlock()
	if !refresh && !retrying && previous.Status == store.ItemBriefReady {
		return snapshotFromBrief(previous), nil
	}

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
	delete(s.failures, itemID)
	r := &run{phase: PhaseQueued}
	if previous.Status == store.ItemBriefReady {
		r.previous = &previous
	}
	s.running[itemID] = r
	s.mu.Unlock()

	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), GenerationTimeout)
	go func() {
		defer cancel()
		s.generate(bg, item, r, opts.withDefaults())
	}()
	return Snapshot{ItemID: itemID, Status: StatusQueued, Phase: PhaseQueued, Brief: previousIfReady(previous)}, nil
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
	briefs, err := s.store.ItemBriefsByItemIDs(ctx, itemIDs)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Snapshot, len(itemIDs))
	s.mu.Lock()
	defer s.mu.Unlock()
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

func (s *Service) generate(ctx context.Context, item store.ItemRow, r *run, opts Options) {
	defer func() {
		s.mu.Lock()
		if s.running[item.ID] == r {
			delete(s.running, item.ID)
		}
		s.mu.Unlock()
	}()

	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		s.fail(ctx, item, r, opts, CodeTimeout, ctx.Err())
		return
	}

	logger := zerolog.Ctx(ctx).With().Str("item", item.ID).Logger()
	prof, err := s.profiles.Resolve(ctx)
	if err != nil {
		s.fail(ctx, item, r, opts, CodeAnalysisFailed, fmt.Errorf("resolve profile: %w", err))
		return
	}
	gen, err := s.generator(ctx)
	if err != nil {
		code := CodeAnalysisFailed
		if errors.Is(err, ErrBriefUnavailable) {
			code = CodeProviderUnavailable
		}
		s.fail(ctx, item, r, opts, code, err)
		return
	}

	s.setPhase(r, PhaseFetching)
	logger.Debug().Str("link", item.Link).Msg("decision brief: fetching article")
	res, err := s.fetch(ctx, item.Link, articlefetch.Limits{
		MaxArticleBytes: int64(opts.MaxArticleBytes),
		Timeout:         opts.FetchTimeout,
	})
	if err != nil {
		s.fail(ctx, item, r, opts, fetchCode(err), err)
		return
	}
	body, err := io.ReadAll(res.Body)
	closeErr := res.Body.Close()
	if err != nil {
		s.fail(ctx, item, r, opts, fetchCode(err), err)
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
		}
		s.fail(ctx, item, r, opts, code, err)
		return
	}
	prepared, err := articletext.Prepare(art.Text, opts.MaxTextBytes)
	if err != nil {
		s.fail(ctx, item, r, opts, CodeEncoding, err)
		return
	}
	prepared.Truncated = prepared.Truncated || art.Truncated
	title := art.Title
	if strings.TrimSpace(title) == "" {
		title = item.Title
	}

	feedScore := item.LLMScore
	userPrompt := BuildUserPrompt(prof.Content, item, prepared, res.FinalURL, feedScore)
	s.setPhase(r, PhaseAnalyzing)
	logger.Debug().Int("words", prepared.WordCount).Bool("truncated", prepared.Truncated).
		Msg("decision brief: analyzing full text")

	brief, code, err := s.analyze(ctx, gen, userPrompt, opts)
	if err != nil {
		s.fail(ctx, item, r, opts, code, err)
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
		FinalURL:       res.FinalURL,
		FeedScore:      feedScore,
		ProfileID:      prof.ID,
		ProfileName:    prof.Name,
		ProfileHash:    prof.Hash,
		Provider:       opts.Provider,
		Model:          opts.Model,
		CreatedAt:      time.Now().UTC(),
	}
	if err := s.store.SaveItemBrief(ctx, b); err != nil {
		logger.Warn().Err(err).Msg("decision brief: storing result failed")
		s.recordFailure(item.ID, CodeAnalysisFailed, err.Error(), r.previous)
		return
	}
	logger.Debug().Str("title", truncateRunes(title, 80)).Str("recommendation", brief.Recommendation).
		Int("score", brief.Score).Msg("decision brief: ready")
}

// analyze makes one model call and retries once on any call or validation
// failure. The returned code is the stable product failure code on error.
func (s *Service) analyze(ctx context.Context, gen Generator, userPrompt string, opts Options) (Brief, string, error) {
	raw, err := gen.ChatJSON(ctx, SystemPrompt, userPrompt, Schema(), opts.MaxOutputTokens)
	if err == nil {
		brief, parseErr := ParseBrief(raw)
		if parseErr == nil {
			return brief, "", nil
		}
		err = parseErr
	}
	zerolog.Ctx(ctx).Warn().Err(err).Msg("decision brief: model output unusable, retrying once")

	raw, retryErr := gen.ChatJSON(ctx, SystemPrompt, userPrompt, Schema(), opts.MaxOutputTokens)
	if retryErr != nil {
		return Brief{}, CodeAnalysisFailed, fmt.Errorf("model call on retry: %w", retryErr)
	}
	brief, parseErr := ParseBrief(raw)
	if parseErr != nil {
		return Brief{}, CodeInvalidOutput, fmt.Errorf("model output invalid after retry: %w", parseErr)
	}
	return brief, "", nil
}

// fail records a terminal failure. A failed refresh never replaces a ready
// brief; the failure stays in memory so the UI can offer the old brief.
func (s *Service) fail(ctx context.Context, item store.ItemRow, r *run, opts Options, code string, cause error) {
	detail := truncateRunes(cause.Error(), errorDetailLimit)
	zerolog.Ctx(ctx).Warn().Err(cause).Str("item", item.ID).Str("code", code).
		Msg("decision brief failed")
	s.recordFailure(item.ID, code, detail, r.previous)
	if r.previous != nil && r.previous.Status == store.ItemBriefReady {
		return
	}
	b := store.ItemBrief{
		ItemID:      item.ID,
		Status:      store.ItemBriefFailed,
		ErrorCode:   code,
		ErrorDetail: detail,
		Provider:    opts.Provider,
		Model:       opts.Model,
		CreatedAt:   time.Now().UTC(),
	}
	if err := s.store.SaveItemBrief(ctx, b); err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Str("item", item.ID).
			Msg("decision brief: storing failure failed")
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
	case CodeAnalysisFailed:
		return "The analysis call failed. Try again later."
	default:
		return "The analysis call failed. Try again later."
	}
}
