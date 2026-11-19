// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package ollama

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/DanielBlei/rabbithole/internal/rank"
)

// TestNegotiateThinkReusesScoringCapability pins finding 11 for Ollama: the
// scoring client's one-time probe is cached, a brief client constructed later
// inherits it, and a dive does not probe again. An unsupported model must not
// make every brief send a think request Ollama rejects.
func TestNegotiateThinkReusesScoringCapability(t *testing.T) {
	for _, tc := range []struct {
		name       string
		supports   bool
		wantThink  bool
		wantProbes int32
	}{
		{name: "supported model keeps think on", supports: true, wantThink: true, wantProbes: 1},
		{name: "unsupported model turns think off", supports: false, wantThink: false, wantProbes: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withFastValidateRetry(t)
			resetThinkCapabilities()

			var probes, thinkCalls int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/api/chat") {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"models":[{"model":"llama3:latest"}]}`))
					return
				}
				var req struct {
					Think   *bool          `json:"think"`
					Options map[string]any `json:"options"`
				}
				_ = json.NewDecoder(r.Body).Decode(&req)
				isProbe := req.Options["num_predict"] == float64(1)
				if isProbe {
					atomic.AddInt32(&probes, 1)
				}
				if req.Think != nil && *req.Think {
					atomic.AddInt32(&thinkCalls, 1)
					if !tc.supports {
						w.WriteHeader(http.StatusBadRequest)
						_, _ = w.Write([]byte(`{"error":"\"llama3:latest\" does not support thinking"}`))
						return
					}
				}
				w.Header().Set("Content-Type", "application/x-ndjson")
				_, _ = w.Write([]byte(`{"message":{"role":"assistant","content":"{}"},"done":true}` + "\n"))
			}))
			defer srv.Close()

			scoring, err := New(srv.URL, "llama3:latest", "", true, rank.ModelTuning{}, "")
			if err != nil {
				t.Fatalf("New scoring: %v", err)
			}
			if err := scoring.Validate(t.Context()); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if scoring.think != tc.wantThink {
				t.Fatalf("scoring think = %v after Validate, want %v", scoring.think, tc.wantThink)
			}

			brief, err := New(srv.URL, "llama3:latest", "", true, rank.ModelTuning{}, "")
			if err != nil {
				t.Fatalf("New brief: %v", err)
			}
			if brief.think != tc.wantThink {
				t.Fatalf("brief think = %v, want %v (scoring capability not reused)",
					brief.think, tc.wantThink)
			}
			brief.NegotiateThink(t.Context())
			if brief.think != tc.wantThink {
				t.Fatalf("brief think = %v after NegotiateThink, want %v", brief.think, tc.wantThink)
			}

			content, err := brief.ChatJSON(t.Context(), "system", "user",
				json.RawMessage(`{"type":"object"}`), 64)
			if err != nil {
				t.Fatalf("ChatJSON: %v", err)
			}
			if content != "{}" {
				t.Fatalf("content = %q, want {}", content)
			}
			if got := atomic.LoadInt32(&probes); got != tc.wantProbes {
				t.Fatalf("think probes = %d, want %d (negotiated once)", got, tc.wantProbes)
			}
			if tc.supports && atomic.LoadInt32(&thinkCalls) != 2 {
				t.Errorf("think requests = %d, want 2 (probe and synthesis)",
					atomic.LoadInt32(&thinkCalls))
			}
			if !tc.supports && atomic.LoadInt32(&thinkCalls) != 1 {
				t.Errorf("think requests = %d, want only the probe", atomic.LoadInt32(&thinkCalls))
			}
		})
	}
}

// TestChatJSONOmitsFormatWithoutSchema pins the Ollama half of finding 14: a
// nil schema is not sent as a null format.
func TestChatJSONOmitsFormatWithoutSchema(t *testing.T) {
	var got struct {
		Format json.RawMessage `json:"format"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"message":{"role":"assistant","content":"{}"},"done":true}` + "\n"))
	}))
	defer srv.Close()

	c, err := New(srv.URL, "llama3:latest", "", false, rank.ModelTuning{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ChatJSON(t.Context(), "system", "user", nil, 64); err != nil {
		t.Fatalf("ChatJSON: %v", err)
	}
	if len(got.Format) != 0 {
		t.Fatalf("format = %s, want it omitted for a nil schema", got.Format)
	}
}
