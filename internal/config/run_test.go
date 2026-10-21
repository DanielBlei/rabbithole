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
	t.Run("heuristic drops model fields and keeps batch size", func(t *testing.T) {
		r := Config{Inference: InferenceConfig{
			Provider: "heuristic", Host: "http://x", Model: "m", Think: boolp(true), BatchSize: 4,
		}}
		dropped := r.Normalize()
		want := []string{"inference.host", "inference.model", "inference.think"}
		if !slices.Equal(dropped, want) {
			t.Errorf("dropped = %v, want %v", dropped, want)
		}
		if r.Inference.BatchSize != 4 {
			t.Errorf("batch size changed: %+v", r.Inference)
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
		Inference: InferenceConfig{Provider: "ollama", SystemPrompt: SystemPromptSetting{Text: "prompts/../sys.md"}},
		Ingest:    IngestConfig{DigestDir: "out/digests/"},
	}
	r.Normalize()
	wd, _ := os.Getwd()
	if r.Inference.SystemPrompt.Text != "prompts/../sys.md" {
		t.Errorf("system prompt text was rewritten: %q", r.Inference.SystemPrompt.Text)
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
			if in.BatchSize != defaultBatchSize || in.MaxParallel != defaultMaxParallel {
				t.Errorf("shared defaults missing: %+v", in)
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
		},
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
		"inference.provider": `invalid provider "gpt", must be ollama, openai, vllm or heuristic`,
		"inference.effort":   `inference.effort "max", must be one of low, medium, high, xhigh`,
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
			SystemPrompt: SystemPromptSetting{Text: "Score each article.\nBe strict.\n"},
			BatchSize:    5, MaxParallel: 2,
		},
		Ingest: IngestConfig{DigestDir: "/var/digests"},
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
				back, err := ParseStored(raw, RunSchemaVersion)
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
		back, err := ParseStored(raw, RunSchemaVersion)
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
		if ok.Inference.Effort != "" || ok.Inference.Host != "" {
			t.Errorf("Canonical applied defaults: %+v", ok)
		}
	})
}

func TestParseStoredRejects(t *testing.T) {
	if _, err := ParseStored([]byte("inference:\n  provider: ollama\n  bogus: 1\n"), RunSchemaVersion); err == nil {
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
	r, err := ParseStored(nil, RunSchemaVersion)
	if err != nil || r.Inference.Provider != "ollama" {
		t.Errorf("empty document = %+v, %v", r, err)
	}
}

func TestHash(t *testing.T) {
	parse := func(doc string) Config {
		t.Helper()
		r, err := ParseStoredCanonical([]byte(doc), RunSchemaVersion)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	a := parse("user: dan\ninference:\n  provider: ollama\n  model: m1\n")
	b := parse("# a comment\n\ninference:\n  model: m1\n  provider: ollama\nuser: dan\n")
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

func TestEffectiveHash(t *testing.T) {
	unset := Config{}
	pinned := Config{Inference: InferenceConfig{Provider: "ollama", Model: defaultModel, Think: boolp(true)}}
	if unset.Hash() == pinned.Hash() {
		t.Fatal("canonical hashes should differ for a pinned default")
	}
	if unset.EffectiveHash() != pinned.EffectiveHash() {
		t.Error("a pinned default must behave like an unset one")
	}
	other := Config{Inference: InferenceConfig{Model: "m2"}}
	if unset.EffectiveHash() == other.EffectiveHash() {
		t.Error("effective hash ignores a changed value")
	}
	if unset.Inference.Provider != "" {
		t.Error("EffectiveHash defaulted the receiver")
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
	if run.User != "dan" || run.Inference.Model != "m" || legacy.Since.Std() != 72*time.Hour ||
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

func TestMigrateV1(t *testing.T) {
	dir := t.TempDir()
	prompt := filepath.Join(dir, "system.md")
	if err := os.WriteFile(prompt, []byte("Be strict."), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Run("drops since and summary, inlines the prompt file, keeps the rest", func(t *testing.T) {
		old := "user: dan\ninference:\n  provider: openai\n  model: m\n  system_prompt: " + prompt + "\n" +
			"  summary:\n    model: s\ningest:\n  since: 14d\n  digest_dir: /var/d\n"
		got, err := ParseStoredCanonical([]byte(old), 1)
		if err != nil {
			t.Fatal(err)
		}
		want := Config{
			User: "dan",
			Inference: InferenceConfig{
				Provider: "openai", Model: "m", SystemPrompt: SystemPromptSetting{Text: "Be strict."},
			},
			Ingest: IngestConfig{DigestDir: "/var/d"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("migrated = %+v\nwant %+v", got, want)
		}
	})
	t.Run("an unreadable prompt file is dropped", func(t *testing.T) {
		old := "inference:\n  system_prompt: " + filepath.Join(dir, "gone.md") + "\n  model: m\n"
		got, err := ParseStoredCanonical([]byte(old), 1)
		if err != nil {
			t.Fatal(err)
		}
		if got.Inference.SystemPrompt != (SystemPromptSetting{}) || got.Inference.Model != "m" {
			t.Errorf("migrated = %+v", got.Inference)
		}
	})
	t.Run("false stays disabled", func(t *testing.T) {
		got, err := ParseStoredCanonical([]byte("inference:\n  system_prompt: false\n"), 1)
		if err != nil || !got.Inference.SystemPrompt.Disabled {
			t.Errorf("migrated = %+v, %v", got.Inference.SystemPrompt, err)
		}
	})
	t.Run("current documents are not migrated and stay strict", func(t *testing.T) {
		if _, err := ParseStoredCanonical([]byte("ingest:\n  since: 7d\n"), RunSchemaVersion); err == nil {
			t.Error("ingest.since accepted in a current document")
		}
		if _, err := ParseStoredCanonical(
			[]byte("inference:\n  summary:\n    model: s\n"),
			RunSchemaVersion,
		); err == nil {
			t.Error("inference.summary accepted in a current document")
		}
	})
}
