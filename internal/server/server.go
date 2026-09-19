// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

// Package server is the HTTP composition root for the serve command: it builds
// the root mux, wraps it in compression and the access log, and mounts the
// route sets that live
// in their own packages — the JSON API (internal/api) under /api/, and the HTML
// web UI (internal/web, serving its own /static/ assets) at /, both behind the
// web UI's login gate. The health endpoints are its own, since serving
// lifecycle is not a web or API concern.
package server

import (
	"net/http"
	"net/netip"
	"sync/atomic"

	"github.com/rs/zerolog"

	"github.com/DanielBlei/rabbithole/internal/api"
	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/httpgzip"
	"github.com/DanielBlei/rabbithole/internal/httplog"
	"github.com/DanielBlei/rabbithole/internal/ingest"
	"github.com/DanielBlei/rabbithole/internal/store"
	"github.com/DanielBlei/rabbithole/internal/web"
)

// Server holds the dependencies shared by every mounted route set.
type Server struct {
	db       *store.Store
	cfg      *config.Config
	addr     string // listen address, passed to the web UI for its shell prompt
	cfgPath  string // config file path, surfaced by the web UI's config viewer
	ing      *ingest.Manager
	log      zerolog.Logger
	draining atomic.Bool // set by Drain; makes /readyz report 503

	proxies    []netip.Prefix // set by TrustProxies
	proxiesSet bool
	dev        bool // set by Dev: serve assets no-cache
}

// New returns a Server backed by db, using cfg for request defaults. addr is the
// bound listen address, forwarded to the web UI for display only. cfgPath is the
// loaded config's path, shown read-only by the web UI's config viewer. ing owns
// the in-process ingest runs the web UI triggers. log backs the access log.
func New(
	db *store.Store, cfg *config.Config, addr, cfgPath string,
	ing *ingest.Manager, log zerolog.Logger,
) *Server {
	return &Server{db: db, cfg: cfg, addr: addr, cfgPath: cfgPath, ing: ing, log: log}
}

// TrustProxies sets the networks whose forwarding headers the login gate
// believes, in place of its loopback default. Empty trusts none.
func (s *Server) TrustProxies(nets []netip.Prefix) {
	s.proxies, s.proxiesSet = nets, true
}

// Dev serves the embedded assets no-cache, so an edited stylesheet or script
// shows on the next reload instead of after the normal cache window.
func (s *Server) Dev(on bool) { s.dev = on }

// Routes builds the root handler. The API sub-mux keeps its full /api/ patterns,
// so mounting it under "/api/" (no StripPrefix) lets its method+path patterns
// match unchanged.
func (s *Server) Routes() http.Handler {
	w := web.New(s.db, s.cfg, s.cfgPath, s.ing)
	if s.proxiesSet {
		w.SetTrustedProxies(s.proxies)
	}
	w.SetDev(s.dev)
	app := http.NewServeMux()
	app.Handle("/api/", api.New(s.db).Routes())
	// The web mux owns "/" (digest page) and "/static/" (embedded assets);
	// the more specific /api/ pattern still wins for its own requests.
	app.Handle("/", w.Routes())

	root := http.NewServeMux()
	// Health checks are polled on a timer by Docker and the like, so they are
	// quiet by default for the same reason the ingest poll target is. They sit
	// outside the login gate, which covers everything else, the API included.
	root.Handle("GET /healthz", httplog.QuietHandler(http.HandlerFunc(s.handleHealthz)))
	root.Handle("GET /readyz", httplog.QuietHandler(http.HandlerFunc(s.handleReadyz)))
	root.Handle("/", w.Gate(app))
	// Cross-origin protection refuses state-changing requests another site's
	// page makes on a logged-in browser's behalf; SameSite=Lax on the session
	// cookie is the second line.
	protected := http.NewCrossOriginProtection().Handler(root)
	// Compression sits inside the access log, so the bytes a request reports
	// are the bytes that went on the wire rather than the ones a handler wrote.
	return httplog.Middleware(s.log)(httpgzip.Middleware(protected))
}
