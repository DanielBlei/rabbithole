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
	"net"
	"net/http"
	"net/netip"
	"strings"
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
	src      config.Source
	addr     string // listen address, passed to the web UI for its shell prompt
	ing      *ingest.Manager
	log      zerolog.Logger
	draining atomic.Bool // set by Drain; makes /readyz report 503

	proxies    []netip.Prefix // set by TrustProxies
	proxiesSet bool
	dev        bool            // set by Dev: serve assets no-cache
	managedBy  string          // set by ManagedBy: the --config file, applied when it changes
	env        *config.EnvFile // set by EnvFile: where api key variables come from and pasted keys go
	noEnvWrite bool            // set by EnvFile: refuse pasted keys
	storeInfo  *web.StoreInfo  // set by StoreInfo: what boot knew about the store
}

// New returns a Server backed by db, reading the run config from src. addr is the
// bound listen address, forwarded to the web UI for display only. ing owns the
// in-process ingest runs the web UI triggers. log backs the access log.
func New(
	db *store.Store, src config.Source, addr string,
	ing *ingest.Manager, log zerolog.Logger,
) *Server {
	return &Server{db: db, src: src, addr: addr, ing: ing, log: log}
}

// TrustProxies sets the networks whose forwarding headers the login gate
// believes, in place of its loopback default. Empty trusts none.
func (s *Server) TrustProxies(nets []netip.Prefix) {
	s.proxies, s.proxiesSet = nets, true
}

// ManagedBy names the --config file, shown on the settings page.
func (s *Server) ManagedBy(path string) { s.managedBy = path }

// EnvFile gives the settings page the .env file: it tells an api key variable
// from the environment apart from one read out of .env, and takes a pasted key
// unless noWrite is set.
func (s *Server) EnvFile(e *config.EnvFile, noWrite bool) { s.env, s.noEnvWrite = e, noWrite }

// StoreInfo hands the settings page the store boot opened.
func (s *Server) StoreInfo(i web.StoreInfo) { s.storeInfo = &i }

// Dev serves the embedded assets no-cache, so an edited stylesheet or script
// shows on the next reload instead of after the normal cache window.
func (s *Server) Dev(on bool) { s.dev = on }

// Routes builds the root handler. The API sub-mux keeps its full /api/ patterns,
// so mounting it under "/api/" (no StripPrefix) lets its method+path patterns
// match unchanged.
func (s *Server) Routes() http.Handler {
	w := web.New(s.db, s.src, s.ing)
	if s.proxiesSet {
		w.SetTrustedProxies(s.proxies)
	}
	w.SetDev(s.dev)
	w.SetManagedBy(s.managedBy)
	w.SetEnvFile(s.env, s.noEnvWrite)
	if s.storeInfo != nil {
		w.SetStoreInfo(*s.storeInfo)
	}
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
	// A loopback listener also answers only to loopback names: a page on
	// another domain that rebinds its DNS to 127.0.0.1 would otherwise read the
	// app as same-origin.
	if loopbackAddr(s.addr) {
		protected = loopbackHostOnly(protected)
	}
	// Compression sits inside the access log, so the bytes a request reports
	// are the bytes that went on the wire rather than the ones a handler wrote.
	return httplog.Middleware(s.log)(httpgzip.Middleware(protected))
}

// loopbackAddr reports whether a listen address binds only the loopback
// interface. A bare port or an unspecified address binds every interface.
func loopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	return loopbackName(host)
}

// loopbackName reports whether a host is localhost or a loopback IP literal.
func loopbackName(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

// loopbackHostOnly refuses a request whose Host header names anything but a
// loopback host. A missing Host (HTTP/1.0) passes.
func loopbackHostOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.Trim(host, "[]")
		if host != "" && !loopbackName(host) {
			http.Error(w, "Host not allowed. Use localhost or 127.0.0.1.", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
