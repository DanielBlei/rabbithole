// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func boolp(b bool) *bool { return &b }

func TestNormalizeDropsNonApplicable(t *testing.T) {
	t.Run("ollama drops effort without error", func(t *testing.T) {
		r := Config{Inference: InferenceConfig{Provider: "ollama", Effort: "high"}}
		dropped, err := r.Prepare()
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(dropped, []string{"inference.effort"}) {
			t.Errorf("dropped = %v", dropped)
		}
		if r.Inference.Effort != "" {
			t.Errorf("effort = %q, want cleared", r.Inference.Effort)
		}
	})
	t.Run("heuristic drops model fields and keeps summary", func(t *testing.T) {
		r := Config{Inference: InferenceConfig{
			Provider: "heuristic", Host: "http://x", Model: "m", Think: boolp(true),
			BatchSize: 4, Summary: SummaryConfig{Model: "s", Host: "http://s"},
		}}
		dropped := r.Normalize()
		want := []string{"inference.host", "inference.model", "inference.think"}
		if !slices.Equal(dropped, want) {
			t.Errorf("dropped = %v, want %v", dropped, want)
		}
		if r.Inference.BatchSize != 4 || r.Inference.Summary.Model != "s" || r.Inference.Summary.Host != "http://s" {
			t.Errorf("batch size or summary changed: %+v", r.Inference)
		}
	})
	t.Run("unset fields are not reported", func(t *testing.T) {
		r := Config{Inference: InferenceConfig{Provider: "heuristic"}}
		if d := r.Normalize(); len(d) != 0 {
			t.Errorf("dropped = %v", d)
		}
	})
	t.Run("num_ctx is ollama only", func(t *testing.T) {
		r := Config{Inference: InferenceConfig{Provider: "vllm"}}
		r.Inference.ModelTuning.NumCtx = 8192
		r.Inference.ModelTuning.MaxTokens = 100
		d := r.Normalize()
		if !slices.Equal(d, []string{"inference.model_tuning.num_ctx"}) || r.Inference.ModelTuning.MaxTokens != 100 {
			t.Errorf("dropped = %v tuning = %+v", d, r.Inference.ModelTuning)
		}
	})
	t.Run("unknown provider is left for validate", func(t *testing.T) {
		r := Config{Inference: InferenceConfig{Provider: "claude", Host: "h"}}
		if d := r.Normalize(); len(d) != 0 || r.Inference.Host != "h" {
			t.Errorf("dropped = %v", d)
		}
	})
}

func TestNormalizeAbsolutisesPaths(t *testing.T) {
	r := Config{
		Inference: InferenceConfig{Provider: "ollama", SystemPrompt: SystemPromptSetting{Path: "prompts/../sys.md"}},
		Ingest:    IngestConfig{DigestDir: "out/digests/"},
	}
	r.Normalize()
	wd, _ := os.Getwd()
	if want := filepath.Join(wd, "sys.md"); r.Inference.SystemPrompt.Path != want {
		t.Errorf("system prompt = %q, want %q", r.Inference.SystemPrompt.Path, want)
	}
	if want := filepath.Join(wd, "out", "digests"); r.Ingest.DigestDir != want {
		t.Errorf("digest dir = %q, want %q", r.Ingest.DigestDir, want)
	}
}

func TestPrepareDefaultsAreProviderAware(t *testing.T) {
	tests := []struct {
		provider string
		host     string
		model    string
		think    bool
		effort   string
		so       bool
	}{
		{"heuristic", "", "", false, "", false},
		{"ollama", defaultHost, defaultModel, true, "", true},
		{"vllm", defaultHost, defaultModel, true, "", true},
		{"openai", defaultHost, defaultModel, true, defaultEffort, true},
		{"", defaultHost, defaultModel, true, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			r := Config{Inference: InferenceConfig{Provider: tt.provider}}
			if _, err := r.Prepare(); err != nil {
				t.Fatal(err)
			}
			in := r.Inference
			if in.Host != tt.host || in.Model != tt.model || in.Effort != tt.effort {
				t.Errorf("host/model/effort = %q/%q/%q", in.Host, in.Model, in.Effort)
			}
			if (in.Think != nil) != tt.think || (in.StructuredOutput != nil) != tt.so {
				t.Errorf("think=%v structured=%v", in.Think, in.StructuredOutput)
			}
			if in.ThinkEnabled() != tt.think {
				t.Errorf("ThinkEnabled = %v", in.ThinkEnabled())
			}
			if in.BatchSize != defaultBatchSize || in.MaxParallel != defaultMaxParallel ||
				r.Ingest.Since.Std() != defaultSince {
				t.Errorf("shared defaults missing: %+v %v", in, r.Ingest.Since)
			}
		})
	}
}

