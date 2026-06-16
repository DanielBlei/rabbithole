// Package vllm implements rank.Scorer against an OpenAI-compatible endpoint
// such as vLLM.
package vllm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/DanielBlei/ai-searcher/internal/feeds"
	"github.com/DanielBlei/ai-searcher/internal/httpclient"
	"github.com/DanielBlei/ai-searcher/internal/rank"
)

const (
	listTimeout = 30 * time.Second
	chatTimeout = 3 * time.Minute
)

// Client scores items via /v1/chat/completions in JSON mode.
type Client struct {
	host        string
	model       string
	think       bool
	hc          *http.Client
	once        sync.Once
	validateErr error
}

// New connects to host using the given model. apiKey is optional (Bearer).
// think enables the model's reasoning mode (off by default for scoring).
func New(host, model, apiKey string, think bool) (*Client, error) {
	if _, err := url.Parse(host); err != nil {
		return nil, fmt.Errorf("invalid host %q: %w", host, err)
	}
	return &Client{
		host:  strings.TrimRight(host, "/"),
		model: model,
		think: think,
		hc: &http.Client{
			Transport: &httpclient.BearerTransport{Token: apiKey, Base: http.DefaultTransport},
		},
	}, nil
}

// Validate confirms the server is reachable and the model is loaded.
func (c *Client) Validate(ctx context.Context) error {
	c.once.Do(func() {
		ctx, cancel := context.WithTimeout(ctx, listTimeout)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.host+"/v1/models", nil)
		if err != nil {
			c.validateErr = fmt.Errorf("build models request: %w", err)
			return
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			c.validateErr = fmt.Errorf("connect to vLLM: %w", err)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			c.validateErr = fmt.Errorf("vLLM /v1/models returned %d", resp.StatusCode)
			return
		}
		var models struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&models); err != nil {
			c.validateErr = fmt.Errorf("decode models response: %w", err)
			return
		}
		for _, m := range models.Data {
			if m.ID == c.model {
				return
			}
		}
		c.validateErr = fmt.Errorf("model %q not loaded in vLLM at %s", c.model, c.host)
	})
	return c.validateErr
}

type chatRequest struct {
	Model            string         `json:"model"`
	Messages         []chatMessage  `json:"messages"`
	Stream           bool           `json:"stream"`
	ResponseFormat   responseFormat `json:"response_format"`
	IncludeReasoning *bool          `json:"include_reasoning,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type responseFormat struct {
	Type string `json:"type"`
}

// Score sends one JSON-mode completion for the batch and parses the verdicts.
func (c *Client) Score(ctx context.Context, profile string, items []feeds.Item) ([]rank.ItemScore, error) {
	ctx, cancel := context.WithTimeout(ctx, chatTimeout)
	defer cancel()

	reqBody := chatRequest{
		Model:  c.model,
		Stream: false,
		Messages: []chatMessage{
			{Role: "system", Content: rank.SystemPrompt},
			{Role: "user", Content: rank.BuildUserPrompt(profile, items)},
		},
		ResponseFormat: responseFormat{Type: "json_object"},
	}
	// Disable reasoning unless requested, so reasoning models return JSON
	// promptly. Servers that don't recognize the field ignore it.
	if !c.think {
		off := false
		reqBody.IncludeReasoning = &off
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal chat request: %w", err)
	}

	log.Debug().
		Str("model", c.model).
		Int("items", len(items)).
		Bool("think", c.think).
		Msg("vllm: sending scoring request")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.host+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build chat request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("vLLM chat: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("vLLM chat returned %d: %s", resp.StatusCode, bytes.TrimSpace(msg))
	}

	var completion struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&completion); err != nil {
		return nil, fmt.Errorf("decode chat response: %w", err)
	}
	if len(completion.Choices) == 0 {
		return nil, fmt.Errorf("vLLM returned no choices")
	}

	content := completion.Choices[0].Message.Content
	log.Debug().
		Str("elapsed", time.Since(start).Round(time.Millisecond).String()).
		Int("response_bytes", len(content)).
		Msg("vllm: scoring response received")

	return rank.ParseScores(content, items)
}
