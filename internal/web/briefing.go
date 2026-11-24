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
	Cal         briefingCalendar
	CalLabel    string
}

// briefingPanel is the stored-briefing product state. Generating, Ready and
// Failed are the only terminal/poll states.
type briefingPanel struct {
	PromptUser  string
	Kind        string
	PeriodStart string
	PeriodLabel string

	HasBriefing bool
	Generating  bool
	Ready       bool
	Failed      bool
	Complete    bool
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
	// The service's state, not a bare store read: a run that has taken the
	// single flight but not persisted its row yet still renders as generating.
	stored, err := s.briefingSvc.State(r.Context(), period.Kind, period.Start)
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
	cal, err := s.calendarFor(r.Context(), period, monthOf(period.Start))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
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
		HasNext:     !next.Start.After(last.Start),
		Panel:       s.panelFor(r.Context(), period, stored),
		Cal:         cal,
		CalLabel:    map[bool]string{true: "Pick a week", false: "Pick a date"}[period.Kind == "weekly"],
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
	if period.End.After(time.Now()) {
		http.Error(w, "this period is not complete yet", http.StatusBadRequest)
		return
	}
	svc, cfg, err := s.briefingService(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	opts := briefingOptions(cfg)
	// The panel renders from Start's return value, never a store re-query: a
	// request that joins an in-flight generation gets the generating state
	// directly, even in the window before the owner has persisted its row.
	b, err := svc.Start(r.Context(), period.Kind, period.Start, opts)
	if err != nil {
		log.Error().Err(err).Msg("start briefing")
		// Keep showing a stored briefing: a failed start must not hide it.
		stored, _ := s.db.BriefingForPeriod(r.Context(), period.Kind, period.Start)
		p := s.panelFor(r.Context(), period, stored)
		if !p.Ready && !p.Generating {
			p.Failed = true
		}
		p.Error = "Could not start the briefing. Check the server log."
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := briefingTmpl.ExecuteTemplate(w, "briefingPanel", p); err != nil {
			log.Error().Err(err).Msg("render briefing start failure")
		}
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := briefingTmpl.ExecuteTemplate(w, "briefingPanel", s.panelFor(r.Context(), period, b)); err != nil {
		log.Error().Err(err).Msg("render briefing generating panel")
	}
}

func (s *Web) handleBriefingStatus(w http.ResponseWriter, r *http.Request) {
	period, err := briefingPeriod(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// The service's state, not a bare store read: a poll that lands during
	// the window before the owner's first write still renders the generating
	// panel instead of flipping to the empty state.
	stored, err := s.briefingSvc.State(r.Context(), period.Kind, period.Start)
	if err != nil && !errors.Is(err, store.ErrBriefingNotFound) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := briefingTmpl.ExecuteTemplate(w, "briefingPanel", s.panelFor(r.Context(), period, stored)); err != nil {
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
	exportable := stored.Status == store.BriefingReady || stored.Status == store.BriefingGenerating
	if !exportable || stored.BodyMD == "" {
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

// newBriefingService builds the one Service the Web shares across requests, so
// its single-flight map spans concurrent generates. The generator resolves the
// active inference config per run, so settings changes still take effect.
func (s *Web) newBriefingService() *briefing.Service {
	gen := func(ctx context.Context) (briefing.Generator, error) {
		cfg, _, err := s.src.Active(ctx)
		if err != nil {
			return nil, err
		}
		if s.briefingGen != nil {
			return s.briefingGen(ctx, cfg)
		}
		return inference.ResolveBriefing(ctx, cfg.Inference, cfg.Inference.ThinkEnabled())
	}
	return briefing.New(s.db, s.profiles, gen)
}

func (s *Web) briefingService(ctx context.Context) (*briefing.Service, config.Config, error) {
	cfg, _, err := s.src.Active(ctx)
	if err != nil {
		return nil, config.Config{}, err
	}
	return s.briefingSvc, cfg, nil
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

// panelFor is briefingPanelFor plus the prompt user the panel's command line
// shows, so a swapped panel renders the same bar as the page.
func (s *Web) panelFor(ctx context.Context, period briefing.Period, b store.Briefing) briefingPanel {
	p := briefingPanelFor(period, b)
	p.PromptUser = s.pagePromptUser(ctx)
	return p
}

func briefingPanelFor(period briefing.Period, b store.Briefing) briefingPanel {
	p := briefingPanel{
		Kind:        period.Kind,
		PeriodStart: period.Start.Format("2006-01-02"),
		PeriodLabel: briefingPeriodLabel(period),
		Status:      b.Status,
		Complete:    !period.End.After(time.Now()),
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
		p.BodyHTML = renderBriefingMarkdown(b.BodyMD)
		p.ActionLabel = "Generating…"
	case store.BriefingReady:
		p.Ready = true
		p.Error = b.Error
		p.BodyHTML = renderBriefingMarkdown(b.BodyMD)
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

// briefingCalendar is one month of the period picker. Daily mode fills Weeks
// with day cells; weekly mode makes each row the clickable unit.
type briefingCalendar struct {
	Kind       string
	Period     string
	MonthLabel string
	Month      string
	PrevMonth  string
	NextMonth  string
	HasNext    bool
	LatestHref string
	Weekly     bool
	Weeks      []calWeek
}

type calWeek struct {
	Href     string
	Label    string
	Ready    bool
	Selected bool
	Today    bool
	Disabled bool
	Tab      bool
	Days     []calDay
}

type calDay struct {
	Num      int
	Href     string
	Label    string
	Ready    bool
	Selected bool
	Today    bool
	Disabled bool
	Out      bool
	Tab      bool
}

func monthOf(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func (s *Web) handleBriefingCalendar(w http.ResponseWriter, r *http.Request) {
	period, err := briefingPeriod(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	month := monthOf(period.Start)
	if raw := strings.TrimSpace(r.FormValue("month")); raw != "" {
		month, err = time.ParseInLocation("2006-01", raw, time.UTC)
		if err != nil {
			http.Error(w, "month must be YYYY-MM", http.StatusBadRequest)
			return
		}
	}
	cal, err := s.calendarFor(r.Context(), period, month)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := briefingTmpl.ExecuteTemplate(w, "briefingCal", cal); err != nil {
		log.Error().Err(err).Msg("render briefing calendar")
	}
}

// calendarFor builds the month grid around viewed. Rows run Monday to Sunday
// and spill into the neighbouring months; a period that has not finished is
// shown disabled, the same rule that gates generating one.
func (s *Web) calendarFor(ctx context.Context, viewed briefing.Period, month time.Time) (briefingCalendar, error) {
	now := time.Now()
	kind := viewed.Kind
	first, err := briefing.PeriodAt("weekly", month)
	if err != nil {
		return briefingCalendar{}, err
	}
	lastDay := month.AddDate(0, 1, -1)
	lastWeek, err := briefing.PeriodAt("weekly", lastDay)
	if err != nil {
		return briefingCalendar{}, err
	}
	gridStart, gridEnd := first.Start, lastWeek.End

	marks, err := s.db.BriefingMarks(ctx, kind, gridStart, gridEnd)
	if err != nil {
		return briefingCalendar{}, err
	}
	ready := map[string]bool{}
	for _, m := range marks {
		if m.Status == store.BriefingReady {
			ready[m.PeriodStart.Format("2006-01-02")] = true
		}
	}
	latest, err := briefing.PeriodFor(kind, now)
	if err != nil {
		return briefingCalendar{}, err
	}
	today := now.UTC().Format("2006-01-02")
	viewedKey := viewed.Start.Format("2006-01-02")
	weekly := kind == "weekly"
	href := func(start time.Time) string {
		return "/briefing?kind=" + kind + "&period=" + start.Format("2006-01-02")
	}
	suffix := func(label string, rdy, off bool) string {
		switch {
		case rdy:
			return label + ", briefing available"
		case off:
			return label + ", not finished yet"
		}
		return label
	}

	cal := briefingCalendar{
		Kind: kind, Period: viewedKey, Weekly: weekly,
		Month:      month.Format("2006-01"),
		MonthLabel: month.Format("January 2006"),
		PrevMonth:  month.AddDate(0, -1, 0).Format("2006-01"),
		NextMonth:  month.AddDate(0, 1, 0).Format("2006-01"),
		HasNext:    !month.AddDate(0, 1, 0).After(now),
		LatestHref: href(latest.Start),
	}
	for ws := gridStart; ws.Before(gridEnd); ws = ws.AddDate(0, 0, 7) {
		wk := calWeek{}
		wkPeriod := briefing.Period{Start: ws, End: ws.AddDate(0, 0, 7)}
		hasViewed := false
		for i := 0; i < 7; i++ {
			d := ws.AddDate(0, 0, i)
			key := d.Format("2006-01-02")
			cd := calDay{Num: d.Day(), Out: d.Month() != month.Month()}
			if key == today {
				wk.Today = true
				cd.Today = true
			}
			if weekly {
				if key == viewedKey {
					hasViewed = true
				}
			} else {
				end := d.AddDate(0, 0, 1)
				cd.Disabled = end.After(now)
				cd.Ready = ready[key]
				cd.Selected = key == viewedKey
				cd.Href = href(d)
				cd.Label = suffix(d.Format("2 January 2006"), cd.Ready, cd.Disabled)
				if cd.Disabled {
					cd.Href = ""
				}
			}
			wk.Days = append(wk.Days, cd)
		}
		if weekly {
			wk.Selected = hasViewed
			wk.Ready = ready[ws.Format("2006-01-02")]
			wk.Disabled = wkPeriod.End.After(now)
			wk.Href = href(ws)
			wk.Label = suffix("Week of "+ws.Format("2 January 2006"), wk.Ready, wk.Disabled)
			if wk.Disabled {
				wk.Href = ""
			}
		}
		cal.Weeks = append(cal.Weeks, wk)
	}
	// One roving tab stop: the viewed cell, else the first enabled one.
	if weekly {
		pick := -1
		for i, wk := range cal.Weeks {
			if wk.Disabled {
				continue
			}
			if wk.Selected {
				pick = i
				break
			}
			if pick < 0 {
				pick = i
			}
		}
		if pick >= 0 {
			cal.Weeks[pick].Tab = true
		}
	} else {
		var first, sel *calDay
		for wi := range cal.Weeks {
			for di := range cal.Weeks[wi].Days {
				d := &cal.Weeks[wi].Days[di]
				if d.Disabled {
					continue
				}
				if d.Selected && sel == nil {
					sel = d
				}
				if !d.Out && first == nil {
					first = d
				}
			}
		}
		if sel == nil {
			sel = first
		}
		if sel != nil {
			sel.Tab = true
		}
	}
	return cal, nil
}
