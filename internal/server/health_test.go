package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func get(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	var payload struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decoding %q: %v", body, err)
	}
	return rec.Code, payload.Status
}

func TestHealthzIsOK(t *testing.T) {
	srv, _ := newTestServer(t)
	if code, status := get(t, srv.Routes(), "/healthz"); code != http.StatusOK || status != "ok" {
		t.Errorf("/healthz = %d %q, want 200 \"ok\"", code, status)
	}
}

// Liveness must not depend on the store: a broken database is not something a
// process restart fixes, so healthz stays 200 while readyz reports the outage.
func TestHealthzIgnoresBrokenStore(t *testing.T) {
	srv, db := newTestServer(t)
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if code, _ := get(t, srv.Routes(), "/healthz"); code != http.StatusOK {
		t.Errorf("/healthz = %d, want 200 with the store down", code)
	}
}

func TestReadyzIsOK(t *testing.T) {
	srv, _ := newTestServer(t)
	if code, status := get(t, srv.Routes(), "/readyz"); code != http.StatusOK || status != "ok" {
		t.Errorf("/readyz = %d %q, want 200 \"ok\"", code, status)
	}
}

func TestReadyzFailsOnUnreachableStore(t *testing.T) {
	srv, db := newTestServer(t)
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if code, _ := get(t, srv.Routes(), "/readyz"); code != http.StatusServiceUnavailable {
		t.Errorf("/readyz = %d, want 503 with the store closed", code)
	}
}

func TestDrainFailsReadiness(t *testing.T) {
	srv, _ := newTestServer(t)
	routes := srv.Routes()
	srv.Drain()
	if code, _ := get(t, routes, "/readyz"); code != http.StatusServiceUnavailable {
		t.Errorf("/readyz = %d after Drain, want 503", code)
	}
	// Draining is about routing, not liveness — the process is still healthy.
	if code, _ := get(t, routes, "/healthz"); code != http.StatusOK {
		t.Errorf("/healthz = %d after Drain, want 200", code)
	}
}
