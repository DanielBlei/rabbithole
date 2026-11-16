// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

// Package inference resolves the configured backend into a rank.Scorer.
package inference

import (
	"context"
	"fmt"

	"github.com/rs/zerolog"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/decisionbrief"
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

// ResolveBrief constructs the Decision Brief generator for the configured
// provider and reports the provider/model of the client it built, from that one
// configuration snapshot. The heuristic scorer cannot read full text, so it
// returns decisionbrief.ErrBriefUnavailable for the UI to render as a product
// state.
//
// Capability negotiation is shared with the scoring client: an unsupported
// Ollama think mode or an OpenAI reasoning_effort the model rejects is learned
// once and reused by later dives. The negotiation is cached, so a dive does not
// repeat a model probe.
func ResolveBrief(
	ctx context.Context,
	cfg config.InferenceConfig,
	think bool,
) (decisionbrief.Generator, decisionbrief.Provenance, error) {
	key, err := cfg.ResolvedAPIKey()
	if err != nil {
		return nil, decisionbrief.Provenance{}, err
	}
	main := cfg
	main.APIKey = key
	tuning := main.ModelTuning
	structured := main.StructuredOutput == nil || *main.StructuredOutput
	if !structured {
		// The same escape hatch scoring uses: the client omits the provider's
		// schema field, and the prompt carries the field contract instead.
		tuning.Unconstrained = true
		zerolog.Ctx(ctx).Debug().Str("provider", main.Provider).
			Msg("structured output off: the system prompt supplies the decision brief field contract")
	}
	prov := decisionbrief.Provenance{Provider: main.Provider, Model: main.Model}
	switch main.Provider {
	case "ollama":
		gen, err := ollama.New(main.Host, main.Model, main.APIKey, think, tuning, "")
		if err != nil {
			return nil, decisionbrief.Provenance{}, fmt.Errorf("backend init: %w", err)
		}
		if think {
			gen.NegotiateThink(ctx)
		}
		return gen, prov, nil
	case "openai", "vllm":
		flavor := vllm.OpenAI
		if main.Provider == "vllm" {
			flavor = vllm.VLLM
		}
		gen, err := vllm.New(flavor, main.Host, main.Model, main.APIKey,
			think, main.Effort, tuning, "")
		if err != nil {
			return nil, decisionbrief.Provenance{}, fmt.Errorf("backend init: %w", err)
		}
		if flavor == vllm.OpenAI {
			if err := gen.NegotiateEffort(ctx); err != nil {
				return nil, decisionbrief.Provenance{}, fmt.Errorf("backend negotiation: %w", err)
			}
		}
		return gen, prov, nil
	case "heuristic":
		return nil, decisionbrief.Provenance{}, decisionbrief.ErrBriefUnavailable
	default:
		return nil, decisionbrief.Provenance{}, fmt.Errorf(
			"unknown provider %q, must be ollama, openai, vllm or heuristic", main.Provider)
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
