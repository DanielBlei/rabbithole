// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package feeds

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const emptyAtom = `<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom"></feed>`

// A rate-limited arXiv request is retried until the provider answers.
func TestFetchArXivRetriesWhileRateLimited(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(emptyAtom))
	}))
	defer srv.Close()

	f := NewAcademicFetcher()
	f.Client = srv.Client()
	f.ArXivBaseURL = srv.URL

	if _, err := f.Fetch(context.Background(), Source{Name: "arxiv"},
		AcademicQuery{Provider: "arxiv", Query: "agents"}); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("requests = %d, want 3", got)
	}
}

// A provider that stays busy gets academicAttempts tries, then the error.
func TestFetchArXivGivesUpAfterAttempts(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	f := NewAcademicFetcher()
	f.Client = srv.Client()
	f.ArXivBaseURL = srv.URL

	_, err := f.Fetch(context.Background(), Source{Name: "arxiv"},
		AcademicQuery{Provider: "arxiv", Query: "agents"})
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("err = %v, want the 429 status", err)
	}
	if got := calls.Load(); got != academicAttempts {
		t.Fatalf("requests = %d, want %d", got, academicAttempts)
	}
}
