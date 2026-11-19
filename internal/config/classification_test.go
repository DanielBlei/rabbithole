// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/rank"
)

func TestClassificationDefaultsAndInferenceFallback(t *testing.T) {
	r := Config{Inference: InferenceConfig{
		Provider: "vllm", Host: "http://gpu:8000", Model: "qwen2.5:7b",
	}}
	if _, err := r.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	cl := r.Classification
	if cl.Provider != "vllm" || cl.Host != "http://gpu:8000" || cl.Model != "qwen2.5:7b" {
		t.Errorf("classification did not inherit the inference backend: %+v", cl)
	}
	if cl.Enabled == nil || !*cl.Enabled {
		t.Error("classification must default on")
	}
	if cl.ShowKeywords == nil || !*cl.ShowKeywords {
		t.Error("show_keywords must default on")
	}
	if cl.ShowAssumptions == nil || *cl.ShowAssumptions {
		t.Error("show_assumptions must default off")
	}
	if cl.VocabularyHint == nil || *cl.VocabularyHint {
		t.Error("vocabulary_hint must default off")
	}
	if cl.BatchSize != 1 || cl.MaxParallel != 1 {
		t.Errorf("batch defaults = %d/%d", cl.BatchSize, cl.MaxParallel)
	}
	if time.Duration(cl.Interval) != DefaultClassificationInterval {
		t.Errorf("interval = %s, want %s", cl.Interval, DefaultClassificationInterval)
	}
}

func TestClassificationIndependentProviderDefaults(t *testing.T) {
	r := Config{
		Inference:      InferenceConfig{Provider: "openai", Host: "https://api.example", Model: "big"},
		Classification: ClassificationConfig{Provider: "ollama"},
	}
	if _, err := r.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	cl := r.Classification
	if cl.Provider != "ollama" || cl.Host != defaultHost || cl.Model != defaultModel {
		t.Errorf("independent provider defaults = %+v", cl)
	}
	if !cl.ThinkEnabled() {
		t.Error("think should default on for a model-backed provider")
	}
}

func TestClassificationShowAssumptionsRequiresKeywords(t *testing.T) {
	off, on := false, true
	r := Config{Classification: ClassificationConfig{
		ShowKeywords: &off, ShowAssumptions: &on,
	}}
	_, err := r.Prepare()
	if err == nil {
		t.Fatal("Prepare accepted assumptions without keywords")
	}
	found := false
	for _, fe := range FieldErrors(err) {
		if fe.Field == "classification.show_assumptions" &&
			strings.Contains(fe.Error(), "requires classification.show_keywords") {
			found = true
		}
	}
	if !found {
		t.Errorf("field errors = %v", FieldErrors(err))
	}
}

func TestClassificationNormalizeDropsNonApplicable(t *testing.T) {
	r := Config{Classification: ClassificationConfig{
		Provider: "vllm", Effort: "high",
		ModelTuning: rank.ModelTuning{NumCtx: 4096},
	}}
	dropped := r.Normalize()
	if !reflect.DeepEqual(dropped, []string{"classification.effort", "classification.model_tuning.num_ctx"}) {
		t.Errorf("dropped = %v", dropped)
	}
	if r.Classification.Effort != "" || r.Classification.ModelTuning.NumCtx != 0 {
		t.Errorf("normalization left non-applicable fields: %+v", r.Classification)
	}
}

func TestClassificationYAMLRoundTrip(t *testing.T) {
	on := true
	r := Config{Classification: ClassificationConfig{
		Provider: "ollama", Model: "qwen2.5:3b", ShowAssumptions: &on,
		ShowKeywords: &on, VocabularyHint: &on, Interval: Duration(5 * time.Minute),
	}}
	raw, err := r.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, want := range []string{"classification:", "provider: ollama", "model: qwen2.5:3b",
		"show_assumptions: true", "vocabulary_hint: true", "interval: 5m"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("marshaled YAML missing %q:\n%s", want, raw)
		}
	}
	empty, err := (Config{}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(empty), "classification:") {
		t.Errorf("empty config emitted a classification block:\n%s", empty)
	}
}

func TestClassificationParseStored(t *testing.T) {
	raw := []byte(`classification:
  provider: ollama
  model: qwen2.5:3b
  show_keywords: false
`)
	r, err := ParseStored(raw, RunSchemaVersion)
	if err != nil {
		t.Fatalf("ParseStored: %v", err)
	}
	if r.Classification.Provider != "ollama" || r.Classification.Model != "qwen2.5:3b" {
		t.Errorf("classification = %+v", r.Classification)
	}
	if r.Classification.ShowKeywords == nil || *r.Classification.ShowKeywords {
		t.Error("show_keywords: false was not kept")
	}
	if off := r.Classification.ShowAssumptions; off == nil || *off {
		t.Error("show_assumptions must stay off")
	}
}

func TestClassificationExtractLiteralKey(t *testing.T) {
	r := Config{Classification: ClassificationConfig{APIKey: "secret"}}
	secrets := r.ExtractLiterals()
	if len(secrets) != 1 || secrets[0].Field != classificationKeyField ||
		secrets[0].Name != "RABBITHOLE_CLASSIFICATION_API_KEY" {
		t.Fatalf("secrets = %+v", secrets)
	}
	if r.Classification.APIKey != "$RABBITHOLE_CLASSIFICATION_API_KEY" {
		t.Errorf("api key = %q", r.Classification.APIKey)
	}
}
