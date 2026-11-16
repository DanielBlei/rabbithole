// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func closedStoreAPI(t *testing.T) *API {
	t.Helper()
	s := newTestServer(t)
	if err := s.db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return s
}

func assertSafeInternalResponse(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if body != internalErrorMessage+"\n" {
		t.Fatalf("body = %q, want the fixed internal error message", body)
	}
	for _, leak := range []string{
		"database", "begin tx", "sql:", "postgres://", "sqlite", "secret",
	} {
		if strings.Contains(body, leak) {
			t.Fatalf("body leaked %q: %s", leak, body)
		}
	}
}

func TestClosedStoreStatusMutationIsSanitized(t *testing.T) {
	s := closedStoreAPI(t)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/items/a/seen", nil))
	assertSafeInternalResponse(t, rec)
}

func TestClosedStoreItemListIsSanitized(t *testing.T) {
	s := closedStoreAPI(t)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/items", nil))
	assertSafeInternalResponse(t, rec)
}

func TestClosedStoreSourceListIsSanitized(t *testing.T) {
	s := closedStoreAPI(t)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sources", nil))
	assertSafeInternalResponse(t, rec)
}

func TestMissingItemStatusKeepsNotFoundContract(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/items/missing/seen", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body)
	}
}

func TestInternalErrorLogsOriginalDiagnosticOnly(t *testing.T) {
	oldLogger := log.Logger
	var logs bytes.Buffer
	log.Logger = zerolog.New(&logs).Level(zerolog.DebugLevel)
	t.Cleanup(func() { log.Logger = oldLogger })

	rec := httptest.NewRecorder()
	writeInternalError(rec, "test_operation", errors.New("backend failure: synthetic-secret-value"))
	assertSafeInternalResponse(t, rec)
	if !strings.Contains(logs.String(), "synthetic-secret-value") {
		t.Fatalf("server log did not retain the original diagnostic: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "test_operation") {
		t.Fatalf("server log lacks operation context: %s", logs.String())
	}
}

func TestInvalidListFilterRemainsClientError(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/items?status=bogus", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "status") {
		t.Fatalf("client validation message was lost: %s", rec.Body)
	}
}
