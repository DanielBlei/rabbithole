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
)

const (
	listTimeout = 30 * time.Second
	chatTimeout = 5 * time.Minute
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
// think enables the model's reasoning mode; for scoring it should normally be
// off so reasoning models (e.g. qwen3) return JSON promptly.
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
func (c *Client) Validate(ctx context.Context) error {
	c.validateOnce.Do(func() {
		ctx, cancel := context.WithTimeout(ctx, listTimeout)
		defer cancel()
		resp, err := c.api.List(ctx)
		if err != nil {
			c.validateErr = fmt.Errorf("connect to ollama: %w", err)
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

// Score sends one JSON-mode chat request for the batch and parses the verdicts.
func (c *Client) Score(ctx context.Context, profile string, items []feeds.Item) ([]rank.ItemScore, error) {
	ctx, cancel := context.WithTimeout(ctx, chatTimeout)
	defer cancel()

	stream := false
	req := &api.ChatRequest{
		Model:  c.model,
		Stream: &stream,
		Format: json.RawMessage(`"json"`),
		Messages: []api.Message{
			{Role: "system", Content: rank.SystemPrompt},
			{Role: "user", Content: rank.BuildUserPrompt(profile, items)},
		},
	}

	// Set the model's reasoning mode explicitly. Scoring is structured
	// extraction, so think is off by default — reasoning models (e.g. qwen3)
	// otherwise spend the whole timeout on chain-of-thought before the JSON.
	// Ignored by models without a think mode.
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

	return rank.ParseScores(sb.String(), items)
}

func normalize(m string) string {
	if !strings.Contains(m, ":") {
		return m + ":latest"
	}
	return m
}
