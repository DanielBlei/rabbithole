// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/DanielBlei/rabbithole/internal/rank"
)

func TestParseDuration(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"14d", 14 * 24 * time.Hour, false},
		{"7d", 7 * 24 * time.Hour, false},
		{"168h", 168 * time.Hour, false},
		{"1h30m", 90 * time.Minute, false},
		{" 7d ", 7 * 24 * time.Hour, false},
		{"", 0, true},
		{"abc", 0, true},
		{"d", 0, true},   // missing number before 'd'
		{"14x", 0, true}, // unknown unit
	}
	for _, c := range cases {
		got, err := ParseDuration(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseDuration(%q): expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseDuration(%q): unexpected error %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseDuration(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// writeConfig writes a config file plus the minimal feeds.yaml beside it that
// Load now requires, and returns the config path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	return writeConfigWithFeeds(t, body, minimalFeeds)
}

// baseFeeds is the required-fields-only config the duration/think tests build on.
const baseFeeds = "profile: ./p.md\nstore:\n  db_path: ./test.db\n"

// minimalFeeds is the smallest valid feeds file.
const minimalFeeds = "feeds:\n  - name: x\n    url: http://x\n"

func TestLoadSinceDaysAndHours(t *testing.T) {
	for _, tc := range []struct {
		since string
		want  time.Duration
	}{
		{"ingest:\n  since: 14d\n", 14 * 24 * time.Hour},
		{"ingest:\n  since: 168h\n", 168 * time.Hour},
		{"", 7 * 24 * time.Hour}, // omitted -> default 7d
	} {
		cfg, err := Load(writeConfig(t, baseFeeds+tc.since))
		if err != nil {
			t.Fatalf("Load(since=%q): %v", tc.since, err)
		}
		if cfg.Ingest.Since.Std() != tc.want {
			t.Errorf("since=%q -> %s, want %s", tc.since, cfg.Ingest.Since, tc.want)
		}
	}
}

func TestLoadSinceInvalid(t *testing.T) {
	if _, err := Load(writeConfig(t, baseFeeds+"ingest:\n  since: 14x\n")); err == nil {
		t.Error("expected error for invalid since value")
	}
}

func TestLoadThinkDefaultsAndOverride(t *testing.T) {
	for _, tc := range []struct {
		think string
		want  bool
	}{
		{"", true},                              // omitted -> default true
		{"inference:\n  think: true\n", true},   // explicit on
		{"inference:\n  think: false\n", false}, // explicit off must not read as the default
	} {
		cfg, err := Load(writeConfig(t, baseFeeds+tc.think))
		if err != nil {
			t.Fatalf("Load(think=%q): %v", tc.think, err)
		}
		if cfg.Inference.Think == nil {
			t.Fatalf("think=%q -> nil, want non-nil after defaults", tc.think)
		}
		if *cfg.Inference.Think != tc.want {
			t.Errorf("think=%q -> %v, want %v", tc.think, *cfg.Inference.Think, tc.want)
		}
	}
}

func TestStripHTMLComments(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"no comment", "# Profile\n- AI", "# Profile\n- AI"},
		{"leading block", "<!--\nnotes\n-->\n\n# Profile\n- AI", "# Profile\n- AI"},
		{"inline", "- AI <!-- keep an eye on this --> and infra", "- AI  and infra"},
		{"multiple", "<!-- a -->x<!-- b -->", "x"},
		{"unterminated is left alone", "# Profile\n<!-- oops", "# Profile\n<!-- oops"},
	} {
		if got := stripHTMLComments(tc.in); got != tc.want {
			t.Errorf("%s: stripHTMLComments(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// The shipped template is mostly an HTML comment, so a profile that looks filled
// in can strip to nothing. Scoring every article against nothing is meaningless
// rather than degraded, and it used to happen without a word.
func TestLoadProfileRejectsAnEmptyProfile(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		wantErr bool
	}{
		{"real profile", "# Profile\n- Local LLMs", false},
		{"empty file", "", true},
		{"whitespace only", "\n\n   \n", true},
		{"comment only", "<!--\nDescribe what you want to read.\n-->\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "profile.md")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := (&Config{Profile: path}).LoadProfile()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("LoadProfile() = %q, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadProfile() error = %v", err)
			}
		})
	}
}

func TestSystemPromptSettingUnmarshalYAML(t *testing.T) {
	for _, tc := range []struct {
		name    string
		yaml    string
		want    SystemPromptSetting
		wantErr bool
	}{
		{"path", "system_prompt: ./configs/prompts/system.md", SystemPromptSetting{Path: "./configs/prompts/system.md"}, false},
		{"false disables", "system_prompt: false", SystemPromptSetting{Disabled: true}, false},
		{"true is rejected", "system_prompt: true", SystemPromptSetting{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cfg struct {
				SystemPrompt SystemPromptSetting `yaml:"system_prompt"`
			}
			err := yaml.Unmarshal([]byte(tc.yaml), &cfg)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Unmarshal(%q) = %+v, want an error", tc.yaml, cfg.SystemPrompt)
				}
				return
			}
			if err != nil {
				t.Fatalf("Unmarshal(%q) error = %v", tc.yaml, err)
			}
			if cfg.SystemPrompt != tc.want {
				t.Fatalf("Unmarshal(%q) = %+v, want %+v", tc.yaml, cfg.SystemPrompt, tc.want)
			}
		})
	}
}

