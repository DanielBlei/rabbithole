// Package server is the HTTP composition root for the serve command: it builds
// the root mux and mounts the route sets that live in their own packages. Today
// it mounts the JSON API (internal/api) under /api/; the HTML web UI
// (internal/web) and its /static/ assets are mounted here in a later pass.
package server

import (
	"net/http"

	"github.com/DanielBlei/ai-searcher/internal/api"
	"github.com/DanielBlei/ai-searcher/internal/config"
	"github.com/DanielBlei/ai-searcher/internal/store"
)

// Server holds the dependencies shared by every mounted route set.
type Server struct {
	db  *store.Store
	cfg *config.Config
}

// New returns a Server backed by db, using cfg for request defaults.
func New(db *store.Store, cfg *config.Config) *Server {
	return &Server{db: db, cfg: cfg}
}

// Routes builds the root handler. The API sub-mux keeps its full /api/ patterns,
// so mounting it under "/api/" (no StripPrefix) lets its method+path patterns
// match unchanged.
func (s *Server) Routes() http.Handler {
	root := http.NewServeMux()
	root.Handle("/api/", api.New(s.db, s.cfg).Routes())
	// TODO: mount the HTML web UI (internal/web) at "/" and its embedded
	// /static/ assets once the digest page is wired up.
	return root
}
