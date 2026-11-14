// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/rs/zerolog/log"

	"github.com/DanielBlei/rabbithole/internal/articlefetch"
	"github.com/DanielBlei/rabbithole/internal/decisionbrief"
	"github.com/DanielBlei/rabbithole/internal/httplog"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// briefStatusUnavailable is a UI-only state for a failed status read. It
// deliberately offers no action: the real state is unknown.
const briefStatusUnavailable = "unavailable"

const (
	briefStartFailureMessage  = "Decision Brief could not be started. Please try again."
	briefStatusFailureMessage = "Decision Brief status is temporarily unavailable."
	briefBusyMessage          = "Decision Brief is busy. Please try again shortly."
)

// briefView is the row panel's state. Current is a ready brief; Previous is a
// readable brief kept when a refresh failed. ID is the authoritative raw item
// identity; URLID is its path-escaped form for URLs and DOM ids.
type briefView struct {
	ID           string
	URLID        string
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
// a zero score still renders as 0. HasScore/HasFeedScore distinguish a real
// zero from a missing score; ScoreNote is grounded only in those fields.
type briefData struct {
	ID                  string
	URLID               string
	HasScore            bool
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
	ScoreNote           string
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
		op := "brief_start"
		if refresh {
			op = "brief_refresh"
		}
		httpBriefStartError(w, err, id, op)
		return
	}
	s.renderBrief(w, id, snap)
}

// httpBriefStartError keeps a full queue a distinct, retryable product state
// rather than a generic start failure. The message is fixed; the error text
// never reaches the response.
func httpBriefStartError(w http.ResponseWriter, err error, itemID, op string) {
	if errors.Is(err, decisionbrief.ErrBriefBusy) {
		w.Header().Set("Retry-After", "1")
		http.Error(w, briefBusyMessage, http.StatusTooManyRequests)
		return
	}
	httpBriefStoreError(w, err, itemID, op, briefStartFailureMessage)
}

// handleBriefStatus answers the 2s poll with the same panel fragment. The
// polling attribute disappears with a terminal state, so polling stops itself.
func (s *Web) handleBriefStatus(w http.ResponseWriter, r *http.Request) {
	httplog.Quiet(r)
	id := r.PathValue("id")
	if _, err := s.db.Get(r.Context(), id); err != nil {
		httpBriefStoreError(w, err, id, "brief_status_item", briefStatusFailureMessage)
		return
	}
	snap, err := s.briefs.Status(r.Context(), id)
	if err != nil {
		httpBriefStoreError(w, err, id, "brief_status", briefStatusFailureMessage)
		return
	}
	s.renderBrief(w, id, snap)
}

// httpBriefStoreError answers a Decision Brief storage failure without echoing
// the database error. A missing item stays a 404; anything else is a 500 with
// the endpoint's fixed product message.
func httpBriefStoreError(w http.ResponseWriter, err error, itemID, op, message string) {
	if errors.Is(err, store.ErrItemNotFound) {
		http.Error(w, "item not found", http.StatusNotFound)
		return
	}
	logBriefStorageFailure(err, op, itemID, true)
	http.Error(w, message, http.StatusInternalServerError)
}

// logBriefStorageFailure records stable classification and type only. Database
// errors can embed DSNs, passwords or tokens; the raw text stays out of logs
// and HTTP responses.
func logBriefStorageFailure(err error, op, itemID string, severe bool) {
	ev := log.Warn()
	if severe {
		ev = log.Error()
	}
	ev = ev.Str("op", op).
		Str("error_class", briefStorageClass(err)).
		Str("error_type", fmt.Sprintf("%T", err))
	if itemID != "" {
		ev = ev.Str("item", itemID)
	}
	ev.Msg("decision brief storage failure")
}

func briefStorageClass(err error) string {
	switch {
	case errors.Is(err, store.ErrItemNotFound):
		return "not_found"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return "storage_error"
	}
}

