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

// ResolveBriefing constructs and validates the briefing generator for the
// configured provider, through the same capability negotiation as scoring:
// structured_output: false is honored, and Validate probes the backend so an
// unreachable host or a rejected think mode surfaces here instead of
// persisting a failed briefing. The heuristic scorer cannot synthesize text,
// so it returns briefing.ErrBriefingUnavailable for the UI to render as a
// product state.
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
	if main.Provider == "heuristic" {
		return nil, briefing.ErrBriefingUnavailable
	}
	// The briefing sends its own system prompt on every call, so the
	// empty-system-prompt warning build() adds for scoring does not apply.
	tuning, _ := deriveTuning(ctx, main)
	s, err := newBackend(main, tuning, think, "")
	if err != nil {
		return nil, fmt.Errorf("backend init: %w", err)
	}
	if err := s.Validate(ctx); err != nil {
		return nil, fmt.Errorf("backend validation: %w", err)
	}
	gen, ok := s.(briefing.Generator)
	if !ok {
		return nil, fmt.Errorf("provider %q cannot synthesize briefings", main.Provider)
	}
	return gen, nil
}

// deriveTuning returns cfg's model tuning with Unconstrained set for every
// non-heuristic provider whose structured output is off, plus whether that
// escape hatch is active. Both scoring and briefing synthesis go through it,
// so inference.structured_output: false behaves the same on either path.
func deriveTuning(ctx context.Context, cfg config.InferenceConfig) (rank.ModelTuning, bool) {
	tuning := cfg.ModelTuning
	if cfg.StructuredOutput != nil && !*cfg.StructuredOutput && cfg.Provider != "heuristic" {
		tuning.Unconstrained = true
		zerolog.Ctx(ctx).Debug().Str("provider", cfg.Provider).
			Msg("structured output off: the system prompt sets the JSON shape")
		return tuning, true
	}
	return tuning, false
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

// newBackend constructs the provider client described by cfg, whose APIKey is
// already resolved. Scoring and briefing synthesis share it, so a provider
// added here is available to both.
func newBackend(
	cfg config.InferenceConfig,
	tuning rank.ModelTuning,
	think bool,
	systemPrompt string,
) (rank.Scorer, error) {
	switch cfg.Provider {
	case "ollama":
		return ollama.New(cfg.Host, cfg.Model, cfg.APIKey, think, tuning, systemPrompt)
	case "openai":
		return vllm.New(vllm.OpenAI, cfg.Host, cfg.Model, cfg.APIKey,
			think, cfg.Effort, tuning, systemPrompt)
	case "vllm":
		return vllm.New(vllm.VLLM, cfg.Host, cfg.Model, cfg.APIKey,
			think, cfg.Effort, tuning, systemPrompt)
	case "heuristic":
		return rank.NewHeuristic(), nil
	default:
		return nil, fmt.Errorf("unknown provider %q, must be ollama, openai, vllm or heuristic", cfg.Provider)
	}
}

// build constructs and validates the Scorer described by cfg, whose APIKey is already resolved.
func build(ctx context.Context, cfg config.InferenceConfig, think bool, systemPrompt string) (rank.Scorer, error) {
	tuning, unconstrained := deriveTuning(ctx, cfg)
	if unconstrained && systemPrompt == "" {
		zerolog.Ctx(ctx).Warn().Str("provider", cfg.Provider).
			Msg("structured_output is false and no system prompt is sent: nothing asks the model for the JSON shape")
	}
	s, err := newBackend(cfg, tuning, think, systemPrompt)
	if err != nil {
		return nil, fmt.Errorf("backend init: %w", err)
	}
	if err := s.Validate(ctx); err != nil {
		return nil, fmt.Errorf("backend validation: %w", err)
	}
	return s, nil
}
