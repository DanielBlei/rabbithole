// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package config

import "testing"

func TestAppliesMatrix(t *testing.T) {
	const (
		o = ProviderOllama
		v = ProviderVLLM
		a = ProviderOpenAI
		h = ProviderHeuristic
	)
	want := map[string]map[string]bool{
		"host":                          {o: true, v: true, a: true},
		"model":                         {o: true, v: true, a: true},
		"api_key":                       {o: true, v: true, a: true},
		"think":                         {o: true, v: true, a: true},
		"effort":                        {a: true},
		"structured_output":             {o: true, v: true, a: true},
		"system_prompt":                 {o: true, v: true, a: true},
		"batch_size":                    {o: true, v: true, a: true, h: true},
		"max_parallel":                  {o: true, v: true, a: true, h: true},
		"model_tuning.num_ctx":          {o: true},
		"model_tuning.max_tokens":       {o: true, v: true, a: true},
		"model_tuning.tokens_per_item":  {o: true, v: true, a: true},
		"model_tuning.tokens_overhead":  {o: true, v: true, a: true},
		"model_tuning.tokens_thinking":  {o: true, v: true, a: true},
		"model_tuning.reason_max_chars": {o: true, v: true, a: true},
	}
	fields := InferenceFields()
	if len(fields) != len(want) {
		t.Fatalf("InferenceFields has %d entries, matrix has %d", len(fields), len(want))
	}
	for _, field := range fields {
		for _, p := range Providers {
			if got := Applies(p, field); got != want[field][p] {
				t.Errorf("Applies(%s, %s) = %v, want %v", p, field, got, want[field][p])
			}
		}
	}
	if !Applies("", "host") || Applies("", "effort") {
		t.Error("empty provider must behave as the default provider")
	}
	if Applies("claude", "host") || Applies(o, "nope") {
		t.Error("unknown provider or field must apply to nothing")
	}
}