func TestRunDefaults(t *testing.T) {
	d := RunDefaults()
	if d.Inference.Provider != "ollama" || d.Inference.Host != defaultHost || !d.Inference.ThinkEnabled() {
		t.Errorf("defaults = %+v", d)
	}
	if (InferenceConfig{}).ThinkEnabled() {
		t.Error("nil think must be off")
	}
}

func TestValidateFieldErrors(t *testing.T) {
	r := Config{
		Inference: InferenceConfig{
			Provider: "gpt",
			Effort:   "max",
			Summary:  SummaryConfig{Host: "http://s", Effort: "bogus"},
		},
		Ingest: IngestConfig{Since: Duration(-time.Hour)},
	}
	_, err := r.Prepare()
	if err == nil {
		t.Fatal("expected errors")
	}
	got := map[string]string{}
	for _, fe := range FieldErrors(err) {
		got[fe.Field] = fe.Error()
	}
	want := map[string]string{
		"inference.provider":       `invalid provider "gpt", must be ollama, openai, vllm or heuristic`,
		"inference.effort":         `inference.effort "max", must be one of low, medium, high, xhigh`,
		"inference.summary.effort": `inference.summary.effort "bogus", must be one of low, medium, high, xhigh`,
		"inference.summary.model":  "inference.summary.model is required when other inference.summary fields are set",
		"ingest.since":             "since must be positive, got -1h0m0s",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("field errors = %v\nwant %v", got, want)
	}

	r = Config{Inference: InferenceConfig{Provider: "openai", Effort: "max"}}
	_, err = r.Prepare()
	if fe := FieldErrors(
		err,
	); len(fe) != 1 ||
		fe[0].Error() != `inference.effort "max", must be one of low, medium, high, xhigh` {
		t.Errorf("effort errors = %v", fe)
	}
}

func TestValidateRejectsNonReferenceKeys(t *testing.T) {
	r := Config{Inference: InferenceConfig{APIKey: "sk-literal-secret"}}
	_, err := r.Prepare()
	fe := FieldErrors(err)
	if len(fe) != 1 || fe[0].Field != "inference.api_key" {
		t.Fatalf("errors = %v", fe)
	}
	if strings.Contains(err.Error(), "sk-literal-secret") {
		t.Error("error echoes the secret")
	}
	ok := Config{Inference: InferenceConfig{APIKey: "$RABBITHOLE_INFERENCE_API_KEY"}}
	if _, err := ok.Prepare(); err != nil {
		t.Errorf("allow-listed reference rejected: %v", err)
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	r := Config{
		User: "dan",
		Inference: InferenceConfig{
			Provider: "openai", Host: "https://api.example", Model: "m", APIKey: "$RABBITHOLE_INFERENCE_API_KEY",
			Think: boolp(false), Effort: "high", StructuredOutput: boolp(false),
			SystemPrompt: SystemPromptSetting{Path: "/etc/sys.md"},
			BatchSize:    5, MaxParallel: 2,
			Summary: SummaryConfig{Model: "s", Think: boolp(false)},
		},
		Ingest: IngestConfig{Since: Duration(14 * 24 * time.Hour), DigestDir: "/var/digests"},
	}
	r.Inference.ModelTuning.MaxTokens = 900
	if _, err := r.Canonical(); err != nil {
		t.Fatal(err)
	}
	raw, err := r.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	canon, err := ParseStoredCanonical(raw, RunSchemaVersion)
	if err != nil || !reflect.DeepEqual(canon, r) {
		t.Fatalf("canonical round trip: %v\n%s\n%+v\n%+v", err, raw, canon, r)
	}
	back, err := ParseStored(raw, RunSchemaVersion)
	if err != nil {
		t.Fatalf("%v\n%s", err, raw)
	}
	eff := r
	if _, err := eff.Prepare(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, eff) {
		t.Errorf("round trip differs:\n%s\n%+v\n%+v", raw, back, eff)
	}
	if canon.Hash() != r.Hash() {
		t.Error("hash differs after round trip")
	}

	t.Run("only set keys are written", func(t *testing.T) {
		tests := []struct {
			name string
			in   Config
			want string
		}{
			{"empty", Config{}, "{}\n"},
			{
				"heuristic",
				Config{Inference: InferenceConfig{Provider: "heuristic"}},
				"inference:\n  provider: heuristic\n",
			},
			{
				"ollama",
				Config{Inference: InferenceConfig{Provider: "ollama", Model: "m"}},
				"inference:\n  provider: ollama\n  model: m\n",
			},
			{
				"openai",
				Config{Inference: InferenceConfig{Provider: "openai", Effort: "low"}},
				"inference:\n  provider: openai\n  effort: low\n",
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				if _, err := tt.in.Canonical(); err != nil {
					t.Fatal(err)
				}
				raw, _ := tt.in.Marshal()
				if string(raw) != tt.want {
					t.Errorf("yaml =\n%s\nwant\n%s", raw, tt.want)
				}
				back, err := ParseStored(raw, 1)
				eff := tt.in
				_, _ = eff.Prepare()
				if err != nil || !reflect.DeepEqual(back, eff) {
					t.Errorf("round trip: %v", err)
				}
			})
		}
	})
	t.Run("disabled system prompt", func(t *testing.T) {
		d := Config{Inference: InferenceConfig{SystemPrompt: SystemPromptSetting{Disabled: true}}}
		_, _ = d.Canonical()
		raw, _ := d.Marshal()
		if !strings.Contains(string(raw), "system_prompt: false") {
			t.Errorf("yaml =\n%s", raw)
		}
		back, err := ParseStored(raw, 1)
		if err != nil || !back.Inference.SystemPrompt.Disabled {
			t.Errorf("round trip: %v %+v", err, back.Inference.SystemPrompt)
		}
	})
	t.Run("canonical reports prepare's errors without defaulting", func(t *testing.T) {
		bad := Config{Inference: InferenceConfig{Provider: "openai", Effort: "max"}}
		_, err := bad.Canonical()
		if len(FieldErrors(err)) != 1 {
			t.Errorf("errors = %v", err)
		}
		ok := Config{Inference: InferenceConfig{Provider: "openai"}}
		_, _ = ok.Canonical()
		if ok.Inference.Effort != "" || ok.Inference.Host != "" || ok.Ingest.Since != 0 {
			t.Errorf("Canonical applied defaults: %+v", ok)
		}
	})
}

