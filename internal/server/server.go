// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

// Package server is the HTTP composition root for the serve command: it builds
// the root mux, wraps it in the access log, and mounts the route sets that live
// in their own packages — the JSON API (internal/api) under /api/, and the HTML
// web UI (internal/web, serving its own /static/ assets) at /. The health
// endpoints are its own, since serving lifecycle is not a web or API concern.
package server

import (
	"net/http"
	"sync/atomic"

	"github.com/rs/zerolog"

	"github.com/DanielBlei/rabbithole/internal/api"
	"github.com/DanielBlei/rabbithole/internal/config"
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

// Routes builds the root handler. The API sub-mux keeps its full /api/ patterns,
// so mounting it under "/api/" (no StripPrefix) lets its method+path patterns
// match unchanged.
func (s *Server) Routes() http.Handler {
	root := http.NewServeMux()
	// Health checks are polled on a timer by Docker and the like, so they are
	// quiet by default for the same reason the ingest poll target is.
	root.Handle("GET /healthz", httplog.QuietHandler(http.HandlerFunc(s.handleHealthz)))
	root.Handle("GET /readyz", httplog.QuietHandler(http.HandlerFunc(s.handleReadyz)))
	root.Handle("/api/", api.New(s.db).Routes())
	// The web mux owns "/" (digest page) and "/static/" (embedded assets);
	// the more specific patterns above still win for their own requests.
	root.Handle("/", web.New(s.db, s.cfg, s.addr, s.cfgPath, s.ing).Routes())
	return httplog.Middleware(s.log)(root)
}
