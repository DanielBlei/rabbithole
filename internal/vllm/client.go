// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

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
	"time"

	"github.com/rs/zerolog"

	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/httpclient"
	"github.com/DanielBlei/rabbithole/internal/rank"
	"github.com/DanielBlei/rabbithole/internal/retry"
)

const (
	listTimeout = 30 * time.Second
	chatTimeout = 3 * time.Minute
	// probeTimeout is short: the probe asks for one token, not a scoring budget.
	probeTimeout = 30 * time.Second
)

// validateAttempts/validateBackoff bound how long Validate waits for the
// server to come up: 3 tries with exponential backoff starting at 30s (30s, 60s).
var (
	validateAttempts = 3
	validateBackoff  = 30 * time.Second
)

type modelsResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// probeRequest omits response_format: some servers reject an empty one.
type probeRequest struct {
	Model     string        `json:"model"`
	Messages  []chatMessage `json:"messages"`
	Stream    bool          `json:"stream"`
	MaxTokens int           `json:"max_tokens"`
}

// probeError carries a non-200 chat-probe response.
type probeError struct {
	status int
	body   []byte
}

func (e *probeError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.status, e.body)
}

func (e *probeError) transient() bool {
	switch e.status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return true
	}
	return e.status < 400 || e.status >= 500
}

// probeModelWithRetry retries transient failures; any other 4xx is a verdict.
func (c *Client) probeModelWithRetry(ctx context.Context) error {
	var last error
	_ = retry.Do(ctx, validateAttempts, validateBackoff, func() error {
		last = c.probeModel(ctx)
		if p, ok := last.(*probeError); ok && !p.transient() {
			return nil // stop retrying; last still holds the error
		}
		return last
	}, func(attempt int, err error, delay time.Duration) {
		zerolog.Ctx(ctx).Debug().Str("backend", "vllm").Int("attempt", attempt).Err(err).
			Str("retry_in", delay.String()).Msg("vllm: chat probe failed, retrying")
	})
	return last
}

// probeModel posts a one-token chat completion; only a 200 accepts the model.
func (c *Client) probeModel(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	body, err := json.Marshal(probeRequest{
		Model:     c.model,
		Messages:  []chatMessage{{Role: "user", Content: "ok"}},
		Stream:    false,
		MaxTokens: 1,
	})
	if err != nil {
		return fmt.Errorf("marshal probe request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.host+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build probe request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("connect for chat probe: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return &probeError{status: resp.StatusCode, body: bytes.TrimSpace(msg)}
	}
	return nil
}

// Client scores items via /v1/chat/completions in JSON mode.
type Client struct {
	host         string
	model        string
	think        bool
	tuning       rank.ModelTuning
	systemPrompt string // empty sends no system message at all
	hc           *http.Client
	validator    *retry.Validator
}

// New connects to host using the given model. apiKey is optional (Bearer).
// think enables the model's reasoning mode (on by default for scoring).
// tuning carries the decoding limits; its zero value uses rank's defaults.
// systemPrompt is sent as the request's system message; empty omits it entirely.
func New(host, model, apiKey string, think bool, tuning rank.ModelTuning, systemPrompt string) (*Client, error) {
	if _, err := url.Parse(host); err != nil {
		return nil, fmt.Errorf("invalid host %q: %w", host, err)
	}
	return &Client{
		host:         strings.TrimRight(host, "/"),
		model:        model,
		think:        think,
		tuning:       tuning.Normalize(),
		systemPrompt: systemPrompt,
		hc: &http.Client{
			Transport: &httpclient.BearerTransport{Token: apiKey, Base: http.DefaultTransport},
		},
		validator: retry.NewValidator("vllm", validateAttempts, validateBackoff),
	}, nil
}

// Validate confirms the server is reachable and the model is loaded.
// Connectivity is retried with backoff since the server may still be
// starting up. A model missing from /v1/models is accepted if a chat probe
// succeeds, since gateways can route models their catalog doesn't list.
func (c *Client) Validate(ctx context.Context) error {
	return c.validator.Validate(ctx, c.model,
		fmt.Sprintf("not listed by /v1/models at %s", c.host), c.listModelNames,
		func(ctx context.Context) error {
			if err := c.probeModelWithRetry(ctx); err != nil {
				return fmt.Errorf("chat probe: %w", err)
			}
			zerolog.Ctx(ctx).Warn().
				Str("model", c.model).
				Str("host", c.host).
				Msg("vllm: model not in /v1/models catalog; accepted via chat probe")
			return nil
		})
}

func (c *Client) listModelNames(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.host+"/v1/models", nil)
	if err != nil {
		return nil, fmt.Errorf("build models request: %w", err)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connect to vLLM: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("vLLM /v1/models returned %d", resp.StatusCode)
	}
	var models modelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&models); err != nil {
		return nil, fmt.Errorf("decode models response: %w", err)
	}
	names := make([]string, len(models.Data))
	for i, m := range models.Data {
		names[i] = m.ID
	}
	return names, nil
}

