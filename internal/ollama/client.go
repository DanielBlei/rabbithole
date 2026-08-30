// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

// Package ollama implements rank.Scorer against a local Ollama server.
package ollama

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ollama/ollama/api"
	"github.com/rs/zerolog"

	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/httpclient"
	"github.com/DanielBlei/rabbithole/internal/rank"
	"github.com/DanielBlei/rabbithole/internal/retry"
)

const (
	listTimeout  = 30 * time.Second
	chatTimeout  = 5 * time.Minute
	probeTimeout = 1 * time.Minute // think-support probe; allows a cold model load
)

// thinkUnsupported is the substring Ollama returns when a model without a
// reasoning mode is sent a think request (e.g. 'gemma3:1b' does not support
// thinking). Matched to fall back to think-off rather than fail every score.
const thinkUnsupported = "does not support thinking"

// validateAttempts/validateBackoff bound how long Validate waits for Ollama
// to come up: 3 tries with exponential backoff starting at 30s (30s, 60s).
var (
	validateAttempts = 3
	validateBackoff  = 30 * time.Second
)

// Client scores items using an Ollama chat model in JSON mode.
type Client struct {
	api          *api.Client
	model        string
	think        bool
	tuning       rank.ModelTuning
	systemPrompt string // empty sends no system message at all
	validator    *retry.Validator
	thinkOnce    sync.Once // gates the one-time think-support probe in Validate
}

// New connects to host using the given chat model. apiKey is optional (Bearer).
// The model must carry an explicit tag to avoid pulling the wrong image.
// think enables the model's reasoning mode, which is on by default for scoring.
// tuning carries the decoding limits; its zero value uses rank's defaults.
// systemPrompt is sent as the chat request's system message; empty omits it entirely.
func New(host, model, apiKey string, think bool, tuning rank.ModelTuning, systemPrompt string) (*Client, error) {
	u, err := url.Parse(host)
	if err != nil {
		return nil, fmt.Errorf("invalid host %q: %w", host, err)
	}
	if !strings.Contains(model, ":") {
		return nil, fmt.Errorf(
			"model %q has no tag, use an explicit tag (e.g. %s:latest)", model, model)
	}
	hc := &http.Client{
		Transport: &httpclient.BearerTransport{Token: apiKey, Base: http.DefaultTransport},
	}
	return &Client{
		api:          api.NewClient(u, hc),
		model:        model,
		think:        think,
		tuning:       tuning.Normalize(),
		systemPrompt: systemPrompt,
		validator:    retry.NewValidator("ollama", validateAttempts, validateBackoff),
	}, nil
}

// Validate confirms Ollama is reachable and the chat model is available.
// Connectivity is retried with backoff since Ollama may still be starting up;
// a model that's reachable but missing the tag fails immediately instead.
//
// When think mode is on, it then probes once that the model actually supports
// reasoning: Ollama 400s a think request for models without it, so rather than
// let every scoring request fail we fall back to think-off here.
func (c *Client) Validate(ctx context.Context) error {
	if err := c.validator.Validate(ctx, normalize(c.model),
		fmt.Sprintf("run: ollama pull %s", c.model), c.listModelNames); err != nil {
		return err
	}
	c.thinkOnce.Do(func() {
		if c.think {
			c.checkThinkSupport(ctx)
		}
	})
	return nil
}

// checkThinkSupport sends a minimal think-enabled request to confirm the model
// has a reasoning mode. On the specific "does not support thinking" rejection it
// disables think (a warning, not a hard failure). Any other error is treated as
// inconclusive — think stays on and the real scoring request will surface and
// retry the problem. The write to c.think is safe: Validate runs to completion
// before any concurrent Score call (inference.Resolve validates, then scoring
// fans out).
func (c *Client) checkThinkSupport(ctx context.Context) {
	logger := zerolog.Ctx(ctx)
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	stream := false
	think := &api.ThinkValue{}
	if err := think.UnmarshalJSON([]byte("true")); err != nil {
		return
	}
	req := &api.ChatRequest{
		Model:    c.model,
		Stream:   &stream,
		Think:    think,
		Messages: []api.Message{{Role: "user", Content: "ping"}},
		Options:  map[string]any{"num_predict": 1}, // keep the probe cheap when think is supported
	}

	err := c.api.Chat(ctx, req, func(api.ChatResponse) error { return nil })
	switch {
	case err == nil:
		// Model accepts think — leave it on.
	case strings.Contains(err.Error(), thinkUnsupported):
		c.think = false
		logger.Warn().Str("model", c.model).
			Msg("ollama: model does not support thinking, disabling think mode for scoring")
	default:
		logger.Debug().Err(err).Str("model", c.model).
			Msg("ollama: think-support probe inconclusive, leaving think enabled")
	}
}

