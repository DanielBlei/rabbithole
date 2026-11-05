// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

// Package briefing synthesizes a persisted, source-grounded daily or weekly
// briefing from already-scored articles.
package briefing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/digest"
	"github.com/DanielBlei/rabbithole/internal/profile"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// ErrBriefingUnavailable means the active inference provider cannot synthesize
// text (the heuristic scorer). Callers render it as a product state.
var ErrBriefingUnavailable = errors.New("briefing needs a model provider")

// ErrGenerationFailed wraps a generation that was persisted as failed.
var ErrGenerationFailed = errors.New("briefing generation failed")

const (
	// DefaultMaxOutputTokens caps one synthesis reply. It is a hard ceiling, not
	// an estimate: the adapter passes it as the provider's output limit.
	DefaultMaxOutputTokens = 4096
	// minMaxOutputTokens keeps a caller from accidentally capping the reply so
	// low that no valid briefing could fit.
	minMaxOutputTokens = 256
	// maxMaxOutputTokens bounds the configurable ceiling.
	maxMaxOutputTokens = 8192
	// summaryExcerpt bounds one article summary in the prompt.
	summaryExcerpt = 500
)

// Generator is the one model call a briefing makes.
type Generator interface {
	// ChatJSON sends one JSON-mode chat request and returns the raw content.
	ChatJSON(ctx context.Context, system, user string, schema json.RawMessage, maxTokens int) (string, error)
}

// GeneratorFunc resolves the active provider for one generation. It is called
// after the item set and profile are known, so an unavailable provider costs
// nothing.
type GeneratorFunc func(ctx context.Context) (Generator, error)

// Store is the persistence surface a Service needs.
type Store interface {
	BriefingItems(ctx context.Context, start, end time.Time, limit int) ([]store.BriefingItem, error)
	SaveBriefing(ctx context.Context, b store.Briefing) (store.Briefing, error)
	BriefingForPeriod(ctx context.Context, kind string, start time.Time) (store.Briefing, error)
}

// ProfileResolver resolves the active interest profile.
type ProfileResolver interface {
	Resolve(ctx context.Context) (profile.Snapshot, error)
}

// Options sizes one generation. Zero values take documented defaults.
type Options struct {
	Now              time.Time // clock for the last-completed period; zero uses time.Now
	MaxArticles      int
	MaxDevelopments  int
	RecommendedReads int
	MaxOutputTokens  int
	Provider         string // provenance recorded with the briefing
	Model            string // provenance recorded with the briefing
}

func (o Options) withDefaults() Options {
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	b := config.BriefingConfig{
		MaxArticles:      o.MaxArticles,
		MaxDevelopments:  o.MaxDevelopments,
		RecommendedReads: o.RecommendedReads,
	}.WithDefaults()
	o.MaxArticles = b.MaxArticles
	o.MaxDevelopments = b.MaxDevelopments
	o.RecommendedReads = b.RecommendedReads
	if o.MaxOutputTokens <= 0 {
		o.MaxOutputTokens = DefaultMaxOutputTokens
	}
	o.MaxOutputTokens = min(max(o.MaxOutputTokens, minMaxOutputTokens), maxMaxOutputTokens)
	return o
}

// Period is one half-open UTC reporting window.
type Period struct {
	Kind  string
	Start time.Time
	End   time.Time
}

// PeriodFor returns the last completed period relative to now.
func PeriodFor(kind string, now time.Time) (Period, error) {
	start, end, err := digest.PeriodBounds(kind, now)
	if err != nil {
		return Period{}, err
	}
	return Period{Kind: kind, Start: start, End: end}, nil
}

// PeriodAt returns the period beginning at start (normalized to UTC midnight).
func PeriodAt(kind string, start time.Time) (Period, error) {
	start = time.Date(start.UTC().Year(), start.UTC().Month(), start.UTC().Day(),
		0, 0, 0, 0, time.UTC)
	switch kind {
	case "daily":
		return Period{Kind: kind, Start: start, End: start.AddDate(0, 0, 1)}, nil
	case "weekly":
		// ISO weeks run Monday to Monday: a date picked mid-week names the week
		// that contains it, so switching daily -> weekly or stepping through
		// history always lands on a real period boundary.
		offset := (int(start.Weekday()) + 6) % 7
		start = start.AddDate(0, 0, -offset)
		return Period{Kind: kind, Start: start, End: start.AddDate(0, 0, 7)}, nil
	default:
		return Period{}, fmt.Errorf("unknown period %q (want daily or weekly)", kind)
	}
}

