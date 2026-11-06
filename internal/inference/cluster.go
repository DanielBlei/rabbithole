// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package inference

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/DanielBlei/rabbithole/internal/cluster"
	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/httpclient"
)

const (
	embeddingTimeout    = 2 * time.Minute
	verifyTimeout       = 90 * time.Second
	embeddingBatchSize  = 32
	embeddingMaxBody    = 2048
	embeddingHTTPHeader = "application/json"
)

// ResolveEmbedder returns the configured embedding client. An empty
// embedding_model or the heuristic provider leaves embeddings unavailable.
func ResolveEmbedder(cfg config.InferenceConfig) (cluster.Embedder, error) {
	if cfg.Provider == config.ProviderHeuristic || strings.TrimSpace(cfg.EmbeddingModel) == "" {
		return nil, nil
	}
	key, err := cfg.ResolvedAPIKey()
	if err != nil {
		return nil, err
	}
	switch cfg.Provider {
	case config.ProviderOllama, config.ProviderVLLM, config.ProviderOpenAI:
	default:
		return nil, fmt.Errorf("unknown provider %q", cfg.Provider)
	}
	return &embedder{
		provider: cfg.Provider,
		host:     strings.TrimRight(cfg.Host, "/"),
		model:    cfg.EmbeddingModel,
		hc: &http.Client{
			Transport: &httpclient.BearerTransport{Token: key, Base: http.DefaultTransport},
		},
	}, nil
}

// ResolveStoryVerifier returns a chat-model verifier. The heuristic provider has
// no model to ask, so it returns no verifier.
func ResolveStoryVerifier(
	_ context.Context,
	cfg config.InferenceConfig,
	think bool,
) (cluster.Verifier, error) {
	if cfg.Provider == config.ProviderHeuristic {
		return nil, nil
	}
	key, err := cfg.ResolvedAPIKey()
	if err != nil {
		return nil, err
	}
	switch cfg.Provider {
	case config.ProviderOllama, config.ProviderVLLM, config.ProviderOpenAI:
	default:
		return nil, fmt.Errorf("unknown provider %q", cfg.Provider)
	}
	return cluster.NewModelVerifier(&chatJSON{
		provider: cfg.Provider,
		host:     strings.TrimRight(cfg.Host, "/"),
		model:    cfg.Model,
		think:    think,
		hc: &http.Client{
			Transport: &httpclient.BearerTransport{Token: key, Base: http.DefaultTransport},
		},
	}), nil
}

type chatJSON struct {
	provider string
	host     string
	model    string
	think    bool
	hc       *http.Client
}

func (c *chatJSON) ChatJSON(
	ctx context.Context,
	system, user string,
	schema json.RawMessage,
	maxTokens int,
) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()
	messages := make([]map[string]string, 0, 2)
	if system != "" {
		messages = append(messages, map[string]string{"role": "system", "content": system})
	}
	messages = append(messages, map[string]string{"role": "user", "content": user})
	path := "/v1/chat/completions"
	body := map[string]any{
		"model":      c.model,
		"messages":   messages,
		"stream":     false,
		"max_tokens": maxTokens,
	}
	if c.provider == config.ProviderOllama {
		path = "/api/chat"
		body["format"] = schema
		body["options"] = map[string]any{"num_predict": maxTokens}
		body["think"] = c.think
	} else {
		body["response_format"] = map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name": "story_verification", "strict": true, "schema": schema,
			},
		}
		if c.provider == config.ProviderVLLM && !c.think {
			body["chat_template_kwargs"] = map[string]any{"enable_thinking": false}
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("marshal story verification request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.host+path, bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("build story verification request: %w", err)
	}
	req.Header.Set("Content-Type", embeddingHTTPHeader)
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("story verification request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, embeddingMaxBody))
		return "", fmt.Errorf("story verification endpoint returned %d: %s",
			resp.StatusCode, bytes.TrimSpace(msg))
	}
	var decoded struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return "", fmt.Errorf("decode story verification response: %w", err)
	}
	if decoded.Message.Content != "" {
		return decoded.Message.Content, nil
	}
	if len(decoded.Choices) > 0 {
		return decoded.Choices[0].Message.Content, nil
	}
	return "", fmt.Errorf("story verification endpoint returned no content")
}

type embedder struct {
	provider string
	host     string
	model    string
	hc       *http.Client
}

func (e *embedder) Embed(ctx context.Context, texts []string) ([][]float64, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	out := make([][]float64, 0, len(texts))
	for start := 0; start < len(texts); start += embeddingBatchSize {
		end := min(start+embeddingBatchSize, len(texts))
		got, err := e.embedBatch(ctx, texts[start:end])
		if err != nil {
			return nil, err
		}
		out = append(out, got...)
	}
	if len(out) != len(texts) {
		return nil, fmt.Errorf("embedding count %d does not match input count %d", len(out), len(texts))
	}
	return out, nil
}

func (e *embedder) embedBatch(ctx context.Context, texts []string) ([][]float64, error) {
	ctx, cancel := context.WithTimeout(ctx, embeddingTimeout)
	defer cancel()
	path := "/v1/embeddings"
	if e.provider == config.ProviderOllama {
		path = "/api/embed"
	}
	body, err := json.Marshal(map[string]any{"model": e.model, "input": texts})
	if err != nil {
		return nil, fmt.Errorf("marshal embedding request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.host+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build embedding request: %w", err)
	}
	req.Header.Set("Content-Type", embeddingHTTPHeader)
	resp, err := e.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedding request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, embeddingMaxBody))
		return nil, fmt.Errorf("embedding endpoint returned %d: %s", resp.StatusCode,
			bytes.TrimSpace(msg))
	}
	var decoded struct {
		Embeddings [][]float64 `json:"embeddings"`
		Embedding  []float64   `json:"embedding"`
		Data       []struct {
			Embedding []float64 `json:"embedding"`
			Index     *int      `json:"index"`
		} `json:"data"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decode embedding response: %w", err)
	}
	if decoded.Error != "" {
		return nil, fmt.Errorf("embedding endpoint: %s", decoded.Error)
	}
	if len(decoded.Embeddings) > 0 {
		return decoded.Embeddings, nil
	}
	if len(decoded.Embedding) > 0 {
		return [][]float64{decoded.Embedding}, nil
	}
	if len(decoded.Data) == 0 {
		return nil, fmt.Errorf("embedding endpoint returned no vectors")
	}
	out := make([][]float64, len(texts))
	for i, entry := range decoded.Data {
		idx := i
		if entry.Index != nil {
			idx = *entry.Index
		}
		if idx < 0 || idx >= len(out) {
			return nil, fmt.Errorf("embedding endpoint returned out-of-range index %d", idx)
		}
		out[idx] = entry.Embedding
	}
	for i := range out {
		if out[i] == nil {
			return nil, fmt.Errorf("embedding endpoint omitted index %d", i)
		}
	}
	return out, nil
}
