// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/DanielBlei/rabbithole/internal/briefing"
	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/inference"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// briefingPageData is the full page model: where the viewer is in period
// history plus the panel that a generate/status swap replaces.
type briefingPageData struct {
	Title      string
	Active     string
	Chrome     chromeData
	PromptUser string

	Kind        string
	Daily       bool
	Weekly      bool
	PeriodStart string
	PeriodLabel string
	PrevStart   string
	NextStart   string
	HasNext     bool
	Panel       briefingPanel
}

// briefingPanel is the stored-briefing product state. Generating, Ready and
// Failed are the only terminal/poll states.
type briefingPanel struct {
	Kind        string
	PeriodStart string
	PeriodLabel string

	HasBriefing bool
	Generating  bool
	Ready       bool
	Failed      bool
	Status      string

	BodyHTML template.HTML
	Error    string

	ProfileName string
	ProfileHash string
	Provider    string
	Model       string
	ItemCount   int

	ExportURL   string
	ActionLabel string
}

func (s *Web) handleBriefing(w http.ResponseWriter, r *http.Request) {
	period, err := briefingPeriod(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	stored, err := s.db.BriefingForPeriod(r.Context(), period.Kind, period.Start)
	if err != nil && !errors.Is(err, store.ErrBriefingNotFound) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	last, err := briefing.PeriodFor(period.Kind, time.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	next := period.Next()
	data := briefingPageData{
		Title:       "Briefing",
		Active:      "briefing",
		Chrome:      s.chrome(r.Context()),
		PromptUser:  s.pagePromptUser(r.Context()),
		Kind:        period.Kind,
		Daily:       period.Kind == "daily",
		Weekly:      period.Kind == "weekly",
		PeriodStart: period.Start.Format("2006-01-02"),
		PeriodLabel: briefingPeriodLabel(period),
		PrevStart:   period.Previous().Start.Format("2006-01-02"),
		NextStart:   next.Start.Format("2006-01-02"),
		HasNext:     next.Start.Before(last.Start),
		Panel:       briefingPanelFor(period, stored),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := briefingTmpl.ExecuteTemplate(w, "layout", data); err != nil {
		log.Error().Err(err).Msg("render briefing")
	}
}

func (s *Web) handleBriefingGenerate(w http.ResponseWriter, r *http.Request) {
	period, err := briefingPeriod(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	svc, cfg, err := s.briefingService(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	opts := briefingOptions(cfg)
	if _, err := svc.Start(r.Context(), period.Kind, period.Start, opts); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	stored, err := s.db.BriefingForPeriod(r.Context(), period.Kind, period.Start)
	if err != nil && !errors.Is(err, store.ErrBriefingNotFound) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := briefingTmpl.ExecuteTemplate(w, "briefingPanel", briefingPanelFor(period, stored)); err != nil {
		log.Error().Err(err).Msg("render briefing generating panel")
	}
}

func (s *Web) handleBriefingStatus(w http.ResponseWriter, r *http.Request) {
	period, err := briefingPeriod(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	stored, err := s.db.BriefingForPeriod(r.Context(), period.Kind, period.Start)
	if err != nil && !errors.Is(err, store.ErrBriefingNotFound) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := briefingTmpl.ExecuteTemplate(w, "briefingPanel", briefingPanelFor(period, stored)); err != nil {
		log.Error().Err(err).Msg("render briefing status panel")
	}
}

func (s *Web) handleBriefingExport(w http.ResponseWriter, r *http.Request) {
	period, err := briefingPeriod(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	stored, err := s.db.BriefingForPeriod(r.Context(), period.Kind, period.Start)
	if errors.Is(err, store.ErrBriefingNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if stored.Status != store.BriefingReady || stored.BodyMD == "" {
		http.Error(w, "this briefing is not ready", http.StatusConflict)
		return
	}
	name := "rabbithole-briefing-" + period.Kind + "-" + period.Start.Format("2006-01-02") + ".md"
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	if _, err := w.Write([]byte(stored.BodyMD)); err != nil {
		log.Error().Err(err).Msg("write briefing export")
	}
}

// briefingPeriod reads and validates the kind/period query. A missing period is
// the last completed one, so /briefing lands on a useful briefing.
func briefingPeriod(r *http.Request) (briefing.Period, error) {
	kind := strings.TrimSpace(r.FormValue("kind"))
	if kind == "" {
		kind = "daily"
	}
	raw := strings.TrimSpace(r.FormValue("period"))
	if raw == "" {
		return briefing.PeriodFor(kind, time.Now())
	}
	start, err := time.ParseInLocation("2006-01-02", raw, time.UTC)
	if err != nil {
		return briefing.Period{}, err
	}
	return briefing.PeriodAt(kind, start)
}

func (s *Web) briefingService(ctx context.Context) (*briefing.Service, config.Config, error) {
	cfg, _, err := s.src.Active(ctx)
	if err != nil {
		return nil, config.Config{}, err
	}
	if s.briefingGen != nil {
		gen := func(gctx context.Context) (briefing.Generator, error) {
			return s.briefingGen(gctx, cfg)
		}
		return briefing.New(s.db, s.profiles, gen), cfg, nil
	}
	think := cfg.Inference.ThinkEnabled()
	gen := func(gctx context.Context) (briefing.Generator, error) {
		return inference.ResolveBriefing(gctx, cfg.Inference, think)
	}
	return briefing.New(s.db, s.profiles, gen), cfg, nil
}

func briefingOptions(cfg config.Config) briefing.Options {
	b := cfg.Briefing.WithDefaults()
	return briefing.Options{
		MaxArticles:      b.MaxArticles,
		MaxDevelopments:  b.MaxDevelopments,
		RecommendedReads: b.RecommendedReads,
		Provider:         cfg.Inference.Provider,
		Model:            cfg.Inference.Model,
	}
}

func briefingPanelFor(period briefing.Period, b store.Briefing) briefingPanel {
	p := briefingPanel{
		Kind:        period.Kind,
		PeriodStart: period.Start.Format("2006-01-02"),
		PeriodLabel: briefingPeriodLabel(period),
		Status:      b.Status,
		ActionLabel: "Generate briefing",
	}
	if b.ID == "" {
		return p
	}
	p.HasBriefing = true
	p.ProfileName = b.ProfileName
	p.ProfileHash = shortHash(b.ProfileHash)
	p.Provider = b.Provider
	p.Model = b.Model
	p.ItemCount = b.ItemCount
	p.ExportURL = "/briefing/export?kind=" + period.Kind + "&period=" + p.PeriodStart
	switch b.Status {
	case store.BriefingGenerating:
		p.Generating = true
		p.ActionLabel = "Generating…"
	case store.BriefingReady:
		p.Ready = true
		p.BodyHTML = renderMarkdown(b.BodyMD)
		p.ActionLabel = "Regenerate"
	case store.BriefingFailed:
		p.Failed = true
		p.Error = b.Error
		p.ActionLabel = "Try again"
	}
	return p
}

func briefingPeriodLabel(p briefing.Period) string {
	if p.Kind == "weekly" {
		return "Week of " + p.Start.Format("Monday, 2 January 2006")
	}
	return p.Start.Format("Monday, 2 January 2006")
}

// SetBriefingGenerator overrides the briefing generator, for handler tests and
// local harnesses. Production leaves it nil and resolves the configured provider.
func (s *Web) SetBriefingGenerator(fn func(context.Context, config.Config) (briefing.Generator, error)) {
	s.briefingGen = fn
}
