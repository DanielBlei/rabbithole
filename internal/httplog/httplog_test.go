// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package httplog

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// capture runs one request through the middleware and returns the decoded
// access-log event it wrote.
func capture(t *testing.T, r *http.Request, h http.HandlerFunc) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	logger := zerolog.New(&buf).Level(zerolog.DebugLevel)
	Middleware(logger)(h).ServeHTTP(httptest.NewRecorder(), r)

	var event map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &event); err != nil {
		t.Fatalf("decoding log event %q: %v", buf.String(), err)
	}
	return event
}

func TestLogsRequestOutcome(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/ingest/run", nil)
	event := capture(t, req, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("hello"))
	})

	want := map[string]any{
		"level":   "info",
		"method":  http.MethodPost,
		"path":    "/ingest/run",
		"status":  float64(http.StatusCreated),
		"bytes":   float64(5),
		"message": "request",
	}
	for k, v := range want {
		if event[k] != v {
			t.Errorf("%s = %v, want %v", k, event[k], v)
		}
	}
	// req is a process-wide counter, so only its presence is stable here.
	if _, ok := event["req"]; !ok {
		t.Error("no req field on the access line")
	}
	// The value depends on the machine; only that it is a number is stable.
	if _, ok := event["dur_ms"].(float64); !ok {
		t.Errorf("dur_ms = %v, want a number", event["dur_ms"])
	}
}

// A handler that never calls WriteHeader still reports 200, matching what the
// stdlib actually sends.
func TestImplicitStatusIsOK(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/feed", nil)
	event := capture(t, req, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("body"))
	})
	if event["status"] != float64(http.StatusOK) {
		t.Errorf("status = %v, want 200", event["status"])
	}
}

func TestLevelByOutcome(t *testing.T) {
	tests := []struct {
		name   string
		status int
		quiet  bool
		want   string
	}{
		{"success is info", http.StatusOK, false, "info"},
		{"quiet success is debug", http.StatusOK, true, "debug"},
		{"client error is warn", http.StatusNotFound, false, "warn"},
		{"server error is error", http.StatusInternalServerError, false, "error"},
		// Quiet suppresses boredom, not problems: a failing poll must resurface.
		{"quiet client error still warns", http.StatusBadRequest, true, "warn"},
		{"quiet server error still errors", http.StatusBadGateway, true, "error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/ingest/status", nil)
			event := capture(t, req, func(w http.ResponseWriter, r *http.Request) {
				if tt.quiet {
					Quiet(r)
				}
				w.WriteHeader(tt.status)
			})
			if event["level"] != tt.want {
				t.Errorf("level = %v, want %v", event["level"], tt.want)
			}
		})
	}
}

func TestQuietHandlerMarksWholeHandler(t *testing.T) {
	var buf bytes.Buffer
	logger := zerolog.New(&buf).Level(zerolog.DebugLevel)
	quiet := QuietHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/static/style.css", nil)
	Middleware(logger)(quiet).ServeHTTP(httptest.NewRecorder(), req)

	var event map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &event); err != nil {
		t.Fatalf("decoding log event: %v", err)
	}
	if event["level"] != "debug" {
		t.Errorf("level = %v, want debug", event["level"])
	}
}

// Quiet outside the middleware must not panic — it is a no-op when the request
// carries no per-request state.
func TestQuietWithoutMiddleware(t *testing.T) {
	Quiet(httptest.NewRequest(http.MethodGet, "/", nil))
}

// The handler's own lines must carry the same req id as the access line, which
// is the only thing that makes the two joinable in a shared terminal.
func TestHandlerLoggerCarriesRequestID(t *testing.T) {
	var buf bytes.Buffer
	logger := zerolog.New(&buf).Level(zerolog.DebugLevel)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		zerolog.Ctx(r.Context()).Info().Msg("from the handler")
	})
	req := httptest.NewRequest(http.MethodGet, "/feed", nil)
	Middleware(logger)(h).ServeHTTP(httptest.NewRecorder(), req)

	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("got %d log lines, want 2 (handler + access)", len(lines))
	}
	var handlerLine, accessLine map[string]any
	if err := json.Unmarshal(lines[0], &handlerLine); err != nil {
		t.Fatalf("decoding handler line: %v", err)
	}
	if err := json.Unmarshal(lines[1], &accessLine); err != nil {
		t.Fatalf("decoding access line: %v", err)
	}
	if handlerLine["req"] == nil || handlerLine["req"] != accessLine["req"] {
		t.Errorf("handler req = %v, access req = %v, want equal and set",
			handlerLine["req"], accessLine["req"])
	}
}

// Wrapping the ResponseWriter must not hide the interfaces the stdlib reaches
// for through http.ResponseController — Flush is the one SSE would need.
func TestRecorderKeepsResponseControllerWorking(t *testing.T) {
	var buf bytes.Buffer
	logger := zerolog.New(&buf)
	var flushErr error
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("chunk"))
		flushErr = http.NewResponseController(w).Flush()
	})
	req := httptest.NewRequest(http.MethodGet, "/feed", nil)
	Middleware(logger)(h).ServeHTTP(httptest.NewRecorder(), req)

	if flushErr != nil {
		t.Errorf("Flush through the wrapper: %v", flushErr)
	}
}

func TestDurMS(t *testing.T) {
	tests := []struct {
		name string
		in   time.Duration
		want float64
	}{
		{"zero", 0, 0},
		{"below the last decimal rounds away", 26 * time.Microsecond, 0},
		{"tenths survive", 58 * time.Microsecond, 0.1},
		{"just under a ms", 999 * time.Microsecond, 1},
		{"exactly 1ms", time.Millisecond, 1},
		{"rounds to one decimal", 1978 * time.Microsecond, 2},
		{"keeps the decimal when it matters", 1940 * time.Microsecond, 1.9},
		{"whole ms pass through", 12 * time.Millisecond, 12},
		{"seconds report as ms", 2450500 * time.Microsecond, 2450.5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := durMS(tt.in); got != tt.want {
				t.Errorf("durMS(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestRequestIDsAreDistinct(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {})
	first := capture(t, httptest.NewRequest(http.MethodGet, "/feed", nil), h)
	second := capture(t, httptest.NewRequest(http.MethodGet, "/feed", nil), h)
	if first["req"] == second["req"] {
		t.Errorf("both requests logged req=%v, want distinct ids", first["req"])
	}
}