// Previous returns the period immediately before p.
func (p Period) Previous() Period {
	if p.Kind == "weekly" {
		return Period{Kind: p.Kind, Start: p.Start.AddDate(0, 0, -7), End: p.Start}
	}
	return Period{Kind: p.Kind, Start: p.Start.AddDate(0, 0, -1), End: p.Start}
}

// Next returns the period immediately after p.
func (p Period) Next() Period {
	if p.Kind == "weekly" {
		return Period{Kind: p.Kind, Start: p.End, End: p.End.AddDate(0, 0, 7)}
	}
	return Period{Kind: p.Kind, Start: p.End, End: p.End.AddDate(0, 0, 1)}
}

// Service generates and persists briefings. It allows one generation per
// (kind, period) at a time: a concurrent request receives the stored
// generating state instead of starting a second model call.
type Service struct {
	store     Store
	profiles  ProfileResolver
	generator GeneratorFunc

	mu      sync.Mutex
	running map[string]bool
}

// New returns a Service over store, resolving profiles and generators for each
// run.
func New(st Store, profiles ProfileResolver, generator GeneratorFunc) *Service {
	return &Service{store: st, profiles: profiles, generator: generator, running: map[string]bool{}}
}

// Generate synthesizes one briefing for periodStart and waits for the result.
// A zero start means the last completed period. The generating row is persisted
// before any model work, and exactly one terminal row is left behind.
func (s *Service) Generate(
	ctx context.Context,
	kind string,
	periodStart time.Time,
	opts Options,
) (store.Briefing, error) {
	state, b, err := s.begin(ctx, kind, periodStart, opts)
	if state == nil {
		return b, err
	}
	defer state.release()
	return s.synthesize(ctx, state, b)
}

// Start persists the generating row and returns immediately; synthesis runs in
// the background so the web request can render a generating fragment. The
// background call is detached from the request cancellation but keeps the
// request's logger values.
func (s *Service) Start(ctx context.Context, kind string, periodStart time.Time, opts Options) (store.Briefing, error) {
	state, b, err := s.begin(ctx, kind, periodStart, opts)
	if state == nil {
		return b, err
	}
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
	go func() {
		defer cancel()
		defer state.release()
		if _, err := s.synthesize(bg, state, b); err != nil {
			zerolog.Ctx(bg).Warn().Err(err).Str("period", b.PeriodKind).Msg("briefing generation failed")
		}
	}()
	return b, nil
}

type runState struct {
	period  Period
	profile profile.Snapshot
	items   []store.BriefingItem
	opts    Options
	release func()
}

