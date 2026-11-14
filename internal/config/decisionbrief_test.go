// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"

	"github.com/DanielBlei/rabbithole/internal/articleextract"
)

func TestDecisionBriefDefaultsAndCaps(t *testing.T) {
	got := DecisionBriefConfig{}.WithDefaults()
	if got.MaxArticleBytes != DefaultDecisionBriefMaxArticleBytes ||
		got.MaxTextBytes != DefaultDecisionBriefMaxTextBytes ||
		got.MaxOutputTokens != DefaultDecisionBriefMaxOutputTokens {
		t.Fatalf("defaults = %+v", got)
	}
	capped := DecisionBriefConfig{
		MaxArticleBytes: MaxDecisionBriefMaxArticleBytes + 1,
		MaxTextBytes:    MaxDecisionBriefMaxTextBytes + 1,
		MaxOutputTokens: MaxDecisionBriefMaxOutputTokens + 1,
	}.WithDefaults()
	if capped.MaxArticleBytes != MaxDecisionBriefMaxArticleBytes ||
		capped.MaxTextBytes != MaxDecisionBriefMaxTextBytes ||
		capped.MaxOutputTokens != MaxDecisionBriefMaxOutputTokens {
		t.Fatalf("caps = %+v", capped)
	}
	floored := DecisionBriefConfig{MaxOutputTokens: 1}.WithDefaults()
	if floored.MaxOutputTokens != MinDecisionBriefMaxOutputTokens {
		t.Fatalf("floor = %d, want %d", floored.MaxOutputTokens, MinDecisionBriefMaxOutputTokens)
	}
}

func TestDecisionBriefParseAndSplit(t *testing.T) {
	f, err := ParseFile(
		[]byte("decision_brief:\n  max_article_bytes: 1048576\n  max_text_bytes: 8192\n  max_output_tokens: 512\n"),
	)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	run, _, _, _ := f.Split()
	if run.DecisionBrief.MaxArticleBytes != 1048576 ||
		run.DecisionBrief.MaxTextBytes != 8192 ||
		run.DecisionBrief.MaxOutputTokens != 512 {
		t.Fatalf("split = %+v", run.DecisionBrief)
	}
	if _, err := run.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if run.DecisionBrief.MaxOutputTokens != 512 {
		t.Fatalf("explicit value was rewritten: %+v", run.DecisionBrief)
	}
}

// The documented contract is: zero/omitted takes defaults, upper bounds clamp,
// and raw negative values are rejected before defaulting can hide them.
func TestDecisionBriefRawNegativeRejected(t *testing.T) {
	negatives := DecisionBriefConfig{
		MaxArticleBytes: -1,
		MaxTextBytes:    -1,
		MaxOutputTokens: -1,
	}
	wantFields := map[string]string{
		"decision_brief.max_article_bytes": "must not be negative",
		"decision_brief.max_text_bytes":    "must not be negative",
		"decision_brief.max_output_tokens": "must not be negative",
	}
	for _, tc := range []struct {
		name string
		run  func(*Config) ([]string, error)
	}{
		{"Prepare", func(c *Config) ([]string, error) { return c.Prepare() }},
		{"Canonical", func(c *Config) ([]string, error) { return c.Canonical() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{DecisionBrief: negatives}
			_, err := tc.run(&cfg)
			remaining := map[string]string{}
			for k, v := range wantFields {
				remaining[k] = v
			}
			for _, fe := range FieldErrors(err) {
				want, ok := remaining[fe.Field]
				if !ok {
					t.Errorf("unexpected field error %+v", fe)
					continue
				}
				if !strings.Contains(fe.Msg, want) {
					t.Errorf("%s message = %q, want %q", fe.Field, fe.Msg, want)
				}
				delete(remaining, fe.Field)
			}
			if len(remaining) != 0 {
				t.Errorf("missing field errors for %v (got %v)", remaining, FieldErrors(err))
			}
		})
	}

	// Canonical must not rewrite the raw values it validated.
	cfg := Config{DecisionBrief: negatives}
	if _, err := cfg.Canonical(); err == nil {
		t.Fatal("Canonical accepted negative values")
	}
	if cfg.DecisionBrief != negatives {
		t.Errorf("Canonical changed raw values: %+v", cfg.DecisionBrief)
	}
}

