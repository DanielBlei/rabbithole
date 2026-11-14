// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/DanielBlei/rabbithole/internal/decisionbrief"
	"github.com/DanielBlei/rabbithole/internal/store"
)

const sensitiveDBError = "pq: password authentication failed for user admin " +
	"(dsn=postgres://admin:hunter2@db.example/app)"

// briefStartFailStore fails the Start path's stored-brief read while leaving
// the item lookup working.
type briefStartFailStore struct {
	*store.Store
	err error
}

func (s *briefStartFailStore) ItemBrief(context.Context, string) (store.ItemBrief, error) {
	return store.ItemBrief{}, s.err
}

func assertNoSensitive(t *testing.T, body string) {
	t.Helper()
	for _, leaked := range []string{
		"pq:", "password", "hunter2", "secret", "postgres://",
		"authentication", "database is closed", "sql:",
	} {
		if strings.Contains(body, leaked) {
			t.Errorf("response leaked %q: %s", leaked, body)
		}
	}
}

// A/B/G: a start or refresh storage failure returns the fixed start message,
// not the database error.
func TestBriefStartStorageErrorIsSanitized(t *testing.T) {
	var logs bytes.Buffer
	oldLogger := log.Logger
	log.Logger = zerolog.New(&logs).Level(zerolog.DebugLevel)
	t.Cleanup(func() { log.Logger = oldLogger })

	w := newBriefWeb(t, nil, nil)
	w.briefs = decisionbrief.New(
		&briefStartFailStore{Store: w.db, err: errors.New(sensitiveDBError)},
		w.profiles, nil)

	for _, tc := range []struct {
		name string
		path string
	}{
		{"start", "/items/a/brief"},
		{"refresh", "/items/a/brief/refresh"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, tc.path, nil))
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body)
			}
			if got := strings.TrimSpace(rec.Body.String()); got != briefStartFailureMessage {
				t.Fatalf("body = %q, want the fixed start message", got)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
				t.Errorf("Content-Type = %q, want text/plain", ct)
			}
			assertNoSensitive(t, rec.Body.String())
		})
	}

	entry := logs.String()
	if strings.Contains(entry, "hunter2") || strings.Contains(entry, "password") ||
		strings.Contains(entry, "postgres://") {
		t.Errorf("logs leaked database credentials: %s", entry)
	}
	if !strings.Contains(entry, `"error_class":"storage_error"`) {
		t.Errorf("logs lack safe error classification: %s", entry)
	}
}

// C/G: the status endpoint's initial item lookup is sanitized too.
func TestBriefStatusInitialLookupErrorIsSanitized(t *testing.T) {
	w := newBriefWeb(t, nil, nil)
	if err := w.db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/items/a/brief/status", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != briefStatusFailureMessage {
		t.Fatalf("body = %q, want the fixed status message", got)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	assertNoSensitive(t, rec.Body.String())
}

// C/G: a later status read failure uses the same fixed status message.
func TestBriefStatusLoadErrorIsSanitized(t *testing.T) {
	w := newBriefWeb(t, nil, nil)
	w.briefs = decisionbrief.New(
		&statusErrorStore{Store: w.db, err: errors.New(sensitiveDBError)},
		w.profiles, nil)

	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/items/a/brief/status", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != briefStatusFailureMessage {
		t.Fatalf("body = %q, want the fixed status message", got)
	}
	assertNoSensitive(t, rec.Body.String())
}

// D: missing items remain 404s.
func TestBriefMissingItemStaysNotFound(t *testing.T) {
	w := newBriefWeb(t, nil, nil)
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/items/missing/brief"},
		{http.MethodPost, "/items/missing/brief/refresh"},
		{http.MethodGet, "/items/missing/brief/status"},
	} {
		rec := httptest.NewRecorder()
		w.Routes().ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404", tc.method, tc.path, rec.Code)
		}
		if got := strings.TrimSpace(rec.Body.String()); got != "item not found" {
			t.Errorf("%s %s body = %q", tc.method, tc.path, got)
		}
	}
}

// E: successful start and refresh keep their HTML fragment behavior.
func TestBriefSuccessfulFragmentsRemainHTML(t *testing.T) {
	w := newBriefWeb(t, func(context.Context) (decisionbrief.Generator, decisionbrief.Provenance, error) {
		return briefGen(func(context.Context, string, string, json.RawMessage, int) (string, error) {
			return briefJSON, nil
		}), decisionbrief.Provenance{}, nil
	}, briefFetcher(new(int)))

	rec := post(t, w, "/items/a/brief")
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("start Content-Type = %q, want text/html", ct)
	}
	if !strings.Contains(rec.Body.String(), "brief--") {
		t.Errorf("start did not return a panel: %s", rec.Body)
	}
	waitBriefBody(t, w, "a", "Full-text 8/10")

	rec = post(t, w, "/items/a/brief/refresh")
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("refresh Content-Type = %q, want text/html", ct)
	}
	if !strings.Contains(rec.Body.String(), "brief--") {
		t.Errorf("refresh did not return a panel: %s", rec.Body)
	}
	waitBriefBody(t, w, "a", "Full-text 8/10")
}
