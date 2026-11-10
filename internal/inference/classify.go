// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package inference

import (
	"context"
	"fmt"

	"github.com/DanielBlei/rabbithole/internal/classify"
	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/ollama"
	"github.com/DanielBlei/rabbithole/internal/rank"
	"github.com/DanielBlei/rabbithole/internal/vllm"
)

// structuredBackend is the part of the provider clients classification needs.
type structuredBackend interface {
	Complete(ctx context.Context, userPrompt, schemaName, schema string, budget int) (string, error)
	Validate(ctx context.Context) error
}

// promptClassifier turns the configured backend into a classify.Classifier.
type promptClassifier struct {
	backend    structuredBackend
	vocabulary []string
	tuning     rank.ModelTuning
	think      bool
}

// Classify sends one structured-output request and validates the reply.
func (c *promptClassifier) Classify(ctx context.Context, items []feeds.Item) ([]classify.ItemVerdict, error) {
	budget := c.tuning.Budget(len(items), c.think)
	raw, err := c.backend.Complete(
		ctx, classify.BuildUserPrompt(items, c.vocabulary), "classifications", classify.Schema(), budget)
	if err != nil {
		return nil, err
	}
	return classify.ParseClassifications(raw, items)
}

// Validate confirms the backend is reachable.
func (c *promptClassifier) Validate(ctx context.Context) error { return c.backend.Validate(ctx) }

// ResolveClassifier constructs and validates the classifier for the configured
// provider. An api_key reference resolves here, so errors name the field and
// never the value. The heuristic provider is a model-free no-op and never dials
// out; the worker skips it rather than recording empty tags.
func ResolveClassifier(
	ctx context.Context,
	cfg config.ClassificationConfig,
	vocabulary []string,
) (classify.Classifier, error) {
	key, err := cfg.ResolvedAPIKey()
	if err != nil {
		return nil, err
	}
	main := cfg
	main.APIKey = key
	tuning := main.ModelTuning
	if main.StructuredOutput != nil && !*main.StructuredOutput && main.Provider != config.ProviderHeuristic {
		tuning.Unconstrained = true
	}

	var backend structuredBackend
	switch main.Provider {
	case config.ProviderOllama:
		backend, err = ollama.New(
			main.Host,
			main.Model,
			main.APIKey,
			main.ThinkEnabled(),
			tuning,
			classify.SystemPrompt,
		)
	case config.ProviderOpenAI:
		backend, err = vllm.New(vllm.OpenAI, main.Host, main.Model, main.APIKey,
			main.ThinkEnabled(), main.Effort, tuning, classify.SystemPrompt)
	case config.ProviderVLLM:
		backend, err = vllm.New(vllm.VLLM, main.Host, main.Model, main.APIKey,
			main.ThinkEnabled(), main.Effort, tuning, classify.SystemPrompt)
	case config.ProviderHeuristic:
		return classify.NewHeuristic(), nil
	default:
		return nil, fmt.Errorf("unknown provider %q, must be ollama, openai, vllm or heuristic", main.Provider)
	}
	if err != nil {
		return nil, fmt.Errorf("classifier init: %w", err)
	}
	if err := backend.Validate(ctx); err != nil {
		return nil, fmt.Errorf("classifier validation: %w", err)
	}
	return &promptClassifier{
		backend:    backend,
		vocabulary: vocabulary,
		tuning:     main.ModelTuning,
		think:      main.ThinkEnabled(),
	}, nil
}
