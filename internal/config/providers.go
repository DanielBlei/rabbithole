// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"slices"
)

// Provider names accepted by inference.provider.
const (
	ProviderOllama    = "ollama"
	ProviderVLLM      = "vllm"
	ProviderOpenAI    = "openai"
	ProviderHeuristic = "heuristic"
)

// Providers lists the accepted inference providers in display order.
var Providers = []string{ProviderOllama, ProviderVLLM, ProviderOpenAI, ProviderHeuristic}

var (
	allProviders = Providers
	modelBacked  = []string{ProviderOllama, ProviderVLLM, ProviderOpenAI}
)

// inferenceField is one row of the applicability table: an inference field
// (named without the "inference." prefix), the providers that read it, and how
// to test and clear it on an InferenceConfig.
type inferenceField struct {
	name      string
	providers []string
	isSet     func(*InferenceConfig) bool
	clear     func(*InferenceConfig)
}

// inferenceFields is the single source of truth for which inference fields each
// provider reads. Normalize clears the rest; the settings UI iterates it.
var inferenceFields = []inferenceField{
	{"host", modelBacked,
		func(c *InferenceConfig) bool { return c.Host != "" },
		func(c *InferenceConfig) { c.Host = "" }},
	{"model", modelBacked,
		func(c *InferenceConfig) bool { return c.Model != "" },
		func(c *InferenceConfig) { c.Model = "" }},
	{"api_key", modelBacked,
		func(c *InferenceConfig) bool { return c.APIKey != "" },
		func(c *InferenceConfig) { c.APIKey = "" }},
	{"think", modelBacked,
		func(c *InferenceConfig) bool { return c.Think != nil },
		func(c *InferenceConfig) { c.Think = nil }},
	{"effort", []string{ProviderOpenAI},
		func(c *InferenceConfig) bool { return c.Effort != "" },
		func(c *InferenceConfig) { c.Effort = "" }},
	{"structured_output", modelBacked,
		func(c *InferenceConfig) bool { return c.StructuredOutput != nil },
		func(c *InferenceConfig) { c.StructuredOutput = nil }},
	{"system_prompt", modelBacked,
		func(c *InferenceConfig) bool { return c.SystemPrompt != (SystemPromptSetting{}) },
		func(c *InferenceConfig) { c.SystemPrompt = SystemPromptSetting{} }},
	{"batch_size", allProviders,
		func(c *InferenceConfig) bool { return c.BatchSize != 0 },
		func(c *InferenceConfig) { c.BatchSize = 0 }},
	{"max_parallel", allProviders,
		func(c *InferenceConfig) bool { return c.MaxParallel != 0 },
		func(c *InferenceConfig) { c.MaxParallel = 0 }},
	{"model_tuning.num_ctx", []string{ProviderOllama},
		func(c *InferenceConfig) bool { return c.ModelTuning.NumCtx != 0 },
		func(c *InferenceConfig) { c.ModelTuning.NumCtx = 0 }},
	{"model_tuning.max_tokens", modelBacked,
		func(c *InferenceConfig) bool { return c.ModelTuning.MaxTokens != 0 },
		func(c *InferenceConfig) { c.ModelTuning.MaxTokens = 0 }},
	{"model_tuning.tokens_per_item", modelBacked,
		func(c *InferenceConfig) bool { return c.ModelTuning.TokensPerItem != 0 },
		func(c *InferenceConfig) { c.ModelTuning.TokensPerItem = 0 }},
	{"model_tuning.tokens_overhead", modelBacked,
		func(c *InferenceConfig) bool { return c.ModelTuning.TokensOverhead != 0 },
		func(c *InferenceConfig) { c.ModelTuning.TokensOverhead = 0 }},
	{"model_tuning.tokens_thinking", modelBacked,
		func(c *InferenceConfig) bool { return c.ModelTuning.TokensThinking != 0 },
		func(c *InferenceConfig) { c.ModelTuning.TokensThinking = 0 }},
	{"model_tuning.reason_max_chars", modelBacked,
		func(c *InferenceConfig) bool { return c.ModelTuning.ReasonMaxChars != 0 },
		func(c *InferenceConfig) { c.ModelTuning.ReasonMaxChars = 0 }},
}

// InferenceFields returns the inference field names in display order. Names carry
// no "inference." prefix.
func InferenceFields() []string {
	out := make([]string, 0, len(inferenceFields))
	for _, f := range inferenceFields {
		out = append(out, f.name)
	}
	return out
}

// Applies reports whether the inference field (e.g. "effort",
// "model_tuning.num_ctx") is read by the provider. An empty provider means the
// default provider; an unknown provider or field applies to nothing.
func Applies(provider, field string) bool {
	if provider == "" {
		provider = defaultProvider
	}
	for _, f := range inferenceFields {
		if f.name == field {
			return slices.Contains(f.providers, provider)
		}
	}
	return false
}
