// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package inference

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DanielBlei/rabbithole/internal/cluster"
	"github.com/DanielBlei/rabbithole/internal/config"
)

func TestResolveEmbedderIsConfigGated(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  config.InferenceConfig
		want bool
	}{
		{name: "heuristic ignores embedding model", cfg: config.InferenceConfig{
			Provider: config.ProviderHeuristic, EmbeddingModel: "embed",
		}},
		{name: "model provider without embedding model", cfg: config.InferenceConfig{
			Provider: config.ProviderOllama, Host: "http://localhost:11434",
		}},
		{name: "ollama with embedding model", cfg: config.InferenceConfig{
			Provider: config.ProviderOllama, Host: "http://localhost:11434", EmbeddingModel: "embed",
		}, want: true},
		{name: "openai with embedding model", cfg: config.InferenceConfig{
			Provider: config.ProviderOpenAI, Host: "https://api.example.test", EmbeddingModel: "embed",
		}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveEmbedder(tc.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if (got != nil) != tc.want {
				t.Errorf("ResolveEmbedder = %v, want available=%v", got, tc.want)
			}
		})
	}
}

func TestOllamaEmbedder(t *testing.T) {
	var path, model string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		var body struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		model = body.Model
		_ = json.NewEncoder(w).Encode(map[string]any{
			"embeddings": [][]float64{{1, 0}, {0, 1}},
		})
	}))
	defer srv.Close()

	embedder, err := ResolveEmbedder(config.InferenceConfig{
		Provider: config.ProviderOllama, Host: srv.URL, EmbeddingModel: "embed-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := embedder.Embed(context.Background(), []string{"one", "two"})
	if err != nil {
		t.Fatal(err)
	}
	if path != "/api/embed" || model != "embed-model" {
		t.Errorf("request = %s %s, want /api/embed embed-model", path, model)
	}
	if len(got) != 2 || got[0][0] != 1 || got[1][1] != 1 {
		t.Errorf("embeddings = %v", got)
	}
}

func TestOpenAIEmbedderPreservesResponseIndexes(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"embedding": []float64{0, 1}, "index": 1},
				{"embedding": []float64{1, 0}, "index": 0},
			},
		})
	}))
	defer srv.Close()

	embedder, err := ResolveEmbedder(config.InferenceConfig{
		Provider: config.ProviderOpenAI, Host: srv.URL, EmbeddingModel: "embed-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := embedder.Embed(context.Background(), []string{"one", "two"})
	if err != nil {
		t.Fatal(err)
	}
	if path != "/v1/embeddings" {
		t.Errorf("path = %s, want /v1/embeddings", path)
	}
	if got[0][0] != 1 || got[1][1] != 1 {
		t.Errorf("embeddings were not ordered by response index: %v", got)
	}
}

func TestStoryVerifierUsesChatModel(t *testing.T) {
	var path, content string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		content = body.Messages[len(body.Messages)-1].Content
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]string{"content": `{"same":true}`},
			}},
		})
	}))
	defer srv.Close()

	verifier, err := ResolveStoryVerifier(context.Background(), config.InferenceConfig{
		Provider: config.ProviderOpenAI, Host: srv.URL, Model: "chat-model",
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	same, err := verifier.SameDevelopment(context.Background(),
		cluster.Item{Title: "First headline", Source: "S1"},
		cluster.Item{Title: "Second headline", Source: "S2"})
	if err != nil {
		t.Fatal(err)
	}
	if !same {
		t.Error("SameDevelopment = false, want the model's true")
	}
	if path != "/v1/chat/completions" || !strings.Contains(content, "First headline") ||
		!strings.Contains(content, "Second headline") {
		t.Errorf("request path = %s, prompt = %q", path, content)
	}
}

func TestHeuristicHasNoStoryVerifier(t *testing.T) {
	got, err := ResolveStoryVerifier(context.Background(), config.InferenceConfig{
		Provider: config.ProviderHeuristic,
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("heuristic verifier = %v, want nil", got)
	}
}