// Upper bounds are normalized, not rejected. Prepare returns the effective
// clamped values; Canonical validates without rewriting the raw form.
func TestDecisionBriefOverCapNormalizes(t *testing.T) {
	const veryLarge = 1 << 28
	tests := []struct {
		name string
		in   DecisionBriefConfig
		want DecisionBriefConfig
	}{
		{
			name: "immediately above maximum",
			in: DecisionBriefConfig{
				MaxArticleBytes: MaxDecisionBriefMaxArticleBytes + 1,
				MaxTextBytes:    MaxDecisionBriefMaxTextBytes + 1,
				MaxOutputTokens: MaxDecisionBriefMaxOutputTokens + 1,
			},
			want: DecisionBriefConfig{
				MaxArticleBytes: MaxDecisionBriefMaxArticleBytes,
				MaxTextBytes:    MaxDecisionBriefMaxTextBytes,
				MaxOutputTokens: MaxDecisionBriefMaxOutputTokens,
			},
		},
		{
			name: "very large positive",
			in: DecisionBriefConfig{
				MaxArticleBytes: veryLarge,
				MaxTextBytes:    veryLarge,
				MaxOutputTokens: veryLarge,
			},
			want: DecisionBriefConfig{
				MaxArticleBytes: MaxDecisionBriefMaxArticleBytes,
				MaxTextBytes:    MaxDecisionBriefMaxTextBytes,
				MaxOutputTokens: MaxDecisionBriefMaxOutputTokens,
			},
		},
		{
			name: "output below documented floor",
			in:   DecisionBriefConfig{MaxOutputTokens: 1},
			want: DecisionBriefConfig{
				MaxArticleBytes: DefaultDecisionBriefMaxArticleBytes,
				MaxTextBytes:    DefaultDecisionBriefMaxTextBytes,
				MaxOutputTokens: MinDecisionBriefMaxOutputTokens,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{DecisionBrief: tc.in}
			if _, err := cfg.Prepare(); err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			if cfg.DecisionBrief != tc.want {
				t.Fatalf("effective config = %+v, want %+v", cfg.DecisionBrief, tc.want)
			}

			raw := Config{DecisionBrief: tc.in}
			if _, err := raw.Canonical(); err != nil {
				t.Fatalf("Canonical: %v", err)
			}
			if raw.DecisionBrief != tc.in {
				t.Fatalf("Canonical rewrote raw values: %+v", raw.DecisionBrief)
			}
		})
	}
}

func TestDecisionBriefValidBoundsAndDefaults(t *testing.T) {
	valid := []DecisionBriefConfig{
		{
			MaxArticleBytes: 1,
			MaxTextBytes:    MinDecisionBriefMaxTextBytes,
			MaxOutputTokens: MinDecisionBriefMaxOutputTokens,
		},
		{
			MaxArticleBytes: MaxDecisionBriefMaxArticleBytes,
			MaxTextBytes:    MaxDecisionBriefMaxTextBytes,
			MaxOutputTokens: MaxDecisionBriefMaxOutputTokens,
		},
	}
	for _, in := range valid {
		cfg := Config{DecisionBrief: in}
		if _, err := cfg.Prepare(); err != nil {
			t.Fatalf("Prepare(%+v): %v", in, err)
		}
		if cfg.DecisionBrief != in {
			t.Errorf("valid config changed: got %+v want %+v", cfg.DecisionBrief, in)
		}
	}

	var omitted Config
	if _, err := omitted.Prepare(); err != nil {
		t.Fatalf("Prepare omitted: %v", err)
	}
	if omitted.DecisionBrief != (DecisionBriefConfig{
		MaxArticleBytes: DefaultDecisionBriefMaxArticleBytes,
		MaxTextBytes:    DefaultDecisionBriefMaxTextBytes,
		MaxOutputTokens: DefaultDecisionBriefMaxOutputTokens,
	}) {
		t.Errorf("omitted defaults = %+v", omitted.DecisionBrief)
	}

	zero := Config{DecisionBrief: DecisionBriefConfig{
		MaxArticleBytes: 0, MaxTextBytes: 0, MaxOutputTokens: 0,
	}}
	if _, err := zero.Prepare(); err != nil {
		t.Fatalf("Prepare zero: %v", err)
	}
	if zero.DecisionBrief != omitted.DecisionBrief {
		t.Errorf("zero values = %+v, want defaults", zero.DecisionBrief)
	}
}

func TestDecisionBriefDropDefaults(t *testing.T) {
	r := Config{DecisionBrief: DecisionBriefConfig{
		MaxArticleBytes: DefaultDecisionBriefMaxArticleBytes,
		MaxTextBytes:    DefaultDecisionBriefMaxTextBytes,
		MaxOutputTokens: DefaultDecisionBriefMaxOutputTokens,
	}}
	dropped := r.DropDefaults()
	want := map[string]bool{
		"decision_brief.max_article_bytes": true,
		"decision_brief.max_text_bytes":    true,
		"decision_brief.max_output_tokens": true,
	}
	for _, name := range dropped {
		delete(want, name)
	}
	if len(want) != 0 {
		t.Fatalf("DropDefaults missed %v (dropped=%v)", want, dropped)
	}
	if r.DecisionBrief != (DecisionBriefConfig{}) {
		t.Fatalf("defaults were not cleared: %+v", r.DecisionBrief)
	}
	// A non-default value is kept.
	r2 := Config{DecisionBrief: DecisionBriefConfig{MaxOutputTokens: 512}}
	_ = r2.DropDefaults()
	if r2.DecisionBrief.MaxOutputTokens != 512 {
		t.Fatalf("non-default value dropped: %+v", r2.DecisionBrief)
	}
}

