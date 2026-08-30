// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package inference

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DanielBlei/rabbithole/internal/config"
)

// serveJSON starts a test server that always answers body, regardless of path.
func serveJSON(t *testing.T, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestResolve(t *testing.T) {
	t.Run("ollama dispatches and validates against the real model list shape", func(t *testing.T) {
		host := serveJSON(t, `{"models":[{"model":"llama3:latest"}]}`)
		cfg := config.InferenceConfig{Provider: "ollama", Host: host, Model: "llama3:latest"}
		s, err := Resolve(t.Context(), cfg, false, "be nice")
		if err != nil {
			t.Fatalf("Resolve() error = %v", err)
		}
		if s == nil {
			t.Fatal("Resolve() scorer = nil, want a scorer")
		}
	})

	t.Run("vllm dispatches and validates against the real model list shape", func(t *testing.T) {
		host := serveJSON(t, `{"data":[{"id":"llama3"}]}`)
		cfg := config.InferenceConfig{Provider: "vllm", Host: host, Model: "llama3"}
		s, err := Resolve(t.Context(), cfg, false, "be nice")
		if err != nil {
			t.Fatalf("Resolve() error = %v", err)
		}
		if s == nil {
			t.Fatal("Resolve() scorer = nil, want a scorer")
		}
	})

	t.Run("heuristic never dials out", func(t *testing.T) {
		cfg := config.InferenceConfig{Provider: "heuristic", Host: "http://127.0.0.1:1"}
		s, err := Resolve(t.Context(), cfg, false, "")
		if err != nil {
			t.Fatalf("Resolve() error = %v, want nil (heuristic has no backend to reach)", err)
		}
		if s == nil {
			t.Fatal("Resolve() scorer = nil, want a scorer")
		}
	})

	t.Run("unknown provider is rejected before touching a backend", func(t *testing.T) {
		cfg := config.InferenceConfig{Provider: "carrierpigeon"}
		_, err := Resolve(t.Context(), cfg, false, "")
		if err == nil || !strings.Contains(err.Error(), `unknown provider "carrierpigeon"`) {
			t.Fatalf("Resolve() error = %v, want it to name the bad provider", err)
		}
	})

	t.Run("a backend construction error is wrapped, not swallowed", func(t *testing.T) {
		// Ollama requires an explicit tag on the model name; this fails in New,
		// before any network call, so it needs no test server.
		cfg := config.InferenceConfig{Provider: "ollama", Host: "http://127.0.0.1:1", Model: "no-tag"}
		_, err := Resolve(t.Context(), cfg, false, "")
		if err == nil || !strings.Contains(err.Error(), "backend init") {
			t.Fatalf("Resolve() error = %v, want it to mention backend init", err)
		}
	})
}
