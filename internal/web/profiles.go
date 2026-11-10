// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/feedback"
	"github.com/DanielBlei/rabbithole/internal/ingest"
	"github.com/DanielBlei/rabbithole/internal/profile"
	"github.com/DanielBlei/rabbithole/internal/profilemgr"
	"github.com/DanielBlei/rabbithole/internal/store"
)

const (
	profileFormBodyLimit      = profile.MaxContentBytes*4 + 16*1024
	profileRescoreWindowValue = "7d"
)

type profileSettingsData struct {
	Rows       []profileRowData
	ActiveName string
	Form       *profileFormData
	Error      string
	RunActive  bool
	Adaptive   adaptiveRankingView
}

// adaptiveRankingView is the activity indicator beside the active profile.
// State is the word shown on the chip; Detail explains what that means and how
// many explicit ratings feed it.
type adaptiveRankingView struct {
	State  string
	Detail string
	Tone   string
	Count  int
}

type profileRowData struct {
	ID      string
	Name    string
	Builtin bool
	Active  bool
}

type profileFormData struct {
	ID           string
	Name         string
	Mode         string // structured | raw
	Interested   string
	Less         string
	Context      string
	Content      string
	Builtin      bool
	Adding       bool
	CanStructure bool
	Err          string
	Notice       string
}

type profileDeleteData struct {
	ID     string
	Name   string
	Active bool
}

type profileRescoreData struct {
	ProfileName string
	WindowDays  int
}

func (s *Web) handleProfiles(w http.ResponseWriter, r *http.Request) {
	s.renderProfiles(w, r, nil)
}

func (s *Web) profileSettings(ctx context.Context, form *profileFormData) profileSettingsData {
	items, err := s.profiles.List(ctx)
	if err != nil {
		log.Error().Err(err).Msg("list profiles for settings")
		return profileSettingsData{Error: "profiles unavailable: " + err.Error(), Form: form}
	}
	data := profileSettingsData{
		Form: form, RunActive: s.ing.Status().Running,
		Adaptive: s.adaptiveRankingView(ctx),
	}
	for _, item := range items {
		data.Rows = append(data.Rows, profileRowData{
			ID: item.ID, Name: item.Name, Builtin: item.Builtin, Active: item.Active,
		})
		if item.Active {
			data.ActiveName = item.Name
		}
	}
	if data.ActiveName == "" {
		data.ActiveName = profile.DefaultName
	}
	return data
}

// adaptiveRankingView reports whether explicit feedback will shape the next
// score and how much explicit signal has accumulated. A store or config read
// failure is logged and shown as unavailable rather than taking the page down.
func (s *Web) adaptiveRankingView(ctx context.Context) adaptiveRankingView {
	count, err := s.db.CountExplicitFeedback(ctx)
	if err != nil {
		log.Error().Err(err).Msg("count explicit feedback for settings")
		return adaptiveRankingView{State: "Unavailable", Tone: "warn",
			Detail: "Could not read the rating count."}
	}
	cfg, _, err := s.src.Active(ctx)
	if err != nil {
		log.Error().Err(err).Msg("read config for adaptive ranking indicator")
		return adaptiveRankingView{State: "Unavailable", Tone: "warn", Count: count,
			Detail: "Could not read the adaptive ranking setting."}
	}
	if !cfg.AdaptiveRankingEnabled() {
		return adaptiveRankingView{State: "Off", Tone: "dim", Count: count,
			Detail: "Profile-only scoring. Recorded ratings are not used."}
	}
	if providerOf(cfg.Inference) == config.ProviderHeuristic {
		return adaptiveRankingView{State: "Profile-only", Tone: "dim", Count: count,
			Detail: "The keyword provider does not use ratings. Choose a model provider to enable adaptive ranking."}
	}
	if count < feedback.MinExamples {
		return adaptiveRankingView{State: "Collecting", Tone: "warn", Count: count,
			Detail: fmt.Sprintf("%d of %d ratings needed to activate.", count, feedback.MinExamples)}
	}
	return adaptiveRankingView{State: "Active", Tone: "ok", Count: count,
		Detail: "Future scores learn from your ratings."}
}

func (s *Web) handleProfileNew(w http.ResponseWriter, r *http.Request) {
	mode := r.URL.Query().Get("mode")
	if mode != "raw" {
		mode = "structured"
	}
	s.renderProfiles(w, r, &profileFormData{Adding: true, Mode: mode})
}

func (s *Web) handleProfileEdit(w http.ResponseWriter, r *http.Request) {
	item, err := s.profiles.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		httpProfileError(w, err)
		return
	}
	form := formFromProfile(item, r.URL.Query().Get("mode"))
	s.renderProfiles(w, r, form)
}

func formFromProfile(item profilemgr.Item, requestedMode string) *profileFormData {
	fields, canonical := profile.ParseCanonical(item.Content)
	mode := requestedMode
	if item.Builtin {
		mode = "raw"
	} else if mode == "" {
		if canonical {
			mode = "structured"
		} else {
			mode = "raw"
		}
	}
	if mode == "structured" && !canonical {
		mode = "raw"
	}
	return &profileFormData{
		ID: item.ID, Name: item.Name, Content: item.Content, Builtin: item.Builtin,
		Mode: mode, Interested: fields.Interested, Less: fields.Less, Context: fields.Context,
		CanStructure: canonical,
	}
}