func TestDecisionBriefUnknownKeyIgnored(t *testing.T) {
	f, err := ParseFile([]byte("decision_brief:\n  unexpected: true\n"))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	run, _, _, _ := f.Split()
	if _, err := run.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
}

// Finding #5: a positive text limit below the extractor's usable minimum is
// rejected instead of being accepted and silently producing no brief.
func TestDecisionBriefTextMinimum(t *testing.T) {
	t.Run("tiny positive limit rejected", func(t *testing.T) {
		cfg := Config{DecisionBrief: DecisionBriefConfig{MaxTextBytes: 256}}
		_, err := cfg.Prepare()
		fe := FieldErrors(err)
		if len(fe) != 1 || fe[0].Field != "decision_brief.max_text_bytes" {
			t.Fatalf("errors = %v, want one decision_brief.max_text_bytes error", fe)
		}
		if !strings.Contains(fe[0].Msg, "at least") {
			t.Errorf("message = %q, want a lower-bound message", fe[0].Msg)
		}
	})
	t.Run("400-byte limit rejected", func(t *testing.T) {
		cfg := Config{DecisionBrief: DecisionBriefConfig{MaxTextBytes: 400}}
		_, err := cfg.Prepare()
		fe := FieldErrors(err)
		if len(fe) != 1 || fe[0].Field != "decision_brief.max_text_bytes" {
			t.Fatalf("errors = %v, want one decision_brief.max_text_bytes error", fe)
		}
	})
	t.Run("4x unicode floor accepted", func(t *testing.T) {
		cfg := Config{DecisionBrief: DecisionBriefConfig{MaxTextBytes: 1600}}
		if _, err := cfg.Prepare(); err != nil {
			t.Fatalf("Prepare: %v", err)
		}
	})
	t.Run("minimum accepted", func(t *testing.T) {
		cfg := Config{DecisionBrief: DecisionBriefConfig{MaxTextBytes: MinDecisionBriefMaxTextBytes}}
		if _, err := cfg.Prepare(); err != nil {
			t.Fatalf("Prepare: %v", err)
		}
	})
	t.Run("default accepted", func(t *testing.T) {
		var cfg Config
		if _, err := cfg.Prepare(); err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		if cfg.DecisionBrief.MaxTextBytes != DefaultDecisionBriefMaxTextBytes {
			t.Errorf("MaxTextBytes = %d, want default", cfg.DecisionBrief.MaxTextBytes)
		}
	})
	t.Run("zero uses default", func(t *testing.T) {
		cfg := Config{DecisionBrief: DecisionBriefConfig{MaxTextBytes: 0}}
		if _, err := cfg.Prepare(); err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		if cfg.DecisionBrief.MaxTextBytes != DefaultDecisionBriefMaxTextBytes {
			t.Errorf("MaxTextBytes = %d, want default", cfg.DecisionBrief.MaxTextBytes)
		}
	})
	t.Run("canonical rejects raw tiny limit without defaulting", func(t *testing.T) {
		cfg := Config{DecisionBrief: DecisionBriefConfig{MaxTextBytes: 256}}
		_, err := cfg.Canonical()
		fe := FieldErrors(err)
		if len(fe) != 1 || fe[0].Field != "decision_brief.max_text_bytes" {
			t.Fatalf("errors = %v, want one decision_brief.max_text_bytes error", fe)
		}
		if cfg.DecisionBrief.MaxTextBytes != 256 {
			t.Errorf("Canonical changed the raw value to %d", cfg.DecisionBrief.MaxTextBytes)
		}
	})
}

// The config minimum must stay aligned with the extractor's guard without a
// production import between the packages. max_text_bytes is a byte budget while
// the extractor's floor counts Unicode characters, so allow UTF-8's 4-byte
// maximum per character.
func TestDecisionBriefLimitsMatchExtractor(t *testing.T) {
	if MinDecisionBriefMaxTextBytes < 4*articleextract.MinContentChars {
		t.Errorf("MinDecisionBriefMaxTextBytes = %d, want at least 4*%d",
			MinDecisionBriefMaxTextBytes, articleextract.MinContentChars)
	}
	if MaxDecisionBriefMaxArticleBytes > articleextract.MaxInputBytes {
		t.Errorf("article byte cap %d exceeds extractor input limit %d",
			MaxDecisionBriefMaxArticleBytes, articleextract.MaxInputBytes)
	}
}

// Finding #5: files that were valid before remain valid.
func TestDecisionBriefExistingFileCompatible(t *testing.T) {
	f, err := ParseFile([]byte("decision_brief:\n" +
		"  max_article_bytes: 5242880\n" +
		"  max_text_bytes: 32768\n" +
		"  max_output_tokens: 1024\n"))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	run, _, _, _ := f.Split()
	if _, err := run.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
}
