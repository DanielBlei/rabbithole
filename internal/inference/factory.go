// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

// Package inference resolves the configured backend into a rank.Scorer.
package inference

import (
	"context"
	"fmt"

	"github.com/rs/zerolog"

	"github.com/DanielBlei/rabbithole/internal/briefing"
	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/ollama"
	"github.com/DanielBlei/rabbithole/internal/rank"
	"github.com/DanielBlei/rabbithole/internal/vllm"
)

// Resolve constructs and validates the Scorer for the configured provider. Api_key
// references resolve from the environment here, so errors name the field, never the value.
// think is passed separately from cfg because a single run can override the
// configured default (e.g. via --no-think) without editing the config.
// systemPrompt is the resolved value from cfg.EffectiveSystemPrompt(); heuristic ignores it.
func Resolve(ctx context.Context, cfg config.InferenceConfig, think bool, systemPrompt string) (rank.Scorer, error) {
	key, err := cfg.ResolvedAPIKey()
	if err != nil {
		return nil, err
	}
	main := cfg
	main.APIKey = key
	s, err := build(ctx, main, think, systemPrompt)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// ResolveBriefing constructs the briefing generator for the configured
// provider. The heuristic scorer cannot synthesize text, so it returns
// briefing.ErrBriefingUnavailable for the UI to render as a product state.
// No probe request is made: a generation is exactly one synthesis call.
func ResolveBriefing(
	ctx context.Context,
	cfg config.InferenceConfig,
	think bool,
) (briefing.Generator, error) {
	key, err := cfg.ResolvedAPIKey()
	if err != nil {
		return nil, err
	}
	main := cfg
	main.APIKey = key
	switch main.Provider {
	case "ollama":
		return ollama.New(main.Host, main.Model, main.APIKey, think, main.ModelTuning, "")
	case "openai":
		return vllm.New(vllm.OpenAI, main.Host, main.Model, main.APIKey,
			think, main.Effort, main.ModelTuning, "")
	case "vllm":
		return vllm.New(vllm.VLLM, main.Host, main.Model, main.APIKey,
			think, main.Effort, main.ModelTuning, "")
	case "heuristic":
		return nil, briefing.ErrBriefingUnavailable
	default:
		return nil, fmt.Errorf("unknown provider %q, must be ollama, openai, vllm or heuristic", main.Provider)
	}
}

// build constructs and validates the Scorer described by cfg, whose APIKey is already resolved.
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