func TestParseStoredRejects(t *testing.T) {
	if _, err := ParseStored([]byte("inference:\n  provider: ollama\n  bogus: 1\n"), 1); err == nil {
		t.Error("unknown key accepted")
	}
	if _, err := ParseStored(
		[]byte("user: x\n"),
		RunSchemaVersion+1,
	); err == nil ||
		!strings.Contains(err.Error(), "newer rabbithole") {
		t.Errorf("newer schema error = %v", err)
	}
	if _, err := ParseStored([]byte("user: x\n"), 0); err == nil {
		t.Error("schema 0 accepted")
	}
	r, err := ParseStored(nil, 1)
	if err != nil || r.Inference.Provider != "ollama" {
		t.Errorf("empty document = %+v, %v", r, err)
	}
}

func TestHash(t *testing.T) {
	parse := func(doc string) Config {
		t.Helper()
		r, err := ParseStoredCanonical([]byte(doc), 1)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	a := parse("user: dan\ninference:\n  provider: ollama\n  model: m1\ningest:\n  since: 7d\n")
	b := parse("# a comment\ningest: {since: 168h}\n\ninference:\n  model: m1\n  provider: ollama\nuser: dan\n")
	if a.Hash() != b.Hash() {
		t.Error("equal configs with different spelling hash differently")
	}
	c := b
	c.Inference.Model = "m2"
	if c.Hash() == b.Hash() {
		t.Error("hash ignores a changed value")
	}
	d := b
	d.Inference.Think = boolp(false)
	if d.Hash() == b.Hash() {
		t.Error("hash ignores think")
	}
	if parse("{}").Hash() == parse("inference:\n  model: "+defaultModel+"\n").Hash() {
		t.Error("a pinned default must hash differently from an unset one")
	}
}

func TestFileSplit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := `user: dan
profile: profile.md
inference:
  provider: ollama
  effort: high
  model: m
ingest:
  since: 3d
  digest_dir: out
store:
  db_path: data/x.db
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	run, store, legacy, dropped := f.Split()
	if store.DBPath != "data/x.db" {
		t.Errorf("store = %+v", store)
	}
	if legacy.ProfilePath != "profile.md" || legacy.FeedsPath != "" {
		t.Errorf("legacy = %+v", legacy)
	}
	if !slices.Equal(dropped, []string{"inference.effort"}) {
		t.Errorf("dropped = %v", dropped)
	}
	if run.User != "dan" || run.Inference.Model != "m" || run.Ingest.Since.Std() != 72*time.Hour ||
		!filepath.IsAbs(run.Ingest.DigestDir) {
		t.Errorf("run = %+v", run)
	}

	explicit, _ := ParseFile([]byte("ingest:\n  feeds: my/feeds.yaml\nsome_unknown: 1\n"))
	if _, _, l, _ := explicit.Split(); l.FeedsPath != "my/feeds.yaml" {
		t.Errorf("explicit feeds path = %q", l.FeedsPath)
	}
	bare, _ := ParseFile([]byte("user: x\n"))
	if _, _, l, _ := bare.Split(); l.FeedsPath != "" {
		t.Errorf("no path, no feeds: %q", l.FeedsPath)
	}
	if _, err := ReadFile(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Error("missing file accepted")
	}
}
