// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package inference

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/feeds"
)

func TestResolveClassifier(t *testing.T) {
	t.Run("heuristic never dials out", func(t *testing.T) {
		cfg := config.ClassificationConfig{Provider: "heuristic", Host: "http://127.0.0.1:1"}
		c, err := ResolveClassifier(t.Context(), cfg, nil)
		if err != nil {
			t.Fatalf("ResolveClassifier() error = %v", err)
		}
		if c == nil {
			t.Fatal("ResolveClassifier() = nil")
		}
	})

	t.Run("unknown provider is rejected before touching a backend", func(t *testing.T) {
		_, err := ResolveClassifier(t.Context(), config.ClassificationConfig{Provider: "carrierpigeon"}, nil)
		if err == nil || !strings.Contains(err.Error(), `unknown provider "carrierpigeon"`) {
			t.Fatalf("ResolveClassifier() error = %v", err)
		}
	})

	t.Run("ollama dispatches and validates", func(t *testing.T) {
		host := serveJSON(t, `{"models":[{"model":"llama3:latest"}]}`)
		cfg := config.ClassificationConfig{
			Provider: "ollama", Host: host, Model: "llama3:latest",
		}
		if _, err := ResolveClassifier(t.Context(), cfg, nil); err != nil {
			t.Fatalf("ResolveClassifier() error = %v", err)
		}
	})
}

func TestClassifierParsesStructuredOutput(t *testing.T) {
	var request map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"data":[{"id":"llama3"}]}`))
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		_, _ = w.Write(
			[]byte(
				`{"choices":[{"message":{"content":"{\"classifications\":[{\"index\":1,\"keywords\":[\"vLLM\"],\"assumptions\":[\"inference\"]}]}"},"finish_reason":"stop"}]}`,
			),
		)
	}))
	t.Cleanup(srv.Close)

	cfg := config.ClassificationConfig{
		Provider: "vllm", Host: srv.URL, Model: "llama3", StructuredOutput: boolp(true),
	}
	c, err := ResolveClassifier(t.Context(), cfg, []string{"vllm"})
	if err != nil {
		t.Fatalf("ResolveClassifier: %v", err)
	}
	got, err := c.Classify(t.Context(), []feeds.Item{{ID: "a", Title: "vLLM", Summary: "serving"}})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if len(got) != 1 || len(got[0].Keywords) != 1 || got[0].Keywords[0] != "vLLM" {
		t.Fatalf("verdicts = %+v", got)
	}
	if got[0].Assumptions[0] != "inference" {
		t.Errorf("assumptions = %v", got[0].Assumptions)
	}

	format, _ := request["response_format"].(map[string]any)
	schema, _ := format["json_schema"].(map[string]any)
	if schema["name"] != "classifications" {
		t.Errorf("schema name = %v", schema["name"])
	}
	if !strings.Contains(string(mustJSON(t, schema["schema"])), `"maxItems":8`) {
		t.Errorf("classification schema not sent: %v", schema["schema"])
	}
	messages, _ := request["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("messages = %v, want system and user", messages)
	}
	system, _ := messages[0].(map[string]any)
	if system["role"] != "system" || !strings.Contains(system["content"].(string), "content classifier") {
		t.Errorf("system message = %v", system)
	}
}

func boolp(v bool) *bool { return &v }

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
