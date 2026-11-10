// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"slices"
	"time"

	"github.com/DanielBlei/rabbithole/internal/rank"
)

// DefaultClassificationInterval is how often the classification worker scans
// for scored items that still need a verdict.
const DefaultClassificationInterval = 10 * time.Minute

// ClassificationConfig configures the content classifier and how its keywords
// and assumptions are shown. It deliberately mirrors InferenceConfig: when the
// provider or model is left unset, the classifier falls back to the inference
// backend, so one local endpoint only has to be configured once.
type ClassificationConfig struct {
	Enabled *bool `yaml:"enabled,omitempty"` // run the background classifier; defaults true

	Provider string `yaml:"provider,omitempty"` // ollama | openai | vllm | heuristic; defaults to inference.provider
	Host     string `yaml:"host,omitempty"`
	Model    string `yaml:"model,omitempty"`
	APIKey   string `yaml:"api_key,omitempty"`
	Think    *bool  `yaml:"think,omitempty"`
	Effort   string `yaml:"effort,omitempty"`

	StructuredOutput *bool `yaml:"structured_output,omitempty"`

	BatchSize   int `yaml:"batch_size,omitempty"`   // items per classification request
	MaxParallel int `yaml:"max_parallel,omitempty"` // concurrent classification requests in flight

	// Interval is how often the serve worker self-heals the classification
	// queue. Zero takes DefaultClassificationInterval.
	Interval Duration `yaml:"interval,omitempty"`

	ModelTuning rank.ModelTuning `yaml:"model_tuning,omitempty"`

	// Display defaults: keywords on, assumptions off. ShowAssumptions may only
	// be true while ShowKeywords is true.
	ShowKeywords    *bool `yaml:"show_keywords,omitempty"`
	ShowAssumptions *bool `yaml:"show_assumptions,omitempty"`

	// VocabularyHint adds the current top tags to the prompt. Off by default:
	// the vocabulary stays open, and the hint is a reversible drift control.
	VocabularyHint *bool `yaml:"vocabulary_hint,omitempty"`
}

// ResolvedAPIKey expands the api_key reference, naming the field and never the value.
func (c ClassificationConfig) ResolvedAPIKey() (string, error) {
	return expandEnvRef(classificationKeyField, c.APIKey)
}

// ThinkEnabled reports whether reasoning is on. Unset means off until defaults fill it.
func (c ClassificationConfig) ThinkEnabled() bool { return c.Think != nil && *c.Think }

// applyClassificationDefaults fills the classification block. Unset backend
// fields inherit the inference block when both resolve to the same provider,
// which is the single-endpoint default; a separately chosen provider gets the
// same built-in endpoint defaults inference would.
func (r *Config) applyClassificationDefaults() {
	in := &r.Inference
	cl := &r.Classification
	if cl.Provider == "" {
		cl.Provider = in.Provider
	}
	if cl.Provider == in.Provider {
		if cl.Host == "" {
			cl.Host = in.Host
		}
		if cl.Model == "" {
			cl.Model = in.Model
		}
		if cl.APIKey == "" {
			cl.APIKey = in.APIKey
		}
		if cl.Think == nil {
			cl.Think = cloneBool(in.Think)
		}
		if cl.Effort == "" {
			cl.Effort = in.Effort
		}
		if cl.StructuredOutput == nil {
			cl.StructuredOutput = cloneBool(in.StructuredOutput)
		}
	} else {
		if cl.Host == "" {
			cl.Host = defaultHost
		}
		if cl.Model == "" {
			cl.Model = defaultModel
		}
		if cl.Think == nil {
			t := true
			cl.Think = &t
		}
		if cl.Effort == "" {
			cl.Effort = defaultEffort
		}
		if cl.StructuredOutput == nil {
			t := true
			cl.StructuredOutput = &t
		}
	}
	if cl.BatchSize <= 0 {
		cl.BatchSize = defaultBatchSize
	}
	if cl.MaxParallel <= 0 {
		cl.MaxParallel = defaultMaxParallel
	}
	if cl.Enabled == nil {
		t := true
		cl.Enabled = &t
	}
	if cl.ShowKeywords == nil {
		t := true
		cl.ShowKeywords = &t
	}
	if cl.ShowAssumptions == nil {
		f := false
		cl.ShowAssumptions = &f
	}
	if cl.VocabularyHint == nil {
		f := false
		cl.VocabularyHint = &f
	}
	if cl.Interval == 0 {
		cl.Interval = Duration(DefaultClassificationInterval)
	}
}

// normalizeClassification clears classification backend fields the selected
// provider does not read, mirroring Normalize's treatment of inference.
func (r *Config) normalizeClassification(dropped *[]string) {
	cl := &r.Classification
	provider := cl.Provider
	if provider == "" {
		provider = r.Inference.Provider
	}
	if provider == "" {
		provider = defaultProvider
	}
	if !slices.Contains(Providers, provider) {
		return
	}
	view := InferenceConfig{
		Provider: provider, Host: cl.Host, Model: cl.Model, APIKey: cl.APIKey,
		Think: cl.Think, Effort: cl.Effort, StructuredOutput: cl.StructuredOutput,
		BatchSize: cl.BatchSize, MaxParallel: cl.MaxParallel, ModelTuning: cl.ModelTuning,
	}
	for _, f := range inferenceFields {
		if !slices.Contains(f.providers, provider) && f.isSet(&view) {
			f.clear(&view)
			*dropped = append(*dropped, "classification."+f.name)
		}
	}
	cl.Host, cl.Model, cl.APIKey = view.Host, view.Model, view.APIKey
	cl.Think, cl.Effort, cl.StructuredOutput = view.Think, view.Effort, view.StructuredOutput
	cl.BatchSize, cl.MaxParallel, cl.ModelTuning = view.BatchSize, view.MaxParallel, view.ModelTuning
}
