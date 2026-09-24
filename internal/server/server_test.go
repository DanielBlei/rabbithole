// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/ingest"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// newServer builds a Server over an empty store, logging to logger. The login
// gate starts switched off, so the routing tests see the routes themselves;
// the gate's own tests switch it back on.
func newServer(t *testing.T, logger zerolog.Logger) (*Server, *store.Store) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.DisableAuth(context.Background()); err != nil {
		t.Fatalf("DisableAuth: %v", err)
	}

	think := false
	cfg := &config.Config{}
	cfg.Inference.Think = &think
	mgr, err := ingest.NewManager(db, cfg, zerolog.InfoLevel)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return New(db, cfg, "127.0.0.1:0", "config.yaml", mgr, logger), db
}

func newTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	return newServer(t, zerolog.Nop())
}

// newLoggingServer returns a Server whose access log is captured in the buffer.
func newLoggingServer(t *testing.T) (*Server, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	srv, _ := newServer(t, zerolog.New(&buf).Level(zerolog.DebugLevel))
	return srv, &buf
}

// TestRoutesDispatch pins which route set answers each path. The mount order in
// Routes is the thing under test: "/" is a catch-all, so anything more specific
// that stops winning would silently fall through to the web UI.
func TestRoutesDispatch(t *testing.T) {
	tests := []struct {
		name        string
		method      string
		path        string
		wantStatus  int
		wantContent string // Content-Type prefix, identifying the answering route set
	}{
		{"digest at root", http.MethodGet, "/", http.StatusOK, "text/html"},
		{"digest page", http.MethodGet, "/feed", http.StatusOK, "text/html"},
		{"maze page", http.MethodGet, "/maze", http.StatusOK, "text/html"},
		{"api items", http.MethodGet, "/api/items", http.StatusOK, "application/json"},
		{"api sources", http.MethodGet, "/api/sources", http.StatusOK, "application/json"},
		{"liveness", http.MethodGet, "/healthz", http.StatusOK, "application/json"},
		{"readiness", http.MethodGet, "/readyz", http.StatusOK, "application/json"},
		{"static asset", http.MethodGet, "/static/style.css", http.StatusOK, "text/css"},
		{"unknown path", http.MethodGet, "/nope", http.StatusNotFound, ""},
		{"unknown api path", http.MethodGet, "/api/nope", http.StatusNotFound, ""},
		{"wrong method on a page", http.MethodPost, "/feed", http.StatusMethodNotAllowed, ""},
		{"wrong method on health", http.MethodPost, "/healthz", http.StatusNotFound, ""},
	}

	srv, _ := newTestServer(t)
	routes := srv.Routes()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			routes.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, nil))

			if rec.Code != tt.wantStatus {
				t.Errorf("%s %s = %d, want %d", tt.method, tt.path, rec.Code, tt.wantStatus)
			}
			if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, tt.wantContent) {
				t.Errorf("%s %s Content-Type = %q, want prefix %q",
					tt.method, tt.path, got, tt.wantContent)
			}
		})
	}
}

