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
	"github.com/rs/zerolog/log"

	"github.com/DanielBlei/ai-searcher/internal/feeds"
	"github.com/DanielBlei/ai-searcher/internal/httpclient"
	"github.com/DanielBlei/ai-searcher/internal/rank"
	"github.com/DanielBlei/ai-searcher/internal/retry"
)

const (
	listTimeout = 30 * time.Second
	chatTimeout = 5 * time.Minute
)

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
	validateOnce sync.Once
	validateErr  error
}

// New connects to host using the given chat model. apiKey is optional (Bearer).
// The model must carry an explicit tag to avoid pulling the wrong image.
// think enables the model's reasoning mode, which is on by default for scoring.
func New(host, model, apiKey string, think bool) (*Client, error) {
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
	return &Client{api: api.NewClient(u, hc), model: model, think: think}, nil
}

// Validate confirms Ollama is reachable and the chat model is available.
// Connectivity is retried with backoff since Ollama may still be starting up;
// a model that's reachable but missing the tag fails immediately instead.
func (c *Client) Validate(ctx context.Context) error {
	c.validateOnce.Do(func() {
		var resp *api.ListResponse
		c.validateErr = retry.Do(ctx, validateAttempts, validateBackoff, func() error {
			r, err := c.list(ctx)
			if err != nil {
				return err
			}
			resp = r
			return nil
		}, func(attempt int, err error, delay time.Duration) {
			log.Debug().
				Int("attempt", attempt).
				Err(err).
				Str("retry_in", delay.String()).
				Msg("ollama: not reachable yet, retrying")
		})
		if c.validateErr != nil {
			return
		}
		want := normalize(c.model)
		for _, m := range resp.Models {
			if normalize(m.Model) == want {
				return
			}
		}
		c.validateErr = fmt.Errorf("model %q not found — run: ollama pull %s", c.model, c.model)
	})
	return c.validateErr
}

func (c *Client) list(ctx context.Context) (*api.ListResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()
	resp, err := c.api.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("connect to ollama: %w", err)
	}
	return resp, nil
}

// Score sends one JSON-mode chat request for the batch and parses the verdicts.
func (c *Client) Score(ctx context.Context, profile string, items []feeds.Item) ([]rank.ItemScore, error) {
	ctx, cancel := context.WithTimeout(ctx, chatTimeout)
	defer cancel()

	userPrompt := rank.BuildUserPrompt(profile, items)
	stream := false
	req := &api.ChatRequest{
		Model:  c.model,
		Stream: &stream,
		Format: json.RawMessage(`"json"`),
		Messages: []api.Message{
			{Role: "system", Content: rank.SystemPrompt},
			{Role: "user", Content: userPrompt},
		},
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

	log.Debug().
		Str("model", c.model).
		Int("items", len(items)).
		Bool("think", c.think).
		Msg("ollama: sending scoring request")
	log.Trace().Str("prompt", userPrompt).Msg("ollama: prompt sent")

	start := time.Now()
	var sb strings.Builder
	err := c.api.Chat(ctx, req, func(resp api.ChatResponse) error {
		sb.WriteString(resp.Message.Content)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("ollama chat: %w", err)
	}

	log.Debug().
		Str("elapsed", time.Since(start).Round(time.Millisecond).String()).
		Int("response_bytes", sb.Len()).
		Msg("ollama: scoring response received")
	log.Trace().Str("response", sb.String()).Msg("ollama: raw response")

	return rank.ParseScores(sb.String(), items)
}

func normalize(m string) string {
	if !strings.Contains(m, ":") {
		return m + ":latest"
	}
	return m
}
