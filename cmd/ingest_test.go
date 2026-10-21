// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"testing"

	"github.com/DanielBlei/rabbithole/internal/config"
)

func TestApplyIngestOverridesStartsFromTheCanonicalForm(t *testing.T) {
	noThink := false
	stored := config.Config{Inference: config.InferenceConfig{Provider: "ollama", Think: &noThink}}
	stored.Inference.ModelTuning.NumCtx = 8192

	run, err := applyIngestOverrides(stored, "openai")
	if err != nil {
		t.Fatal(err)
	}
	in := run.Inference
	if in.Provider != "openai" {
		t.Errorf("provider = %q, want openai", in.Provider)
	}
	if in.ModelTuning.NumCtx != 0 {
		t.Errorf("num_ctx = %d: an ollama-only setting must not reach openai", in.ModelTuning.NumCtx)
	}
	if in.Effort != "medium" {
		t.Errorf("effort = %q, want openai's default", in.Effort)
	}
	if in.Think == nil || *in.Think {
		t.Errorf("think = %v, want the stored false kept", in.Think)
	}
	if stored.Inference.Provider != "ollama" || stored.Inference.Effort != "" {
		t.Errorf("the stored config was changed: %+v", stored.Inference)
	}
}

func TestApplyIngestOverridesWithoutAFlagJustPreparesTheStoredConfig(t *testing.T) {
	run, err := applyIngestOverrides(config.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if run.Inference.Provider != "ollama" || run.Inference.Host == "" {
		t.Errorf("run = %+v, want the defaults filled in", run.Inference)
	}
	if _, err := applyIngestOverrides(config.Config{}, "nope"); err == nil {
		t.Error("an unknown --provider was accepted")
	}
}

func TestDigestDirPrecedence(t *testing.T) {
	for _, tc := range []struct{ out, stored, want string }{
		{"flag", "stored", "flag"},
		{"", "stored", "stored"},
		{"", "", "digests"},
	} {
		if got := digestDir(tc.out, tc.stored); got != tc.want {
			t.Errorf("digestDir(%q, %q) = %q, want %q", tc.out, tc.stored, got, tc.want)
		}
	}
}
