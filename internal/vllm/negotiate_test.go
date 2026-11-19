// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package vllm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/DanielBlei/rabbithole/internal/rank"
)

// TestNegotiateEffortReusesScoringCapability pins finding 11 for OpenAI: the
// scoring client's one-time effort negotiation is cached, a brief client built
// later inherits the accepted value, and a dive does not probe again or send an
// effort the model rejects.
func TestNegotiateEffortReusesScoringCapability(t *testing.T) {
	withFastValidateRetry(t)
	resetEffortCapabilities()

	var probes int32
	var seen []string
	var synthesis map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt"}]}`))
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if isEffortProbe(body) {
			atomic.AddInt32(&probes, 1)
			effort, _ := body["reasoning_effort"].(string)
			seen = append(seen, effort)
			if effort == "high" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"unsupported reasoning_effort"}`))
				return
			}
			_, _ = w.Write([]byte(`{}`))
			return
		}
		synthesis = body
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"ok\":true}"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	scoring, err := New(OpenAI, srv.URL, "gpt", "", true, "high", rank.ModelTuning{}, "")
	if err != nil {
		t.Fatalf("New scoring: %v", err)
	}
	if err := scoring.Validate(t.Context()); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if scoring.sendEffort != "" || !scoring.effortResolved {
		t.Fatalf("scoring effort = %q (resolved=%v), want the no-field fallback",
			scoring.sendEffort, scoring.effortResolved)
	}

	brief, err := New(OpenAI, srv.URL, "gpt", "", true, "high", rank.ModelTuning{}, "")
	if err != nil {
		t.Fatalf("New brief: %v", err)
	}
	if brief.sendEffort != "" || !brief.effortResolved {
		t.Fatalf("brief effort = %q (resolved=%v), want the cached fallback",
			brief.sendEffort, brief.effortResolved)
	}
	if err := brief.NegotiateEffort(t.Context()); err != nil {
		t.Fatalf("NegotiateEffort: %v", err)
	}
	content, err := brief.ChatJSON(t.Context(), "system", "user",
		json.RawMessage(`{"type":"object"}`), 64)
	if err != nil {
		t.Fatalf("ChatJSON: %v", err)
	}
	if content != `{"ok":true}` {
		t.Fatalf("content = %q", content)
	}
	if got := atomic.LoadInt32(&probes); got != 2 {
		t.Fatalf("effort probes = %d, want 2 (high rejected, no-field accepted)", got)
	}
	if !slices.Equal(seen, []string{"high", ""}) {
		t.Fatalf("probed efforts = %q, want [high \"\"]", seen)
	}
	if effort, ok := synthesis["reasoning_effort"]; ok {
		t.Fatalf("synthesis sent reasoning_effort = %v, want it omitted", effort)
	}
}

// isEffortProbe recognizes the fixed chat probe sent by probeModel.
func isEffortProbe(body map[string]any) bool {
	messages, _ := body["messages"].([]any)
	if len(messages) != 1 {
		return false
	}
	message, _ := messages[0].(map[string]any)
	content, _ := message["content"].(string)
	return content == "Hi, how are you?"
}

// TestChatJSONOmitsResponseFormatWithoutSchema pins finding 14: a nil schema
// must not be marshaled as a strict json_schema with "schema": null.
func TestChatJSONOmitsResponseFormatWithoutSchema(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{}"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	c, err := New(VLLM, srv.URL, "model", "", false, "", rank.ModelTuning{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ChatJSON(t.Context(), "system", "user", nil, 64); err != nil {
		t.Fatalf("ChatJSON: %v", err)
	}
	if format, ok := got["response_format"]; ok {
		t.Fatalf("response_format = %v, want it omitted for a nil schema", format)
	}
}
