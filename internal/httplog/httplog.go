// Package httplog is the HTTP access log: one middleware that records every
// request's outcome and threads a request-scoped logger through the context.
package httplog

import (
	"context"
	"math"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
)

// reqCounter numbers requests for the log's "req" field. A counter rather than
// a UUID: the only reader is a human watching a terminal.
var reqCounter atomic.Uint64

// state is the per-request scratch the middleware keeps outside the logger, so
// a handler can influence the access line that is written after it returns.
type state struct {
	quiet atomic.Bool
}

type ctxKey struct{}

// Quiet marks r as routine traffic: its access line drops to debug level.
// For poll targets and assets, called by the package that owns the route.
// A 4xx or 5xx outcome overrides it — quiet suppresses boredom, not problems.
func Quiet(r *http.Request) {
	if st, ok := r.Context().Value(ctxKey{}).(*state); ok {
		st.quiet.Store(true)
	}
}

// QuietHandler is Quiet for a whole handler, for routes with no body of their
// own to call it from (static assets, health checks).
func QuietHandler(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		Quiet(r)
		h.ServeHTTP(w, r)
	})
}

// Middleware returns the access-log middleware over logger.
func Middleware(logger zerolog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			id := reqCounter.Add(1)
			st := &state{}

			// The request logger carries "req" into whatever the handler logs,
			// so handler lines join to the access line below.
			reqLog := logger.With().Uint64("req", id).Logger()
			ctx := context.WithValue(reqLog.WithContext(r.Context()), ctxKey{}, st)

			rec := &recorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r.WithContext(ctx))

			reqLog.WithLevel(levelFor(rec.status, st.quiet.Load())).
				Str("method", r.Method).
				Str("path", r.URL.Path).
				Int("status", rec.status).
				Int64("bytes", rec.bytes).
				Float64("dur_ms", durMS(time.Since(start))).
				Msg("request")
		})
	}
}

// durMS reports d in milliseconds to one decimal. Anything finer is noise in an
// access log; a float rather than a rounded-up integer keeps the field sortable
// and avoids reporting a sub-millisecond request as a whole millisecond.
func durMS(d time.Duration) float64 {
	return math.Round(float64(d.Microseconds())/100) / 10
}

// levelFor grades an access line by outcome, letting failures escape quiet.
func levelFor(status int, quiet bool) zerolog.Level {
	switch {
	case status >= http.StatusInternalServerError:
		return zerolog.ErrorLevel
	case status >= http.StatusBadRequest:
		return zerolog.WarnLevel
	case quiet:
		return zerolog.DebugLevel
	default:
		return zerolog.InfoLevel
	}
}

// recorder captures the status and size the handler wrote. Unwrap keeps
// http.ResponseController working through the wrapper (Flush, Hijack, deadlines).
type recorder struct {
	http.ResponseWriter
	status  int
	bytes   int64
	written bool
}

func (rec *recorder) Unwrap() http.ResponseWriter { return rec.ResponseWriter }

func (rec *recorder) WriteHeader(status int) {
	if rec.written {
		return
	}
	rec.status = status
	rec.written = true
	rec.ResponseWriter.WriteHeader(status)
}

func (rec *recorder) Write(b []byte) (int, error) {
	rec.written = true
	n, err := rec.ResponseWriter.Write(b)
	rec.bytes += int64(n)
	return n, err
}
