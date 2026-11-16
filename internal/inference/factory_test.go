// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package inference

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/decisionbrief"
	"github.com/DanielBlei/rabbithole/internal/feeds"
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

	for _, provider := range []string{"openai", "vllm"} {
		t.Run(provider+" dispatches and validates against the real model list shape", func(t *testing.T) {
			host := serveJSON(t, `{"data":[{"id":"llama3"}]}`)
			cfg := config.InferenceConfig{Provider: provider, Host: host, Model: "llama3"}
			s, err := Resolve(t.Context(), cfg, false, "be nice")
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if s == nil {
				t.Fatal("Resolve() scorer = nil, want a scorer")
			}
		})
	}

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

func TestResolveHonoursStructuredOutput(t *testing.T) {
	for _, structured := range []bool{true, false} {
		var scoring map[string]any
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(`{"data":[{"id":"llama3"}]}`))
				return
			}
			_ = json.NewDecoder(r.Body).Decode(&scoring)
			_, _ = w.Write([]byte(
				`{"choices":[{"message":{"content":"{\"scores\":[{\"index\":1,\"score\":9,\"reason\":\"Relevant.\"}]}"},"finish_reason":"stop"}]}`,
			))
		}))
		t.Cleanup(srv.Close)

		cfg := config.InferenceConfig{Provider: "vllm", Host: srv.URL, Model: "llama3", StructuredOutput: &structured}
		s, err := Resolve(t.Context(), cfg, true, "be nice")
		if err != nil {
			t.Fatalf("Resolve() error = %v", err)
		}
		if _, err := s.Score(t.Context(), "profile", []feeds.Item{{ID: "a", Title: "A"}}); err != nil {
			t.Fatalf("Score() error = %v", err)
		}
		if _, sent := scoring["response_format"]; sent != structured {
			t.Errorf("structured_output=%v: response_format sent = %v", structured, sent)
		}
	}
}

func TestResolveExpandsAPIKeyReferences(t *testing.T) {
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"llama3"}]}`))
	}))
	t.Cleanup(srv.Close)

	t.Run("references reach the client resolved", func(t *testing.T) {
		auth = nil
		t.Setenv("RABBITHOLE_TEST_KEY", "main-secret")
		cfg := config.InferenceConfig{
			Provider: "vllm", Host: srv.URL, Model: "llama3", APIKey: "$RABBITHOLE_TEST_KEY",
		}
		if _, err := Resolve(t.Context(), cfg, false, ""); err != nil {
			t.Fatalf("Resolve() error = %v", err)
		}
		if len(auth) != 1 || auth[0] != "Bearer main-secret" {
			t.Errorf("Authorization headers = %v, want the resolved key", auth)
		}
	})

	t.Run("an unset reference fails naming the field only", func(t *testing.T) {
		cfg := config.InferenceConfig{
			Provider: "vllm", Host: srv.URL, Model: "llama3", APIKey: "$RABBITHOLE_TEST_UNSET",
		}
		_, err := Resolve(t.Context(), cfg, false, "")
		if err == nil || !strings.Contains(err.Error(), "inference.api_key") {
			t.Fatalf("Resolve() error = %v, want it to name inference.api_key", err)
		}
	})
}

func TestResolveBrief(t *testing.T) {
	for _, tt := range []struct{ provider, model string }{{"ollama", "m:latest"}, {"openai", "m"}, {"vllm", "m"}} {
		provider := tt.provider
		t.Run(provider+" builds a generator and names its provenance", func(t *testing.T) {
			cfg := config.InferenceConfig{Provider: provider, Host: "http://127.0.0.1:1", Model: tt.model}
			gen, prov, err := ResolveBrief(t.Context(), cfg, false)
			if err != nil {
				t.Fatalf("ResolveBrief: %v", err)
			}
			if gen == nil {
				t.Fatal("generator is nil")
			}
			if prov.Provider != provider || prov.Model != tt.model {
				t.Fatalf("provenance = %+v, want %s/%s", prov, provider, tt.model)
			}
		})
	}
	t.Run("heuristic is unavailable", func(t *testing.T) {
		gen, prov, err := ResolveBrief(t.Context(), config.InferenceConfig{Provider: "heuristic"}, false)
		if gen != nil || prov != (decisionbrief.Provenance{}) || !errors.Is(err, decisionbrief.ErrBriefUnavailable) {
			t.Fatalf("gen = %v, prov = %+v, err = %v; want ErrBriefUnavailable", gen, prov, err)
		}
	})
	t.Run("unknown provider", func(t *testing.T) {
		if _, _, err := ResolveBrief(t.Context(), config.InferenceConfig{Provider: "nope"}, false); err == nil {
			t.Fatal("unknown provider accepted")
		}
	})
}

// TestResolveBriefStructuredOutput pins the factory's half of findings 12/14:
// structured output off must remove the provider's schema field entirely, not
// send a null schema, for both provider dialects.
func TestResolveBriefStructuredOutput(t *testing.T) {
	for _, provider := range []string{"ollama", "vllm"} {
		for _, structured := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s structured=%v", provider, structured), func(t *testing.T) {
				var sent map[string]any
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
						t.Errorf("decode request: %v", err)
					}
					w.Header().Set("Content-Type", "application/json")
					if provider == "ollama" {
						w.Header().Set("Content-Type", "application/x-ndjson")
						_, _ = w.Write([]byte(`{"message":{"role":"assistant","content":"{}"},"done":true}` + "\n"))
						return
					}
					_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{}"},"finish_reason":"stop"}]}`))
				}))
				t.Cleanup(srv.Close)

				cfg := config.InferenceConfig{
					Provider: provider, Host: srv.URL, Model: "model",
					StructuredOutput: &structured,
				}
				if provider == "ollama" {
					cfg.Model = "model:latest"
				}
				gen, _, err := ResolveBrief(t.Context(), cfg, false)
				if err != nil {
					t.Fatalf("ResolveBrief: %v", err)
				}
				if _, err := gen.ChatJSON(t.Context(), "system", "user", decisionbrief.Schema(), 128); err != nil {
					t.Fatalf("ChatJSON: %v", err)
				}
				schemaField := "response_format"
				if provider == "ollama" {
					schemaField = "format"
				}
				if _, sentSchema := sent[schemaField]; sentSchema != structured {
					t.Errorf("%s = %v, want present=%v; request=%v",
						schemaField, sent[schemaField], structured, sent)
				}
			})
		}
	}
}