func TestSystemPromptSettingLoadOverride(t *testing.T) {
	t.Run("unset path returns empty", func(t *testing.T) {
		got, err := (&SystemPromptSetting{}).LoadOverride()
		if err != nil {
			t.Fatalf("LoadOverride() error = %v", err)
		}
		if got != "" {
			t.Fatalf("LoadOverride() = %q, want empty", got)
		}
	})

	for _, tc := range []struct {
		name    string
		content string
		wantErr bool
		want    string
	}{
		{"real override", "Score everything a 10.", false, "Score everything a 10."},
		{"empty file", "", true, ""},
		{"comment only", "<!-- edit me -->\n", true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "system.md")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := (&SystemPromptSetting{Path: path}).LoadOverride()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("LoadOverride() = %q, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadOverride() error = %v", err)
			}
			if got != tc.want {
				t.Fatalf("LoadOverride() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestInferenceConfigLoadSystemPrompt(t *testing.T) {
	t.Run("heuristic provider ignores a broken override path", func(t *testing.T) {
		cfg := InferenceConfig{
			Provider:     "heuristic",
			SystemPrompt: SystemPromptSetting{Path: filepath.Join(t.TempDir(), "missing.md")},
		}
		got, err := cfg.LoadSystemPrompt()
		if err != nil {
			t.Fatalf("LoadSystemPrompt() error = %v, want nil (heuristic never reads the path)", err)
		}
		if got != "" {
			t.Fatalf("LoadSystemPrompt() = %q, want empty", got)
		}
	})

	t.Run("disabled returns empty", func(t *testing.T) {
		cfg := InferenceConfig{Provider: "ollama", SystemPrompt: SystemPromptSetting{Disabled: true}}
		got, err := cfg.LoadSystemPrompt()
		if err != nil {
			t.Fatalf("LoadSystemPrompt() error = %v", err)
		}
		if got != "" {
			t.Fatalf("LoadSystemPrompt() = %q, want empty", got)
		}
	})

	t.Run("unset returns the built-in default", func(t *testing.T) {
		cfg := InferenceConfig{Provider: "ollama"}
		got, err := cfg.LoadSystemPrompt()
		if err != nil {
			t.Fatalf("LoadSystemPrompt() error = %v", err)
		}
		if got != rank.DefaultSystemPrompt {
			t.Fatalf("LoadSystemPrompt() = %q, want the built-in default", got)
		}
	})

	t.Run("path overrides the default", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "system.md")
		if err := os.WriteFile(path, []byte("Score everything a 10."), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := InferenceConfig{Provider: "ollama", SystemPrompt: SystemPromptSetting{Path: path}}
		got, err := cfg.LoadSystemPrompt()
		if err != nil {
			t.Fatalf("LoadSystemPrompt() error = %v", err)
		}
		if got != "Score everything a 10." {
			t.Fatalf("LoadSystemPrompt() = %q, want the override", got)
		}
	})
}
