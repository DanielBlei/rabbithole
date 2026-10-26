// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

func TestValidRef(t *testing.T) {
	tests := []struct {
		value string
		ok    bool
	}{
		{"$RABBITHOLE_X", false},
		{"${RABBITHOLE_X}", false},
		{"$RABBITHOLE_DB", false},
		{"${RABBITHOLE_OPENAI_API_KEY}", true},
		{"$RABBITHOLE_FOO_API_KEY", true},
		{"$RABBITHOLE_INFERENCE_API_KEY", true},
		{"$AWS_SECRET", false},
		{"$RABBITHOLE_DB_PASSWORD", false},
		{"${RABBITHOLE_DB_PASSWORD}", false},
		{"sk-literal", false},
		{"$RABBITHOLE_X_API_KEY_$Y", false},
		{"$rabbithole_x", false},
		{"${RABBITHOLE_X_API_KEY", false},
		{"${}", false},
		{"x$RABBITHOLE_X_API_KEY", false},
	}
	for _, tt := range tests {
		err := ValidRef(tt.value)
		if (err == nil) != tt.ok {
			t.Errorf("ValidRef(%q) err = %v, want ok=%v", tt.value, err, tt.ok)
		}
		if err != nil &&
			(strings.Contains(err.Error(), tt.value) || !strings.Contains(err.Error(), "export RABBITHOLE_INFERENCE_API_KEY=$YOUR_VAR")) {
			t.Errorf("ValidRef(%q) error %q must hint and not echo the value", tt.value, err)
		}
	}
}

func TestRefFor(t *testing.T) {
	if RefFor("inference.api_key") != "RABBITHOLE_INFERENCE_API_KEY" ||
		RefFor("inference.summary.api_key") != "" ||
		RefFor("inference.host") != "" {
		t.Error("RefFor mismatch")
	}
}

func TestExtractLiterals(t *testing.T) {
	r := Config{Inference: InferenceConfig{APIKey: "sk-abc"}}
	got := r.ExtractLiterals()
	if len(got) != 1 ||
		got[0] != (Secret{Field: "inference.api_key", Name: "RABBITHOLE_INFERENCE_API_KEY", Value: "sk-abc"}) {
		t.Fatalf("secrets = %+v", got)
	}
	if r.Inference.APIKey != "$RABBITHOLE_INFERENCE_API_KEY" {
		t.Errorf("api_key = %q, want the canonical reference", r.Inference.APIKey)
	}
	if again := r.ExtractLiterals(); len(again) != 0 {
		t.Errorf("second extraction returned %+v", again)
	}
}

func TestResolvedAPIKey(t *testing.T) {
	t.Setenv("RABBITHOLE_INFERENCE_API_KEY", "secret1")
	c := InferenceConfig{APIKey: "$RABBITHOLE_INFERENCE_API_KEY"}
	if k, err := c.ResolvedAPIKey(); err != nil || k != "secret1" {
		t.Errorf("ResolvedAPIKey = %q, %v", k, err)
	}
	t.Setenv("RABBITHOLE_INFERENCE_API_KEY", "")
	if _, err := c.ResolvedAPIKey(); err == nil {
		t.Error("an empty variable must fail to resolve")
	}
	if k, err := (InferenceConfig{}).ResolvedAPIKey(); err != nil || k != "" {
		t.Errorf("empty key = %q, %v", k, err)
	}
}

func TestRefName(t *testing.T) {
	for in, want := range map[string]string{"$A_B": "A_B", "${A_B}": "A_B"} {
		if got, ok := RefName(in); !ok || got != want {
			t.Errorf("RefName(%q) = %q, %v", in, got, ok)
		}
	}
	if _, ok := RefName("literal"); ok {
		t.Error("a literal was taken for a reference")
	}
}
