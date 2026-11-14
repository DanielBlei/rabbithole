// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/rs/zerolog/log"

	"github.com/DanielBlei/rabbithole/internal/decisionbrief"
	"github.com/DanielBlei/rabbithole/internal/httplog"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// briefView is the row panel's state. Current is a ready brief; Previous is a
// readable brief kept when a refresh failed.
type briefView struct {
	ID           string
	Status       string
	Phase        string
	PhaseText    string
	ErrorCode    string
	ErrorMessage string
	ErrorDetail  string
	Current      *briefData
	Previous     *briefData
}

// briefData is one brief formatted for the template. Pointers are flattened so
// a zero score still renders as 0.
type briefData struct {
	ID                  string
	Score               int
	HasFeedScore        bool
	FeedScore           int
	Recommendation      string
	RecommendationLabel string
	Why                 string
	KeyPoints           []string
	Novelty             string
	Caveats             string
	Focus               string
	ReadingMinutes      int
	WordCount           int
	Truncated           bool
	FinalURL            string
	Provenance          string
}

func (s *Web) handleBriefStart(w http.ResponseWriter, r *http.Request) {
	s.briefAction(w, r, false)
}

func (s *Web) handleBriefRefresh(w http.ResponseWriter, r *http.Request) {
	s.briefAction(w, r, true)
}

func (s *Web) briefAction(w http.ResponseWriter, r *http.Request, refresh bool) {
	id := r.PathValue("id")
	snap, err := s.briefs.Start(r.Context(), id, refresh, s.briefOptions(r.Context()))
	if err != nil {
		httpStoreError(w, err)
		return
	}
	s.renderBrief(w, snap)
}

// handleBriefStatus answers the 2s poll with the same panel fragment. The
// polling attribute disappears with a terminal state, so polling stops itself.
func (s *Web) handleBriefStatus(w http.ResponseWriter, r *http.Request) {
	httplog.Quiet(r)
	id := r.PathValue("id")
	if _, err := s.db.Get(r.Context(), id); err != nil {
		httpStoreError(w, err)
		return
	}
	snap, err := s.briefs.Status(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderBrief(w, snap)
}

func (s *Web) renderBrief(w http.ResponseWriter, snap decisionbrief.Snapshot) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := feedTmpl.ExecuteTemplate(w, "brief_panel", briefViewFor(snap)); err != nil {
		log.Error().Err(err).Msg("render brief panel")
	}
}

func (s *Web) briefOptions(ctx context.Context) decisionbrief.Options {
	cfg, _, err := s.src.Active(ctx)
	if err != nil {
		return decisionbrief.Options{}
	}
	eff := cfg.Effective()
	return decisionbrief.Options{
		MaxArticleBytes: eff.DecisionBrief.MaxArticleBytes,
		MaxTextBytes:    eff.DecisionBrief.MaxTextBytes,
		MaxOutputTokens: eff.DecisionBrief.MaxOutputTokens,
	}
}

func briefViewFor(snap decisionbrief.Snapshot) briefView {
	status := snap.Status
	if status == "" {
		status = decisionbrief.StatusIdle
	}
	v := briefView{
		ID: snap.ItemID, Status: status, Phase: snap.Phase,
		ErrorCode: snap.ErrorCode, ErrorDetail: snap.ErrorDetail,
	}
	switch status {
	case decisionbrief.StatusQueued, decisionbrief.StatusRunning:
		v.PhaseText = phaseText(snap.Phase)
	case decisionbrief.StatusReady:
		if snap.Brief.Status == store.ItemBriefReady {
			data := briefDataFrom(snap.Brief)
			v.Current = &data
		}
	case decisionbrief.StatusFailed:
		v.ErrorMessage = decisionbrief.ProductMessage(snap.ErrorCode)
		if snap.Brief.Status == store.ItemBriefReady {
			data := briefDataFrom(snap.Brief)
			v.Previous = &data
		}
	}
	return v
}

func phaseText(phase string) string {
	switch phase {
	case decisionbrief.PhaseFetching:
		return "Fetching article…"
	case decisionbrief.PhaseAnalyzing:
		return "Analyzing full text…"
	case decisionbrief.PhaseQueued:
		return "Queued — waiting for a free analysis slot…"
	default:
		return "Working…"
	}
}

func briefDataFrom(b store.ItemBrief) briefData {
	d := briefData{
		ID:             b.ItemID,
		Recommendation: b.Recommendation,
		Why:            b.Why,
		KeyPoints:      b.KeyPoints,
		Novelty:        b.Novelty,
		Caveats:        b.Caveats,
		Focus:          b.Focus,
		ReadingMinutes: b.ReadingMinutes,
		WordCount:      b.WordCount,
		Truncated:      b.Truncated,
		FinalURL:       b.FinalURL,
	}
	if b.Score != nil {
		d.Score = *b.Score
	}
	if b.FeedScore != nil {
		d.HasFeedScore = true
		d.FeedScore = *b.FeedScore
	}
	d.RecommendationLabel = recommendationLabel(b.Recommendation)
	d.Provenance = provenance(b)
	return d
}

func recommendationLabel(recommendation string) string {
	switch recommendation {
	case "read":
		return "Read in full"
	case "skim":
		return "Skim"
	case "skip":
		return "Skip"
	default:
		return recommendation
	}
}

func provenance(b store.ItemBrief) string {
	var parts []string
	if b.Model != "" {
		model := b.Model
		if b.Provider != "" {
			model = b.Provider + "/" + model
		}
		parts = append(parts, model)
	}
	if b.ProfileName != "" {
		profile := b.ProfileName
		if b.ProfileHash != "" {
			profile += "@" + shortHash(b.ProfileHash)
		}
		parts = append(parts, "profile "+profile)
	}
	if !b.CreatedAt.IsZero() {
		parts = append(parts, b.CreatedAt.UTC().Format("2 Jan 2006"))
	}
	if b.WordCount > 0 {
		parts = append(parts, fmt.Sprintf("%d words", b.WordCount))
	}
	if b.ContentHash != "" {
		parts = append(parts, "content "+shortHash(b.ContentHash))
	}
	return strings.Join(parts, " · ")
}