// The API mux keeps its full /api/ patterns, so it is mounted without
// StripPrefix. If that ever changes, /api/items stops matching and the web
// catch-all answers with HTML instead — a silent break this catches.
func TestAPIMountedWithoutStripPrefix(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/items", nil))

	var payload struct {
		Items  []any `json:"items"`
		Window struct {
			After  string `json:"after"`
			Before string `json:"before"`
		} `json:"window"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("/api/items did not return the API's JSON shape: %v (body %q)", err, rec.Body)
	}
	if payload.Window.After == "" || payload.Window.Before == "" {
		t.Errorf("window = %+v, want both bounds set", payload.Window)
	}
}

// TestAccessLogLevels checks the middleware is actually installed on the root
// handler and that the quiet routes are wired, through the real route table.
func TestAccessLogLevels(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		want   string
	}{
		{"page load is info", http.MethodGet, "/feed", "info"},
		{"api call is info", http.MethodGet, "/api/items", "info"},
		{"liveness is quiet", http.MethodGet, "/healthz", "debug"},
		{"readiness is quiet", http.MethodGet, "/readyz", "debug"},
		{"static asset is quiet", http.MethodGet, "/static/style.css", "debug"},
		{"unknown path warns", http.MethodGet, "/nope", "warn"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, buf := newLoggingServer(t)
			srv.Routes().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(tt.method, tt.path, nil))

			event := lastEvent(t, buf)
			if event["level"] != tt.want {
				t.Errorf("%s logged at %v, want %v", tt.path, event["level"], tt.want)
			}
			if event["path"] != tt.path {
				t.Errorf("path = %v, want %v", event["path"], tt.path)
			}
		})
	}
}

// The ingest modal polls this every 2s while a run is live, so it must stay
// quiet or it buries the run's own output on the shared stderr.
func TestIngestPollIsQuiet(t *testing.T) {
	srv, buf := newLoggingServer(t)
	srv.Routes().ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/ingest/status", nil))

	if event := lastEvent(t, buf); event["level"] != "debug" {
		t.Errorf("/ingest/status logged at %v, want debug", event["level"])
	}
}

// lastEvent decodes the final log line written, which is the access line —
// the middleware logs after the handler returns.
func lastEvent(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	if len(lines) == 0 || len(lines[0]) == 0 {
		t.Fatal("no access line logged — is the middleware installed?")
	}
	var event map[string]any
	last := lines[len(lines)-1]
	if err := json.Unmarshal(last, &event); err != nil {
		t.Fatalf("decoding %q: %v", last, err)
	}
	if event["message"] != "request" {
		t.Fatalf("last line is %q, want the access line", last)
	}
	return event
}

// The gate wraps the API as well as the pages, and leaves only the health
// checks and the assets the login is drawn with open.
func TestGateCoversEverythingButHealthAndAssets(t *testing.T) {
	srv, db := newTestServer(t)
	if err := db.SetPassword(context.Background(), "alice", "correct horse"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	tests := []struct {
		method, path string
		wantStatus   int
		wantLocation string
	}{
		{http.MethodGet, "/feed", http.StatusSeeOther, "/login?next=%2Ffeed"},
		{http.MethodGet, "/", http.StatusSeeOther, "/login"},
		{http.MethodGet, "/api/items", http.StatusUnauthorized, ""},
		{http.MethodPost, "/api/items/1/seen", http.StatusUnauthorized, ""},
		{http.MethodPost, "/todos", http.StatusUnauthorized, ""},
		{http.MethodPost, "/profiles", http.StatusUnauthorized, ""},
		{http.MethodDelete, "/profiles/profile-example", http.StatusUnauthorized, ""},
		{http.MethodGet, "/healthz", http.StatusOK, ""},
		{http.MethodGet, "/readyz", http.StatusOK, ""},
		{http.MethodGet, "/static/style.css", http.StatusOK, ""},
		{http.MethodGet, "/login", http.StatusOK, ""},
	}
	routes := srv.Routes()
	for _, tt := range tests {
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, nil))
		if rec.Code != tt.wantStatus {
			t.Errorf("%s %s = %d, want %d", tt.method, tt.path, rec.Code, tt.wantStatus)
		}
		if got := rec.Header().Get("Location"); got != tt.wantLocation {
			t.Errorf("%s %s Location = %q, want %q", tt.method, tt.path, got, tt.wantLocation)
		}
	}
}

// A state-changing request another site's page makes is refused before any
// handler sees it, the login included.
func TestCrossOriginPostsAreRefused(t *testing.T) {
	srv, _ := newTestServer(t)
	routes := srv.Routes()
	for _, path := range []string{"/todos", "/login", "/logout"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("title=x"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("cross-site POST %s = %d, want 403", path, rec.Code)
		}
	}
}

// Neither the access log nor the handlers' own logging may carry what a login
// sends or gets back: the username (where a password lands by mistake), the
// password, or the session token.
func TestLoginKeepsSecretsOutOfLogs(t *testing.T) {
	var appLog bytes.Buffer
	prev := log.Logger
	log.Logger = zerolog.New(&appLog)
	t.Cleanup(func() { log.Logger = prev })

	srv, accessLog := newLoggingServer(t)
	routes := srv.Routes()
	ctx := context.Background()
	db := srv.db
	if err := db.SetPassword(ctx, "canary-user", "canary-password"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	var token string
	for _, pass := range []string{"canary-wrong", "canary-password"} {
		form := url.Values{"username": {"canary-user"}, "password": {pass}}
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, req)
		for _, c := range rec.Result().Cookies() {
			if c.Name == "rh_session" && c.Value != "" {
				token = c.Value
			}
		}
	}
	if token == "" {
		t.Fatal("the right password did not start a session")
	}

	logged := accessLog.String() + appLog.String()
	for _, secret := range []string{"canary", token} {
		if strings.Contains(logged, secret) {
			t.Errorf("logs contain %q:\n%s", secret, logged)
		}
	}
}