// begin acquires the single flight for the period, resolves the profile and
// bounded item set, and persists the generating row. A nil state means another
// generation owns this period; b is then its current (or synthetic generating)
// row.
func (s *Service) begin(
	ctx context.Context,
	kind string,
	periodStart time.Time,
	opts Options,
) (*runState, store.Briefing, error) {
	opts = opts.withDefaults()
	var (
		period Period
		err    error
	)
	if periodStart.IsZero() {
		period, err = PeriodFor(kind, opts.Now)
	} else {
		period, err = PeriodAt(kind, periodStart)
	}
	if err != nil {
		return nil, store.Briefing{}, err
	}
	key := period.Kind + "|" + period.Start.Format(time.RFC3339)

	s.mu.Lock()
	if s.running[key] {
		s.mu.Unlock()
		b, err := s.store.BriefingForPeriod(ctx, period.Kind, period.Start)
		if errors.Is(err, store.ErrBriefingNotFound) {
			return nil, store.Briefing{
				PeriodKind: period.Kind, PeriodStart: period.Start, PeriodEnd: period.End,
				Status: store.BriefingGenerating,
			}, nil
		}
		return nil, b, err
	}
	s.running[key] = true
	s.mu.Unlock()
	release := func() {
		s.mu.Lock()
		delete(s.running, key)
		s.mu.Unlock()
	}

	prof, err := s.profiles.Resolve(ctx)
	if err != nil {
		release()
		return nil, store.Briefing{}, fmt.Errorf("resolve briefing profile: %w", err)
	}
	items, err := s.store.BriefingItems(ctx, period.Start, period.End, opts.MaxArticles)
	if err != nil {
		release()
		return nil, store.Briefing{}, fmt.Errorf("load briefing items: %w", err)
	}
	b := store.Briefing{
		ID:         briefingID(period),
		PeriodKind: period.Kind, PeriodStart: period.Start, PeriodEnd: period.End,
		Status:    store.BriefingGenerating,
		ProfileID: prof.ID, ProfileName: prof.Name, ProfileHash: prof.Hash,
		Provider: opts.Provider, Model: opts.Model,
		ItemCount: len(items),
	}
	saved, err := s.store.SaveBriefing(ctx, b)
	if err != nil {
		release()
		return nil, store.Briefing{}, err
	}
	return &runState{period: period, profile: prof, items: items, opts: opts, release: release}, saved, nil
}

func (s *Service) synthesize(ctx context.Context, state *runState, b store.Briefing) (store.Briefing, error) {
	period, prof, items, opts := state.period, state.profile, state.items, state.opts
	if len(items) == 0 {
		b.Status = store.BriefingReady
		b.Error = ""
		b.BodyMD = Render(period, nil, Briefing{})
		return s.finish(ctx, b)
	}

	gen, err := s.generator(ctx)
	if err != nil {
		return s.fail(ctx, b, err)
	}
	userPrompt := BuildUserPrompt(period, prof.Content, items)
	schema := Schema(opts.MaxDevelopments, opts.RecommendedReads)
	raw, err := gen.ChatJSON(ctx, SystemPrompt, userPrompt, schema, opts.MaxOutputTokens)
	if err != nil {
		return s.fail(ctx, b, fmt.Errorf("briefing model call: %w", err))
	}
	brief, err := Parse(raw, len(items), opts.MaxDevelopments, opts.RecommendedReads)
	if err != nil {
		// One retry, then the row is recorded as failed. Never partially store.
		raw, retryErr := gen.ChatJSON(ctx, SystemPrompt, userPrompt, schema, opts.MaxOutputTokens)
		if retryErr != nil {
			return s.fail(ctx, b, fmt.Errorf("briefing model call on retry: %w", retryErr))
		}
		brief, err = Parse(raw, len(items), opts.MaxDevelopments, opts.RecommendedReads)
		if err != nil {
			return s.fail(ctx, b, fmt.Errorf("briefing output invalid after retry: %w", err))
		}
	}

	b.Status = store.BriefingReady
	b.Error = ""
	b.ReferencedItemIDs = referencedIDs(brief, items)
	b.BodyMD = Render(period, items, brief)
	return s.finish(ctx, b)
}

func (s *Service) finish(ctx context.Context, b store.Briefing) (store.Briefing, error) {
	saved, err := s.store.SaveBriefing(ctx, b)
	if err != nil {
		return b, err
	}
	return saved, nil
}

func (s *Service) fail(ctx context.Context, b store.Briefing, cause error) (store.Briefing, error) {
	b.Status = store.BriefingFailed
	b.Error = cause.Error()
	b.BodyMD = ""
	b.ReferencedItemIDs = nil
	saved, err := s.store.SaveBriefing(ctx, b)
	if err != nil {
		return b, err
	}
	return saved, fmt.Errorf("%w: %w", ErrGenerationFailed, cause)
}

func briefingID(p Period) string {
	sum := sha256.Sum256([]byte(p.Kind + "|" + p.Start.UTC().Format(time.RFC3339)))
	return "briefing-" + hex.EncodeToString(sum[:12])
}