func (s *Web) renderBrief(w http.ResponseWriter, itemID string, snap decisionbrief.Snapshot) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := feedTmpl.ExecuteTemplate(w, "brief_panel", briefViewFor(itemID, snap, true)); err != nil {
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

// briefViewFor builds one panel from the authoritative item ID and an optional
// snapshot. A missing, mismatched or malformed snapshot becomes an explicit
// unavailable state with no actions, so it can never produce an empty-ID URL.
func briefViewFor(itemID string, snap decisionbrief.Snapshot, ok bool) briefView {
	if strings.TrimSpace(itemID) == "" || !ok || snap.ItemID != itemID ||
		!knownBriefStatus(snap.Status) ||
		(snap.Brief.ItemID != "" && snap.Brief.ItemID != itemID) {
		return unavailableView(itemID)
	}
	v := briefView{
		ID: itemID, URLID: url.PathEscape(itemID),
		Status: snap.Status, Phase: snap.Phase, ErrorCode: snap.ErrorCode,
	}
	// A storage failure is reported with a fixed product message. Its raw cause
	// can contain database or connection details, so it is never rendered, even
	// if a row written elsewhere carries one.
	if snap.ErrorCode != decisionbrief.CodeStorageFailed {
		v.ErrorDetail = snap.ErrorDetail
	}
	switch snap.Status {
	case decisionbrief.StatusQueued, decisionbrief.StatusRunning:
		v.PhaseText = phaseText(snap.Phase)
		// A refresh keeps its previous ready brief visible until the new result
		// is persisted; an initial generation has none.
		if snap.Brief.Status == store.ItemBriefReady {
			data := briefDataFrom(itemID, snap.Brief)
			v.Current = &data
		}
	case decisionbrief.StatusReady:
		if snap.Brief.Status == store.ItemBriefReady {
			data := briefDataFrom(itemID, snap.Brief)
			v.Current = &data
		}
	case decisionbrief.StatusFailed:
		v.ErrorMessage = decisionbrief.ProductMessage(snap.ErrorCode)
		if snap.Brief.Status == store.ItemBriefReady {
			data := briefDataFrom(itemID, snap.Brief)
			v.Previous = &data
		}
	}
	return v
}

func knownBriefStatus(status string) bool {
	switch status {
	case decisionbrief.StatusIdle, decisionbrief.StatusQueued, decisionbrief.StatusRunning,
		decisionbrief.StatusReady, decisionbrief.StatusFailed:
		return true
	default:
		return false
	}
}

func unavailableView(itemID string) briefView {
	return briefView{
		ID: itemID, URLID: url.PathEscape(itemID), Status: briefStatusUnavailable,
	}
}

// applyBriefViews gives every row the panel built from its own authoritative ID.
// A failed batch read is never rendered as an absent state.
func applyBriefViews(rows []rowData, snaps map[string]decisionbrief.Snapshot, err error) {
	for i := range rows {
		if err != nil {
			rows[i].Brief = unavailableView(rows[i].ID)
			continue
		}
		snap, ok := snaps[rows[i].ID]
		rows[i].Brief = briefViewFor(rows[i].ID, snap, ok)
	}
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

func briefDataFrom(itemID string, b store.ItemBrief) briefData {
	d := briefData{
		ID:             itemID,
		URLID:          url.PathEscape(itemID),
		Recommendation: b.Recommendation,
		Why:            b.Why,
		KeyPoints:      b.KeyPoints,
		Novelty:        b.Novelty,
		Caveats:        b.Caveats,
		Focus:          b.Focus,
		ReadingMinutes: b.ReadingMinutes,
		WordCount:      b.WordCount,
		Truncated:      b.Truncated,
		// Persisted rows from before this guard, or a future writer, may still
		// carry userinfo. The panel is the last place it could be shown.
		FinalURL: articlefetch.WithoutUserinfo(b.FinalURL),
	}
	if b.Score != nil {
		d.HasScore = true
		d.Score = *b.Score
	}
	if b.FeedScore != nil {
		d.HasFeedScore = true
		d.FeedScore = *b.FeedScore
	}
	d.RecommendationLabel = recommendationLabel(b.Recommendation)
	d.Provenance = provenance(b)
	d.ScoreNote = scoreNote(d.HasScore, d.HasFeedScore)
	return d
}

// scoreNote states only what the persisted brief proves: whether a feed score
// and a full-text score exist. It never names RSS or infers the feed scoring
// path from a missing value.
func scoreNote(hasScore, hasFeedScore bool) string {
	switch {
	case hasScore && hasFeedScore:
		return "A feed score was recorded for this item; the full-text score above was computed from the fetched article."
	case hasScore:
		return "The full-text score above was computed from the fetched article."
	case hasFeedScore:
		return "A feed score was recorded for this item; no full-text score is available."
	default:
		return "No score provenance was recorded."
	}
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
