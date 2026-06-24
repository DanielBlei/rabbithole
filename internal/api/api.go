// Package api exposes ai-searcher's item store over HTTP as JSON — the stable
// /api/* contract (see docs/api.md). Handlers call the same internal/store
// functions the CLI calls. It is mounted by internal/server alongside the HTML
// web UI; the two are separate route sets over the same store.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/DanielBlei/ai-searcher/internal/store"
)

// defaultListWindow is the lookback a bare /api/items list (no after/before)
// returns, and the page width when paging older via before.
const defaultListWindow = 3 * 24 * time.Hour

// API wires the JSON HTTP handlers to a Store.
type API struct {
	db *store.Store
}

// New returns an API backed by db.
func New(db *store.Store) *API {
	return &API{db: db}
}

// Routes builds the JSON API handler. Uses the stdlib's method+wildcard
// ServeMux (Go 1.22+) — no router dependency needed. The patterns keep their
// full /api/ prefix so this mux still matches when internal/server mounts it
// under "/api/" without StripPrefix.
func (s *API) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/items", s.handleListItems)
	mux.HandleFunc("GET /api/sources", s.handleSources)
	mux.HandleFunc("POST /api/items/{id}/read", s.handleSetStatus(store.StatusRead))
	mux.HandleFunc("POST /api/items/{id}/skip", s.handleSetStatus(store.StatusSkipped))
	mux.HandleFunc("POST /api/items/{id}/unread", s.handleSetStatus(store.StatusUnread))
	return mux
}

// Item is the JSON shape of a store.ItemRow. Kept separate from ItemRow so
// internal/store stays free of HTTP/JSON concerns.
type Item struct {
	ID             string     `json:"id"`
	Source         string     `json:"source"`
	Title          string     `json:"title"`
	Link           string     `json:"link"`
	Status         string     `json:"status"`
	LLMScore       *int       `json:"llm_score,omitempty"`
	LLMScoreReason *string    `json:"llm_score_reason,omitempty"`
	UserScore      *int       `json:"user_score,omitempty"`
	PublishedAt    *time.Time `json:"published_at,omitempty"`
}

func fromItemRow(r store.ItemRow) Item {
	return Item{
		ID:             r.ID,
		Source:         r.Source,
		Title:          r.Title,
		Link:           r.Link,
		Status:         r.Status,
		LLMScore:       r.LLMScore,
		LLMScoreReason: r.LLMScoreReason,
		UserScore:      r.UserScore,
		PublishedAt:    r.PublishedAt,
	}
}

// window reports the After/Before bounds actually used for a list request,
// so the client can page "older" by sending our After back as their Before
// without needing to know the default window width itself.
type window struct {
	After  *time.Time `json:"after,omitempty"`
	Before *time.Time `json:"before,omitempty"`
}

type listResponse struct {
	Items  []Item `json:"items"`
	Window window `json:"window"`
}

func (s *API) handleListItems(w http.ResponseWriter, r *http.Request) {
	filter, err := parseListFilter(r, defaultListWindow)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rows, err := s.db.List(r.Context(), filter)
	if err != nil {
		// A bad status/sort is the client's fault (400); anything else is an
		// execution failure on our side (500).
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrInvalidFilter) {
			status = http.StatusBadRequest
		}
		http.Error(w, err.Error(), status)
		return
	}
	items := make([]Item, len(rows))
	for i, row := range rows {
		items[i] = fromItemRow(row)
	}
	resp := listResponse{Items: items, Window: windowOf(filter)}
	writeJSON(w, http.StatusOK, resp)
}

func windowOf(filter store.ListFilter) window {
	var win window
	if !filter.After.IsZero() {
		win.After = &filter.After
	}
	if !filter.Before.IsZero() {
		win.Before = &filter.Before
	}
	return win
}

type sourceCount struct {
	Source string `json:"source"`
	Count  int    `json:"count"`
}

func (s *API) handleSources(w http.ResponseWriter, r *http.Request) {
	counts, err := s.db.Sources(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp := make([]sourceCount, len(counts))
	for i, c := range counts {
		resp[i] = sourceCount{Source: c.Source, Count: c.Count}
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleSetStatus returns a handler that sets the {id} path item's status,
// shared by the read/skip/unread routes (status is fixed per route).
func (s *API) handleSetStatus(status string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		// status is fixed per route, so the only client error here is an
		// unknown id (404); everything else is an execution failure (500).
		if err := s.db.UpdateUserState(r.Context(), id, store.UserPatch{Status: &status}); err != nil {
			if errors.Is(err, store.ErrItemNotFound) {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// parseListFilter translates an /api/items request's query params into a
// store.ListFilter.
//
// If both after and before are omitted, the window defaults to
// [now-defaultWindow, now). If only before is given (the "older page" case —
// the client just echoes back the previous response's window.after), after
// is derived as before-defaultWindow, so every page is the same width
// without the client needing to know defaultWindow itself. If only after is
// given, before is left open-ended (everything since after).
func parseListFilter(r *http.Request, defaultWindow time.Duration) (store.ListFilter, error) {
	q := r.URL.Query()
	filter := store.ListFilter{
		Status: q.Get("status"),
		Source: q.Get("source"),
		SortBy: q.Get("sort"),
	}

	afterStr, beforeStr := q.Get("after"), q.Get("before")
	var after, before time.Time
	var err error
	if afterStr != "" {
		if after, err = parseTime("after", afterStr); err != nil {
			return store.ListFilter{}, err
		}
	}
	if beforeStr != "" {
		if before, err = parseTime("before", beforeStr); err != nil {
			return store.ListFilter{}, err
		}
	}

	switch {
	case afterStr == "" && beforeStr == "":
		before = time.Now()
		after = before.Add(-defaultWindow)
	case afterStr == "" && beforeStr != "":
		after = before.Add(-defaultWindow)
	}
	filter.After, filter.Before = after, before

	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return store.ListFilter{}, fmt.Errorf("invalid limit %q: %w", v, err)
		}
		filter.Limit = n
	}
	return filter, nil
}

func parseTime(field, v string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid %s %q: must be RFC3339: %w", field, v, err)
	}
	return t, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
