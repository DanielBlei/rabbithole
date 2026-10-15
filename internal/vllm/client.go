// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

// Package vllm implements rank.Scorer against an OpenAI-compatible
// chat-completions endpoint: OpenAI or a hosted gateway, or vLLM itself.
package vllm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
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

// Flavor picks the request dialect; its String is the provider name in logs and errors.
type Flavor int

// The flavors: OpenAI or a gateway that speaks its API, and vLLM itself.
const (
	OpenAI Flavor = iota
	VLLM
)

func (f Flavor) String() string {
	if f == OpenAI {
		return "openai"
	}
	return "vllm"
}

type modelsResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// probeRequest omits response_format: some servers reject an empty one.
type probeRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
	tokenCap
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

// lowestEfforts is the think-off ladder: none (gpt-5.1+), minimal (gpt-5),
// low (older reasoning models), then the field left out.
var lowestEfforts = []string{"none", "minimal", "low", ""}

// tokenCap carries the output limit under the field name the flavor expects:
// OpenAI reasoning models reject max_tokens and take max_completion_tokens.
type tokenCap struct {
	MaxTokens           int `json:"max_tokens,omitempty"`
	MaxCompletionTokens int `json:"max_completion_tokens,omitempty"`
}

func (c *Client) tokenCap(n int) tokenCap {
	if c.flavor == OpenAI {
		return tokenCap{MaxCompletionTokens: n}
	}
	return tokenCap{MaxTokens: n}
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
func (c *Client) probeModelWithRetry(ctx context.Context, effort string) error {
	var last error
	_ = retry.Do(ctx, validateAttempts, validateBackoff, func() error {
		last = c.probeModel(ctx, effort)
		if p, ok := last.(*probeError); ok && !p.transient() {
			return nil // stop retrying; last still holds the error
		}
		return last
	}, func(attempt int, err error, delay time.Duration) {
		zerolog.Ctx(ctx).Debug().Str("backend", c.flavor.String()).Int("attempt", attempt).Err(err).
			Str("retry_in", delay.String()).Msg(c.flavor.String() + ": chat probe failed, retrying")
	})
	return last
}

// probeModel posts a one-token chat completion; only a 200 accepts the model
// (and the effort, when one is given).
func (c *Client) probeModel(ctx context.Context, effort string) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	body, err := json.Marshal(probeRequest{
		Model:           c.model,
		Messages:        []chatMessage{{Role: "user", Content: "ok"}},
		Stream:          false,
		tokenCap:        c.tokenCap(1),
		ReasoningEffort: effort,
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
	flavor       Flavor
	host         string
	model        string
	think        bool
	effort       string // configured reasoning_effort while thinking (openai only)
	tuning       rank.ModelTuning
	systemPrompt string // empty sends no system message at all
	hc           *http.Client
	validator    *retry.Validator

	effortMu       sync.Mutex
	effortResolved bool
	sendEffort     string // reasoning_effort actually sent; "" leaves the field out
}

// New connects to host using the given model, speaking flavor's dialect.
// apiKey is optional (Bearer). think enables the model's reasoning mode (on by
// default for scoring), and effort is the openai reasoning_effort sent while it
// is on. tuning carries the decoding limits; its zero value uses rank's
// defaults. systemPrompt is sent as the request's system message; empty omits
// it entirely.
func New(
	flavor Flavor, host, model, apiKey string, think bool, effort string,
	tuning rank.ModelTuning, systemPrompt string,
) (*Client, error) {
	if _, err := url.Parse(host); err != nil {
		return nil, fmt.Errorf("invalid host %q: %w", host, err)
	}
	c := &Client{
		flavor:       flavor,
		host:         strings.TrimRight(host, "/"),
		model:        model,
		think:        think,
		effort:       effort,
		tuning:       tuning.Normalize(),
		systemPrompt: systemPrompt,
		hc: &http.Client{
			Transport: &httpclient.BearerTransport{Token: apiKey, Base: http.DefaultTransport},
		},
		validator: retry.NewValidator(flavor.String(), validateAttempts, validateBackoff),
	}
	// Until Validate probes the model, send the value every reasoning model accepts.
	if flavor == OpenAI {
		c.sendEffort = effort
		if !think {
			c.sendEffort = "low"
		}
	}
	return c, nil
}

// Validate confirms the server is reachable and the model is loaded.
// Connectivity is retried with backoff since the server may still be
// starting up. A model missing from /v1/models is accepted if a chat probe
// succeeds, since gateways can route models their catalog doesn't list.
// On openai it then probes which reasoning_effort the model accepts.
func (c *Client) Validate(ctx context.Context) error {
	err := c.validator.Validate(ctx, c.model,
		fmt.Sprintf("not listed by /v1/models at %s", c.host), c.listModelNames,
		func(ctx context.Context) error {
			if err := c.probeModelWithRetry(ctx, ""); err != nil {
				return fmt.Errorf("chat probe: %w", err)
			}
			zerolog.Ctx(ctx).Warn().
				Str("model", c.model).
				Str("host", c.host).
				Msg(c.flavor.String() + ": model not in /v1/models catalog; accepted via chat probe")
			return nil
		})
	if err != nil || c.flavor != OpenAI {
		return err
	}
	return c.resolveEffort(ctx)
}

// resolveEffort picks the reasoning_effort Score sends: the configured one
// while thinking, otherwise the lowest the model accepts, and none at all when
// the model takes no reasoning_effort. Validate finishes before any Score runs,
// so the write to sendEffort needs no lock beyond the one guarding the probe.
func (c *Client) resolveEffort(ctx context.Context) error {
	c.effortMu.Lock()
	defer c.effortMu.Unlock()
	if c.effortResolved {
		return nil
	}
	logger := zerolog.Ctx(ctx)
	candidates := lowestEfforts
	if c.think {
		candidates = []string{c.effort, ""}
		if c.effort == "" {
			candidates = candidates[1:]
		}
	}
	var rejected error
	for _, effort := range candidates {
		err := c.probeModelWithRetry(ctx, effort)
		var p *probeError
		switch {
		case err == nil:
			c.sendEffort = effort
			c.effortResolved = true
			if effort == "" {
				logger.Warn().Str("model", c.model).Err(rejected).
					Msg("openai: model rejects reasoning_effort; sending none")
			} else {
				logger.Debug().Str("model", c.model).Str("reasoning_effort", effort).
					Msg("openai: reasoning_effort accepted")
			}
			return nil
		case errors.As(err, &p) && (p.status == http.StatusBadRequest || p.status == http.StatusUnprocessableEntity):
			rejected = err // this value is not accepted; try the next one
		case errors.As(err, &p) && !p.transient():
			return fmt.Errorf("openai chat probe: %w", err) // auth or model, not an effort question
		default:
			logger.Debug().Err(err).Str("model", c.model).Str("reasoning_effort", c.sendEffort).
				Msg("openai: reasoning_effort probe inconclusive, keeping the default")
			return nil
		}
	}
	return fmt.Errorf("openai chat probe: every request was rejected, even without reasoning_effort: %w", rejected)
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
		return nil, fmt.Errorf("connect to %s: %w", c.flavor, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s /v1/models returned %d", c.flavor, resp.StatusCode)
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
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
	tokenCap
	ResponseFormat     responseFormat `json:"response_format"`
	ReasoningEffort    string         `json:"reasoning_effort,omitempty"`
	ChatTemplateKwargs map[string]any `json:"chat_template_kwargs,omitempty"`
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
		Model:    c.model,
		Stream:   false,
		tokenCap: c.tokenCap(budget),
		Messages: messages,
		ResponseFormat: responseFormat{
			Type: "json_schema",
			JSONSchema: &jsonSchema{
				Name:   "scores",
				Strict: true,
				Schema: json.RawMessage(c.tuning.Schema()),
			},
		},
	}
	switch {
	case c.flavor == OpenAI:
		reqBody.ReasoningEffort = c.sendEffort
	case !c.think:
		// vLLM's off switch for templates with a thinking mode (Qwen3 and alike).
		reqBody.ChatTemplateKwargs = map[string]any{"enable_thinking": false}
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal chat request: %w", err)
	}

	logger.Debug().
		Str("model", c.model).
		Int("items", len(items)).
		Bool("think", c.think).
		Str("reasoning_effort", reqBody.ReasoningEffort).
		Int("max_tokens", budget).
		Msg(c.flavor.String() + ": sending scoring request")
	logger.Trace().Str("prompt", userPrompt).Msg(c.flavor.String() + ": prompt sent")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.host+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build chat request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s chat: %w", c.flavor, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("%s chat returned %d: %s", c.flavor, resp.StatusCode, bytes.TrimSpace(msg))
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
		return nil, fmt.Errorf("%s returned no choices", c.flavor)
	}

	content := completion.Choices[0].Message.Content
	finishReason := completion.Choices[0].FinishReason
	logger.Debug().
		Str("elapsed", time.Since(start).Round(time.Millisecond).String()).
		Int("response_bytes", len(content)).
		Str("finish_reason", finishReason).
		Int("completion_tokens", completion.Usage.CompletionTokens).
		Msg(c.flavor.String() + ": scoring response received")
	logger.Trace().Str("response", content).Msg(c.flavor.String() + ": raw response")

	scores, err := rank.ParseScores(content, items)
	if err != nil {
		// finish_reason "length" means the server cut generation off at the token
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
