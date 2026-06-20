package vllm

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func withFastValidateRetry(t *testing.T) {
	origAttempts, origBackoff := validateAttempts, validateBackoff
	validateAttempts = 3
	validateBackoff = time.Millisecond
	t.Cleanup(func() {
		validateAttempts = origAttempts
		validateBackoff = origBackoff
	})
}

func TestValidateRetriesUntilServerComesUp(t *testing.T) {
	withFastValidateRetry(t)

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"llama3"}]}`))
	}))
	defer srv.Close()

	c, err := New(srv.URL, "llama3", "", false)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := c.Validate(t.Context()); err != nil {
		t.Fatalf("Validate() error = %v, want nil", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
}

func TestValidateFailsAfterAttemptsExhausted(t *testing.T) {
	withFastValidateRetry(t)

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c, err := New(srv.URL, "llama3", "", false)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := c.Validate(t.Context()); err == nil {
		t.Fatal("Validate() error = nil, want non-nil")
	}
	if calls != validateAttempts {
		t.Fatalf("calls = %d, want %d", calls, validateAttempts)
	}
}

func TestValidateMissingModelDoesNotRetry(t *testing.T) {
	withFastValidateRetry(t)

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"other"}]}`))
	}))
	defer srv.Close()

	c, err := New(srv.URL, "llama3", "", false)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := c.Validate(t.Context()); err == nil {
		t.Fatal("Validate() error = nil, want non-nil")
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (a missing model should fail fast, not retry)", calls)
	}
}