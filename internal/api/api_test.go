package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// These tests focus on HTTP-specific behavior (query parsing/validation,
// status codes, JSON shape, default-window resolution). store.List's own
// filtering/sorting logic is already covered by internal/store's tests and
// isn't re-asserted here.

func TestParseListFilter(t *testing.T) {
	const defaultWindow = 72 * time.Hour
	refTime := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		query   string
		wantErr bool
		check   func(t *testing.T, f store.ListFilter)
	}{
		{
			name: "no params defaults to window ending now",
			check: func(t *testing.T, f store.ListFilter) {
				if got := f.Before.Sub(f.After); got != defaultWindow {
					t.Errorf("window width = %s, want %s", got, defaultWindow)
				}
				if time.Since(f.Before) > time.Minute {
					t.Errorf("Before = %s, want close to now", f.Before)
				}
			},
		},
		{
			name:  "before only derives after as before-defaultWindow",
			query: "before=" + url.QueryEscape(refTime.Format(time.RFC3339)),
			check: func(t *testing.T, f store.ListFilter) {
				if !f.Before.Equal(refTime) {
					t.Errorf("Before = %s, want %s", f.Before, refTime)
				}
				if want := refTime.Add(-defaultWindow); !f.After.Equal(want) {
					t.Errorf("After = %s, want %s", f.After, want)
				}
			},
		},
		{
			name:  "after only leaves before unbounded",
			query: "after=" + url.QueryEscape(refTime.Format(time.RFC3339)),
			check: func(t *testing.T, f store.ListFilter) {
				if !f.After.Equal(refTime) {
					t.Errorf("After = %s, want %s", f.After, refTime)
				}
				if !f.Before.IsZero() {
					t.Errorf("Before = %s, want zero (unbounded)", f.Before)
				}
			},
		},
		{
			name: "both given used as-is, no defaulting",
			query: "after=" + url.QueryEscape(refTime.Format(time.RFC3339)) +
				"&before=" + url.QueryEscape(refTime.Add(time.Hour).Format(time.RFC3339)),
			check: func(t *testing.T, f store.ListFilter) {
				if !f.After.Equal(refTime) || !f.Before.Equal(refTime.Add(time.Hour)) {
					t.Errorf("got after=%s before=%s", f.After, f.Before)
				}
			},
		},
		{name: "invalid after", query: "after=not-a-time", wantErr: true},
		{name: "invalid before", query: "before=not-a-time", wantErr: true},
		{name: "invalid limit", query: "limit=abc", wantErr: true},
		{
			name:  "status/source/sort/limit pass through unchanged",
			query: "status=read&source=S1&sort=latest&limit=5",
			check: func(t *testing.T, f store.ListFilter) {
				if f.Status != store.StatusRead || f.Source != "S1" || f.SortBy != store.SortByLatest || f.Limit != 5 {
					t.Errorf("got %+v", f)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/items?"+tt.query, nil)
			f, err := parseListFilter(req, defaultWindow)
			if tt.wantErr {
				if err == nil {
					t.Fatal("parseListFilter() error = nil, want non-nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseListFilter() error = %v", err)
			}
			tt.check(t, f)
		})
	}
}

func newTestServer(t *testing.T) *API {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return New(db)
}

func TestHandleListItems(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	items := []feeds.Item{
		{ID: "a", Source: "S1", Title: "A", Link: "https://x/a"},
		{ID: "b", Source: "S1", Title: "B", Link: "https://x/b"},
	}
	digested := []store.DigestEntry{{Item: items[0], Score: 8, Reason: "solid writeup"}}
	if err := s.db.Record(ctx, items, digested, time.Now()); err != nil {
		t.Fatalf("Record: %v", err)
	}

	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/items", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body)
	}
	var resp listResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("got %d items, want 2", len(resp.Items))
	}
	if resp.Window.After == nil || resp.Window.Before == nil {
		t.Error("window.after/before should be set on the default request")
	}
	var got *string
	for _, it := range resp.Items {
		if it.ID == "a" {
			got = it.LLMScoreReason
		}
	}
	if got == nil || *got != "solid writeup" {
		t.Errorf("item a's LLMScoreReason = %v, want %q", got, "solid writeup")
	}
}

func TestHandleListItemsInvalidQuery(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/items?sort=bogus", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleSetStatus(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	items := []feeds.Item{{ID: "a", Source: "S", Title: "A", Link: "https://x/a"}}
	if err := s.db.Record(ctx, items, nil, time.Now()); err != nil {
		t.Fatalf("Record: %v", err)
	}

	tests := []struct {
		name       string
		path       string
		wantStatus int
		wantState  string // "" skips the post-request state check
	}{
		{name: "seen", path: "/api/items/a/seen", wantStatus: http.StatusNoContent, wantState: store.StatusRead},
		{name: "hide", path: "/api/items/a/hide", wantStatus: http.StatusNoContent, wantState: store.StatusSkipped},
		{name: "unread", path: "/api/items/a/unread", wantStatus: http.StatusNoContent, wantState: store.StatusUnread},
		{name: "unknown id", path: "/api/items/missing/seen", wantStatus: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, tt.path, nil))
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tt.wantStatus, rec.Body)
			}
			if tt.wantState == "" {
				return
			}
			rows, err := s.db.List(ctx, store.ListFilter{Limit: 10})
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(rows) != 1 || rows[0].Status != tt.wantState {
				t.Errorf("status after %s = %+v, want %s", tt.name, rows, tt.wantState)
			}
		})
	}
}

func TestHandleSources(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	items := []feeds.Item{
		{ID: "a", Source: "S1", Title: "A", Link: "https://x/a"},
		{ID: "b", Source: "S2", Title: "B", Link: "https://x/b"},
	}
	if err := s.db.Record(ctx, items, nil, time.Now()); err != nil {
		t.Fatalf("Record: %v", err)
	}

	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sources", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var got []sourceCount
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := []sourceCount{{Source: "S1", Count: 1}, {Source: "S2", Count: 1}}
	if !slices.Equal(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}
