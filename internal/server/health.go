// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/rs/zerolog"
)

// pingTimeout bounds the readiness check's store probe, so a wedged database
// answers 503 rather than hanging the caller.
const pingTimeout = 2 * time.Second

// handleHealthz is liveness: it answers for the process alone and deliberately
// touches nothing else. A dependency failure is not fixed by a restart, so
// making this check the store would only turn an outage into a crash loop.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeHealth(w, http.StatusOK, "ok")
}

// handleReadyz is readiness: whether this instance should receive traffic.
// Only the store gates it — an in-flight ingest run is normal operation, not
// degradation, so it deliberately does not consult the ingest manager.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if s.draining.Load() {
		writeHealth(w, http.StatusServiceUnavailable, "shutting down")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), pingTimeout)
	defer cancel()
	if err := s.db.Ping(ctx); err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Msg("readiness check failed")
		writeHealth(w, http.StatusServiceUnavailable, "store unreachable")
		return
	}
	writeHealth(w, http.StatusOK, "ok")
}

// Drain flips readiness to 503 before the HTTP server stops accepting, giving
// a proxy or orchestrator a window to route away while in-flight requests finish.
func (s *Server) Drain() { s.draining.Store(true) }

func writeHealth(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// The status is already written, so a failed encode can only be ignored.
	_ = json.NewEncoder(w).Encode(map[string]string{"status": msg})
}