func (s *Web) handleProfileCreate(w http.ResponseWriter, r *http.Request) {
	form, content, ok := profileFormFromRequest(w, r)
	form.Adding = true
	if !ok {
		s.renderProfiles(w, r, form)
		return
	}
	created, err := s.profiles.Create(r.Context(), form.Name, content)
	if err != nil {
		if isProfileInputError(err) {
			form.Err = err.Error()
			s.renderProfiles(w, r, form)
			return
		}
		httpProfileError(w, err)
		return
	}
	saved := formFromProfile(created, form.Mode)
	saved.Notice = "profile created"
	s.renderProfiles(w, r, saved)
}

func (s *Web) handleProfileUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	form, content, ok := profileFormFromRequest(w, r)
	form.ID = id
	if !ok {
		s.renderProfiles(w, r, form)
		return
	}
	updated, err := s.profiles.Update(r.Context(), id, form.Name, content)
	if err != nil {
		if isProfileInputError(err) {
			form.Err = err.Error()
			s.renderProfiles(w, r, form)
			return
		}
		httpProfileError(w, err)
		return
	}
	saved := formFromProfile(updated, form.Mode)
	saved.Notice = "profile saved"
	s.renderProfiles(w, r, saved)
}

func profileFormFromRequest(w http.ResponseWriter, r *http.Request) (*profileFormData, string, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, profileFormBodyLimit)
	if err := r.ParseForm(); err != nil {
		return &profileFormData{Err: "profile form is too large"}, "", false
	}
	form := &profileFormData{
		Name: r.FormValue("name"), Mode: r.FormValue("mode"),
		Interested: r.FormValue("interested"),
		Less:       r.FormValue("less"),
		Context:    r.FormValue("context"),
		Content:    r.FormValue("content"),
	}
	var content string
	switch form.Mode {
	case "raw":
		content = form.Content
	case "structured", "":
		form.Mode = "structured"
		if strings.TrimSpace(form.Interested) == "" &&
			strings.TrimSpace(form.Less) == "" &&
			strings.TrimSpace(form.Context) == "" {
			form.Err = "profile content cannot be empty"
			return form, "", false
		}
		content = profile.RenderCanonical(profile.EditorFields{
			Interested: form.Interested, Less: form.Less, Context: form.Context,
		})
	default:
		form.Err = "invalid editor mode"
		return form, "", false
	}
	return form, content, true
}

func (s *Web) handleProfileDuplicate(w http.ResponseWriter, r *http.Request) {
	item, err := s.profiles.Duplicate(r.Context(), r.PathValue("id"), "")
	if err != nil {
		httpProfileError(w, err)
		return
	}
	form := formFromProfile(item, "")
	form.Notice = "profile duplicated"
	s.renderProfiles(w, r, form)
}

func (s *Web) handleProfileActive(w http.ResponseWriter, r *http.Request) {
	if err := s.profiles.SetActive(r.Context(), r.PathValue("id")); err != nil {
		httpProfileError(w, err)
		return
	}
	s.renderProfiles(w, r, nil)
}

func (s *Web) handleProfileConfirmRescore(w http.ResponseWriter, r *http.Request) {
	active, err := s.profiles.Resolve(r.Context())
	if err != nil {
		httpProfileError(w, err)
		return
	}
	s.renderFragment(w, "profileConfirmRescore", profileRescoreData{
		ProfileName: active.Name,
		WindowDays:  int(ingest.ProfileRescoreWindow / (24 * time.Hour)),
	})
}

func (s *Web) handleProfileRescore(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid rescore request", http.StatusBadRequest)
		return
	}
	if r.FormValue("window") != profileRescoreWindowValue {
		http.Error(w, "invalid rescore window", http.StatusBadRequest)
		return
	}
	if err := s.ing.StartRescore(r.Context(), ingest.ProfileRescoreWindow); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderIngestModal(w, r, false)
}

func (s *Web) handleProfileConfirmDelete(w http.ResponseWriter, r *http.Request) {
	item, err := s.profiles.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		httpProfileError(w, err)
		return
	}
	if item.Builtin {
		httpProfileError(w, store.ErrProfileImmutable)
		return
	}
	s.renderFragment(w, "profileConfirmDelete", profileDeleteData{
		ID: item.ID, Name: item.Name, Active: item.Active,
	})
}

func (s *Web) handleProfileDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.profiles.Delete(r.Context(), r.PathValue("id")); err != nil {
		httpProfileError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if _, err := w.Write([]byte(
		`<span id="profileDeleteDone" hx-swap-oob="innerHTML:#modalTop"></span>`,
	)); err != nil {
		log.Error().Err(err).Msg("write profile delete response")
		return
	}
	s.writeProfiles(w, r, nil)
}

func (s *Web) renderProfiles(w http.ResponseWriter, r *http.Request, form *profileFormData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	s.writeProfiles(w, r, form)
}

func (s *Web) writeProfiles(w http.ResponseWriter, r *http.Request, form *profileFormData) {
	data := s.profileSettings(r.Context(), form)
	if err := feedTmpl.ExecuteTemplate(w, "profileSettings", data); err != nil {
		log.Error().Err(err).Msg("render profile settings")
	}
}

func isProfileInputError(err error) bool {
	return errors.Is(err, profile.ErrInvalidName) ||
		errors.Is(err, profile.ErrInvalidContent) ||
		errors.Is(err, profile.ErrInvalidID)
}

func httpProfileError(w http.ResponseWriter, err error) {
	switch {
	case isProfileInputError(err):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, store.ErrProfileNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, store.ErrProfileImmutable):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		http.Error(w, fmt.Sprintf("profile operation failed: %v", err), http.StatusInternalServerError)
	}
}