func (c *Client) listModelNames(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()
	resp, err := c.api.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("connect to ollama: %w", err)
	}
	names := make([]string, len(resp.Models))
	for i, m := range resp.Models {
		names[i] = normalize(m.Model)
	}
	return names, nil
}

// Score sends one JSON-mode chat request for the batch and parses the verdicts.
func (c *Client) Score(ctx context.Context, profile string, items []feeds.Item) ([]rank.ItemScore, error) {
	logger := zerolog.Ctx(ctx)
	ctx, cancel := context.WithTimeout(ctx, chatTimeout)
	defer cancel()

	userPrompt := rank.BuildUserPrompt(profile, items)
	stream := false
	budget := c.tuning.Budget(len(items), c.think)
	opts := map[string]any{"num_predict": budget}
	// Left unset, Ollama silently drops the front of an over-long prompt.
	if c.tuning.NumCtx > 0 {
		opts["num_ctx"] = c.tuning.NumCtx
	}
	var messages []api.Message
	// Omitted rather than sent empty: a model's own Modelfile system prompt only kicks in
	// when no system message is present at all.
	if c.systemPrompt != "" {
		messages = append(messages, api.Message{Role: "system", Content: c.systemPrompt})
	}
	messages = append(messages, api.Message{Role: "user", Content: userPrompt})

	req := &api.ChatRequest{
		Model:  c.model,
		Stream: &stream,
		// A schema rather than bare "json": Ollama compiles it to a grammar and samples only conforming tokens
		Format:   json.RawMessage(c.tuning.Schema()),
		Options:  opts,
		Messages: messages,
	}

	// Set the model's reasoning mode explicitly. Ignored by models without a
	// think mode.
	think := &api.ThinkValue{}
	thinkJSON := "false"
	if c.think {
		thinkJSON = "true"
	}
	if err := think.UnmarshalJSON([]byte(thinkJSON)); err == nil {
		req.Think = think
	}

	logger.Debug().
		Str("model", c.model).
		Int("items", len(items)).
		Bool("think", c.think).
		Int("num_predict", budget).
		Msg("ollama: sending scoring request")
	logger.Trace().Str("prompt", userPrompt).Msg("ollama: prompt sent")

	start := time.Now()
	var sb strings.Builder
	var doneReason string
	var evalCount int
	var sawDone bool
	err := c.api.Chat(ctx, req, func(resp api.ChatResponse) error {
		sb.WriteString(resp.Message.Content)
		if resp.Done {
			sawDone = true
			doneReason = resp.DoneReason
			evalCount = resp.EvalCount
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("ollama chat: %w", err)
	}
	if !sawDone {
		// The stream closed without a done chunk at all (not a "length"/"stop"
		// done_reason, which would mean Ollama did report a reason) - distinct
		// from the zero value so logs don't read as a reported-but-empty reason.
		// The response is incomplete but not worthless: ParseScores repairs the
		// truncated tail, so the entries that did arrive still count. Warned
		// either way, since a server cutting off mid-answer is worth knowing
		// about even when the salvage works.
		doneReason = "no done signal (stream closed early)"
		logger.Warn().Str("model", c.model).Int("response_bytes", sb.Len()).
			Msg("ollama: response ended without a done signal, consuming what arrived")
	}

	// Logged as completion_tokens, not Ollama's own eval_count, to match vllm.Client's log key.
	logger.Debug().
		Str("elapsed", time.Since(start).Round(time.Millisecond).String()).
		Int("response_bytes", sb.Len()).
		Str("done_reason", doneReason).
		Int("completion_tokens", evalCount).
		Msg("ollama: scoring response received")
	logger.Trace().Str("response", sb.String()).Msg("ollama: raw response")

	scores, err := rank.ParseScores(sb.String(), items)
	if err != nil {
		// done_reason "length" means Ollama cut generation off at num_predict/
		// num_ctx mid-answer; "stop" means the model emitted its own stop
		// token and still produced unparseable output. Distinguishing the two
		// tells us whether to raise the token budget or suspect the model.
		return nil, fmt.Errorf("%w (done_reason=%q, completion_tokens=%d)", err, doneReason, evalCount)
	}
	return scores, nil
}

func normalize(m string) string {
	if !strings.Contains(m, ":") {
		return m + ":latest"
	}
	return m
}
