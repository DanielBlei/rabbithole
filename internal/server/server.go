// Package server is the HTTP composition root for the serve command: it builds
// the root mux and mounts the route sets that live in their own packages — the
// JSON API (internal/api) under /api/, and the HTML web UI (internal/web,
// serving its own /static/ assets) at /.
package server

import (
	"net/http"

	"github.com/DanielBlei/ai-searcher/internal/api"
	"github.com/DanielBlei/ai-searcher/internal/config"
	"github.com/DanielBlei/ai-searcher/internal/store"
	"github.com/DanielBlei/ai-searcher/internal/web"
)

// Server holds the dependencies shared by every mounted route set.
type Server struct {
	db   *store.Store
	cfg  *config.Config
	addr string // listen address, passed to the web UI for its shell prompt
}

// New returns a Server backed by db, using cfg for request defaults. addr is the
// bound listen address, forwarded to the web UI for display only.
func New(db *store.Store, cfg *config.Config, addr string) *Server {
	return &Server{db: db, cfg: cfg, addr: addr}
}

// Routes builds the root handler. The API sub-mux keeps its full /api/ patterns,
// so mounting it under "/api/" (no StripPrefix) lets its method+path patterns
// match unchanged.
func (s *Server) Routes() http.Handler {
	root := http.NewServeMux()
	root.Handle("/api/", api.New(s.db, s.cfg).Routes())
	// The web mux owns "/" (digest page) and "/static/" (embedded assets);
	// the more specific "/api/" pattern above still wins for API requests.
	root.Handle("/", web.New(s.db, s.cfg, s.addr).Routes())
	return root
}
