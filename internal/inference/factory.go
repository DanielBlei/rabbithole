// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

// Package inference resolves the configured backend into a rank.Scorer.
package inference

import (
	"context"
	"fmt"

	"github.com/rs/zerolog"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/ollama"
	"github.com/DanielBlei/rabbithole/internal/rank"
	"github.com/DanielBlei/rabbithole/internal/vllm"
)

// Resolve constructs and validates the Scorer for the configured provider.
// think is passed separately from cfg because a single run can override the
// configured default (e.g. via --no-think) without editing the config.
// systemPrompt is the resolved value from cfg.LoadSystemPrompt(); heuristic ignores it.
//
// When cfg.Summary is set, its resolved backend is also validated (but its
// Scorer discarded) so a broken placeholder summary model is caught here
// too, even though nothing routes to it yet.
//
// Once something actually routes to the summary model, Resolve will likely
// need to return both Scorers (falling back to returning the same one
// twice when Summary is unset) instead of discarding the second one.
func Resolve(ctx context.Context, cfg config.InferenceConfig, think bool, systemPrompt string) (rank.Scorer, error) {
	s, err := build(ctx, cfg, think, systemPrompt)
	if err != nil {
		return nil, err
	}
	if cfg.Summary != (config.SummaryConfig{}) {
		if _, err := build(ctx, cfg.ResolveSummary(), think, systemPrompt); err != nil {
			return nil, fmt.Errorf("summary model: %w", err)
		}
	}
	return s, nil
}

// build constructs and validates the Scorer described by cfg.
func build(ctx context.Context, cfg config.InferenceConfig, think bool, systemPrompt string) (rank.Scorer, error) {
	var (
		s   rank.Scorer
		err error
	)
	tuning := cfg.ModelTuning
	if cfg.StructuredOutput != nil && !*cfg.StructuredOutput && cfg.Provider != "heuristic" {
		tuning.Unconstrained = true
		logger := zerolog.Ctx(ctx)
		logger.Debug().Str("provider", cfg.Provider).Msg("structured output off: the system prompt sets the JSON shape")
		if systemPrompt == "" {
			logger.Warn().Str("provider", cfg.Provider).
				Msg("structured_output is false and no system prompt is sent: nothing asks the model for the JSON shape")
		}
	}
	switch cfg.Provider {
	case "ollama":
		s, err = ollama.New(cfg.Host, cfg.Model, cfg.APIKey, think, tuning, systemPrompt)
	case "openai":
		s, err = vllm.New(vllm.OpenAI, cfg.Host, cfg.Model, cfg.APIKey,
			think, cfg.Effort, tuning, systemPrompt)
	case "vllm":
		s, err = vllm.New(vllm.VLLM, cfg.Host, cfg.Model, cfg.APIKey,
			think, cfg.Effort, tuning, systemPrompt)
	case "heuristic":
		s = rank.NewHeuristic()
	default:
		return nil, fmt.Errorf("unknown provider %q, must be ollama, openai, vllm or heuristic", cfg.Provider)
	}
	if err != nil {
		return nil, fmt.Errorf("backend init: %w", err)
	}
	if err := s.Validate(ctx); err != nil {
		return nil, fmt.Errorf("backend validation: %w", err)
	}
	return s, nil
}