type chatRequest struct {
	Model           string         `json:"model"`
	Messages        []chatMessage  `json:"messages"`
	Stream          bool           `json:"stream"`
	MaxTokens       int            `json:"max_tokens"`
	ResponseFormat  responseFormat `json:"response_format"`
	ReasoningEffort string         `json:"reasoning_effort,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// responseFormat carries an OpenAI-compatible json_schema request. Guided
// decoding against the schema is what keeps the model from quoting a score or
// inventing a field; plain json_object only guarantees the syntax parses.
type responseFormat struct {
	Type       string      `json:"type"`
	JSONSchema *jsonSchema `json:"json_schema,omitempty"`
}

type jsonSchema struct {
	Name   string          `json:"name"`
	Strict bool            `json:"strict"`
	Schema json.RawMessage `json:"schema"`
}

// Score sends one JSON-mode completion for the batch and parses the verdicts.
func (c *Client) Score(ctx context.Context, profile string, items []feeds.Item) ([]rank.ItemScore, error) {
	logger := zerolog.Ctx(ctx)
	ctx, cancel := context.WithTimeout(ctx, chatTimeout)
	defer cancel()

	userPrompt := rank.BuildUserPrompt(profile, items)
	budget := c.tuning.Budget(len(items), c.think)

	var messages []chatMessage
	// Omitted rather than sent empty: a served model's own default system prompt only kicks
	// in when no system message is present at all.
	if c.systemPrompt != "" {
		messages = append(messages, chatMessage{Role: "system", Content: c.systemPrompt})
	}
	messages = append(messages, chatMessage{Role: "user", Content: userPrompt})

	reqBody := chatRequest{
		Model:     c.model,
		Stream:    false,
		MaxTokens: budget,
		Messages:  messages,
		ResponseFormat: responseFormat{
			Type: "json_schema",
			JSONSchema: &jsonSchema{
				Name:   "scores",
				Strict: true,
				Schema: json.RawMessage(c.tuning.Schema()),
			},
		},
	}
	// "low" is the lowest reasoning_effort in the OpenAI enum.
	if !c.think {
		reqBody.ReasoningEffort = "low"
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal chat request: %w", err)
	}

	logger.Debug().
		Str("model", c.model).
		Int("items", len(items)).
		Bool("think", c.think).
		Int("max_tokens", budget).
		Msg("vllm: sending scoring request")
	logger.Trace().Str("prompt", userPrompt).Msg("vllm: prompt sent")

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
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&completion); err != nil {
		return nil, fmt.Errorf("decode chat response: %w", err)
	}
	if len(completion.Choices) == 0 {
		return nil, fmt.Errorf("vLLM returned no choices")
	}

	content := completion.Choices[0].Message.Content
	finishReason := completion.Choices[0].FinishReason
	logger.Debug().
		Str("elapsed", time.Since(start).Round(time.Millisecond).String()).
		Int("response_bytes", len(content)).
		Str("finish_reason", finishReason).
		Int("completion_tokens", completion.Usage.CompletionTokens).
		Msg("vllm: scoring response received")
	logger.Trace().Str("response", content).Msg("vllm: raw response")

	scores, err := rank.ParseScores(content, items)
	if err != nil {
		// finish_reason "length" means vLLM cut generation off at the token
		// budget mid-answer; "stop" means the model emitted its own stop
		// token and still produced unparseable output. Distinguishing the two
		// tells us whether to raise the token budget or suspect the model.
		return nil, fmt.Errorf(
			"%w (finish_reason=%q, completion_tokens=%d)",
			err,
			finishReason,
			completion.Usage.CompletionTokens,
		)
	}
	return scores, nil
}