// referencedIDs maps the model's 1-based article references back to stable
// stored item IDs, preserving first-reference order without duplicates.
func referencedIDs(b Briefing, items []store.BriefingItem) []string {
	seen := map[int]bool{}
	var out []string
	add := func(n int) {
		if n < 1 || n > len(items) || seen[n] {
			return
		}
		seen[n] = true
		out = append(out, items[n-1].ID)
	}
	for _, d := range b.Developments {
		for _, n := range d.ItemIDs {
			add(n)
		}
	}
	for _, n := range b.RecommendedReading {
		add(n)
	}
	if out == nil {
		out = []string{}
	}
	return out
}

// Briefing is the validated model output.
type Briefing struct {
	ExecutiveSummary   string        `json:"executive_summary"`
	Developments       []Development `json:"developments"`
	EmergingTopics     []string      `json:"emerging_topics,omitempty"`
	RecommendedReading []int         `json:"recommended_reading"`
}

// Development is one synthesized key development, grounded in supplied items.
type Development struct {
	Headline     string `json:"headline"`
	Synthesis    string `json:"synthesis"`
	WhyItMatters string `json:"why_it_matters"`
	ItemIDs      []int  `json:"item_ids"`
}

// Parse strictly decodes and validates one model reply. maxItem is the number
// of supplied articles (their 1-based indexes), and the two caps bound the
// arrays even when a provider ignores the schema.
func Parse(raw string, maxItem, maxDevelopments, recommendedReads int) (Briefing, error) {
	body, err := extractJSON(raw)
	if err != nil {
		return Briefing{}, err
	}
	var b Briefing
	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		return Briefing{}, fmt.Errorf("decode briefing JSON: %w", err)
	}
	if err := b.validate(maxItem, maxDevelopments, recommendedReads); err != nil {
		return Briefing{}, err
	}
	return b, nil
}

func (b Briefing) validate(maxItem, maxDevelopments, recommendedReads int) error {
	if strings.TrimSpace(b.ExecutiveSummary) == "" {
		return errors.New("executive_summary is empty")
	}
	if len(b.Developments) == 0 {
		return errors.New("developments is empty")
	}
	if len(b.Developments) > maxDevelopments {
		return fmt.Errorf("developments has %d entries, cap is %d", len(b.Developments), maxDevelopments)
	}
	if len(b.RecommendedReading) > recommendedReads {
		return fmt.Errorf("recommended_reading has %d entries, cap is %d",
			len(b.RecommendedReading), recommendedReads)
	}
	for i, d := range b.Developments {
		if strings.TrimSpace(d.Headline) == "" || strings.TrimSpace(d.Synthesis) == "" {
			return fmt.Errorf("development %d is missing its headline or synthesis", i+1)
		}
		if len(d.ItemIDs) == 0 {
			return fmt.Errorf("development %d cites no supplied item", i+1)
		}
		if err := validateIDs(d.ItemIDs, maxItem, fmt.Sprintf("development %d", i+1)); err != nil {
			return err
		}
	}
	if err := validateIDs(b.RecommendedReading, maxItem, "recommended_reading"); err != nil {
		return err
	}
	for i, topic := range b.EmergingTopics {
		if strings.TrimSpace(topic) == "" {
			return fmt.Errorf("emerging topic %d is empty", i+1)
		}
	}
	return nil
}

func validateIDs(ids []int, maxItem int, label string) error {
	seen := map[int]bool{}
	for _, id := range ids {
		if id < 1 || id > maxItem {
			return fmt.Errorf("%s cites item %d, outside the %d supplied items", label, id, maxItem)
		}
		if seen[id] {
			return fmt.Errorf("%s cites item %d more than once", label, id)
		}
		seen[id] = true
	}
	return nil
}

// extractJSON accepts a bare object or one wrapped in a code fence/prose, which
// unconstrained providers sometimes emit.
func extractJSON(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if i := strings.Index(s, "```"); i >= 0 {
		s = s[i+3:]
		if j := strings.IndexByte(s, '\n'); j >= 0 {
			s = s[j+1:]
		}
		if j := strings.Index(s, "```"); j >= 0 {
			s = s[:j]
		}
	}
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end < start {
		return "", errors.New("model reply contains no JSON object")
	}
	return s[start : end+1], nil
}
