// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	profiledomain "github.com/DanielBlei/rabbithole/internal/profile"
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
const baseWithoutProfile = "store:\n  db_path: ./test.db\n"

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

func TestLoadAllowsNoConfiguredProfile(t *testing.T) {
	cfg, err := Load(writeConfig(t, baseWithoutProfile))
	if err != nil {
		t.Fatalf("Load without profile: %v", err)
	}
	if cfg.Profile != "" {
		t.Fatalf("Profile = %q, want empty", cfg.Profile)
	}
	got, err := cfg.LoadProfile()
	if err != nil {
		t.Fatalf("LoadProfile without path: %v", err)
	}
	if got != profiledomain.DefaultContent {
		t.Fatalf("LoadProfile without path did not return built-in Default")
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

func TestLoadProfileCompatibilityPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.md")
	if err := os.WriteFile(path, []byte("<!-- private -->\n# Legacy\n- local models"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := (&Config{Profile: path}).LoadProfile()
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if got != "# Legacy\n- local models" {
		t.Fatalf("LoadProfile = %q", got)
	}

	missing := filepath.Join(dir, "missing.md")
	if _, err := (&Config{Profile: missing}).LoadProfile(); err == nil ||
		!strings.Contains(err.Error(), "read profile") {
		t.Fatalf("missing LoadProfile error = %v", err)
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

func TestLoadSummaryRequiresModel(t *testing.T) {
	t.Run("a field set without model is rejected", func(t *testing.T) {
		_, err := Load(writeConfig(t, baseFeeds+"inference:\n  summary:\n    host: http://other:8000\n"))
		if err == nil {
			t.Fatal("expected error for summary block missing model")
		}
	})

	t.Run("model alone is enough", func(t *testing.T) {
		cfg, err := Load(writeConfig(t, baseFeeds+"inference:\n  summary:\n    model: small:1b\n"))
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if cfg.Inference.Summary.Model != "small:1b" {
			t.Errorf("Summary.Model = %q, want small:1b", cfg.Inference.Summary.Model)
		}
	})

	t.Run("unset summary is fine", func(t *testing.T) {
		if _, err := Load(writeConfig(t, baseFeeds)); err != nil {
			t.Fatalf("Load() error = %v", err)
		}
	})
}

func TestResolveSummary(t *testing.T) {
	think := true
	base := InferenceConfig{
		Provider: "ollama",
		Host:     "http://localhost:11434",
		Model:    "big:8b",
		APIKey:   "base-key",
		Think:    &think,
	}

	t.Run("unset summary resolves to the parent unchanged", func(t *testing.T) {
		got := base.ResolveSummary()
		if got != base {
			t.Errorf("ResolveSummary() = %+v, want %+v", got, base)
		}
	})

	t.Run("model alone overrides only Model", func(t *testing.T) {
		cfg := base
		cfg.Summary = SummaryConfig{Model: "small:1b"}
		got := cfg.ResolveSummary()
		if got.Model != "small:1b" {
			t.Errorf("Model = %q, want small:1b", got.Model)
		}
		if got.Provider != base.Provider || got.Host != base.Host || got.APIKey != base.APIKey ||
			got.Think != base.Think {
			t.Errorf("ResolveSummary() changed an unset field: %+v", got)
		}
	})

	t.Run("host and model override both, provider/api_key/think stay inherited", func(t *testing.T) {
		cfg := base
		cfg.Summary = SummaryConfig{Host: "http://other:8000", Model: "small:1b"}
		got := cfg.ResolveSummary()
		if got.Host != "http://other:8000" || got.Model != "small:1b" {
			t.Errorf("ResolveSummary() = %+v, want overridden host/model", got)
		}
		if got.Provider != base.Provider || got.APIKey != base.APIKey || got.Think != base.Think {
			t.Errorf("ResolveSummary() changed an unset field: %+v", got)
		}
	})

	t.Run("full override replaces every field", func(t *testing.T) {
		otherThink := false
		cfg := base
		cfg.Summary = SummaryConfig{
			Provider: "vllm",
			Host:     "http://other:8000",
			Model:    "small:1b",
			APIKey:   "other-key",
			Think:    &otherThink,
		}
		got := cfg.ResolveSummary()
		want := InferenceConfig{
			Provider: "vllm",
			Host:     "http://other:8000",
			Model:    "small:1b",
			APIKey:   "other-key",
			Think:    &otherThink,
		}
		if got.Provider != want.Provider || got.Host != want.Host || got.Model != want.Model ||
			got.APIKey != want.APIKey || got.Think != want.Think {
			t.Errorf("ResolveSummary() = %+v, want %+v", got, want)
		}
	})
}

// The engine is chosen by which key is set, so the pair has to be exclusive:
// neither leaves nothing to open, both leaves two answers and no tiebreak.
func TestValidateStoreNeedsExactlyOneTarget(t *testing.T) {
	cases := []struct {
		name    string
		store   StoreConfig
		wantErr bool
	}{
		{"sqlite only", StoreConfig{DBPath: "./data/rabbithole.db"}, false},
		{"postgres only", StoreConfig{URL: "postgres://u@h/db"}, false},
		{"neither", StoreConfig{}, true},
		{"both", StoreConfig{DBPath: "./x.db", URL: "postgres://u@h/db"}, true},
		{"url not a postgres one", StoreConfig{URL: "mysql://u@h/db"}, true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			c := Config{Profile: "p", Inference: InferenceConfig{Provider: "heuristic"}, Store: tt.store}
			if err := c.validate(); (err != nil) != tt.wantErr {
				t.Errorf("validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestResolvePostgres(t *testing.T) {
	t.Run("the environment beats a password in the url", func(t *testing.T) {
		t.Setenv(DBPasswordEnv, "from-env")
		pg, err := StoreConfig{URL: "postgres://rabbit:from-url@db.host:5432/rabbithole"}.ResolvePostgres()
		if err != nil {
			t.Fatalf("ResolvePostgres: %v", err)
		}
		if !strings.Contains(pg.DSN, "from-env") || strings.Contains(pg.DSN, "from-url") {
			t.Errorf("DSN = %q, want the environment's password", pg.DSN)
		}
		// Overriding the value does not remove the secret from the file, which is
		// the thing the caller warns about.
		if !pg.PasswordInConfigFile {
			t.Error("PasswordInConfigFile = false, want true; store.url still holds one")
		}
	})

	t.Run("a password in the url is used and flagged", func(t *testing.T) {
		pg, err := StoreConfig{URL: "postgres://rabbit:from-url@db.host/rabbithole"}.ResolvePostgres()
		if err != nil {
			t.Fatalf("ResolvePostgres: %v", err)
		}
		if !strings.Contains(pg.DSN, "from-url") {
			t.Errorf("DSN = %q, want the url's password", pg.DSN)
		}
		if !pg.PasswordInConfigFile {
			t.Error("PasswordInConfigFile = false, want true so the caller can warn")
		}
	})

	t.Run("the label never carries the password", func(t *testing.T) {
		t.Setenv(DBPasswordEnv, "hunter2")
		pg, err := StoreConfig{URL: "postgres://rabbit@db.host:5432/rabbithole"}.ResolvePostgres()
		if err != nil {
			t.Fatalf("ResolvePostgres: %v", err)
		}
		if strings.Contains(pg.Label, "hunter2") {
			t.Errorf("Label = %q, must not carry the password", pg.Label)
		}
		if !strings.Contains(pg.Label, "rabbit@db.host") {
			t.Errorf("Label = %q, want the user and host kept", pg.Label)
		}
	})

	t.Run("punctuation in a password survives", func(t *testing.T) {
		const nasty = "p@ss/w:rd?#x"
		t.Setenv(DBPasswordEnv, nasty)
		pg, err := StoreConfig{URL: "postgres://rabbit@db.host/rabbithole"}.ResolvePostgres()
		if err != nil {
			t.Fatalf("ResolvePostgres: %v", err)
		}
		u, err := url.Parse(pg.DSN)
		if err != nil {
			t.Fatalf("the DSN did not survive escaping: %v", err)
		}
		if got, _ := u.User.Password(); got != nasty {
			t.Errorf("password round-tripped as %q, want %q", got, nasty)
		}
		if u.Host != "db.host" {
			t.Errorf("host = %q, want db.host; the password leaked into it", u.Host)
		}
	})

	t.Run("sslmode defaults to require but is never overridden", func(t *testing.T) {
		pg, err := StoreConfig{URL: "postgres://rabbit@db.host/rabbithole"}.ResolvePostgres()
		if err != nil {
			t.Fatalf("ResolvePostgres: %v", err)
		}
		if !strings.Contains(pg.DSN, "sslmode=require") {
			t.Errorf("DSN = %q, want sslmode=require applied", pg.DSN)
		}
		pg, err = StoreConfig{URL: "postgres://rabbit@db.host/rabbithole?sslmode=disable"}.ResolvePostgres()
		if err != nil {
			t.Fatalf("ResolvePostgres: %v", err)
		}
		if !strings.Contains(pg.DSN, "sslmode=disable") {
			t.Errorf("DSN = %q, want the explicit sslmode kept", pg.DSN)
		}
	})
}

// The default leaves the server unverified, so the caller has to be able to say
// so — and has to know when it would only be noise: over loopback nobody but
// this machine can hear the connection at all.
func TestResolvePostgresTLSFlags(t *testing.T) {
	cases := []struct {
		url       string
		mode      string
		loopback  bool
		verified  bool
		encrypted bool
	}{
		// The default, on a hosted database: encrypted, unverified, not loopback.
		{"postgres://rabbit@db.example.com/rabbithole", "require", false, false, true},
		// The hardening the warning asks for.
		{"postgres://rabbit@db.example.com/rabbithole?sslmode=verify-full", "verify-full", false, true, true},
		{"postgres://rabbit@db.example.com/rabbithole?sslmode=verify-ca", "verify-ca", false, true, true},
		// Plain text, which is a different warning than an unverified one.
		{"postgres://rabbit@db.example.com/rabbithole?sslmode=disable", "disable", false, false, false},
		// `prefer` asks for TLS and falls back the moment the server declines, so
		// it cannot be reported as encrypted: the operator has to hear that.
		{"postgres://rabbit@db.example.com/rabbithole?sslmode=prefer", "prefer", false, false, false},
		{"postgres://rabbit@db.example.com/rabbithole?sslmode=allow", "allow", false, false, false},
		// Local development: same weak modes, nothing to warn about.
		{"postgres://rabbit@127.0.0.1:5433/rabbithole?sslmode=disable", "disable", true, false, false},
		{"postgres://rabbit@localhost/rabbithole", "require", true, false, true},
		{"postgres://rabbit@[::1]/rabbithole", "require", true, false, true},
	}
	for _, tc := range cases {
		pg, err := StoreConfig{URL: tc.url}.ResolvePostgres()
		if err != nil {
			t.Fatalf("%s: ResolvePostgres: %v", tc.url, err)
		}
		if pg.SSLMode != tc.mode || pg.Loopback != tc.loopback ||
			pg.TLSVerified() != tc.verified || pg.TLSEncrypted() != tc.encrypted {
			t.Errorf("%s: got mode=%q loopback=%v verified=%v encrypted=%v, "+
				"want mode=%q loopback=%v verified=%v encrypted=%v",
				tc.url, pg.SSLMode, pg.Loopback, pg.TLSVerified(), pg.TLSEncrypted(),
				tc.mode, tc.loopback, tc.verified, tc.encrypted)
		}
	}
}

// The drivers accept the password as a query parameter as well as in the
// userinfo. It has to be treated the same either way, or it slips past both
// the environment override and the warning the caller prints.
func TestResolvePostgresPasswordAsQueryParameter(t *testing.T) {
	t.Run("used and flagged", func(t *testing.T) {
		pg, err := StoreConfig{URL: "postgres://rabbit@db.host/rabbithole?password=hunter2"}.ResolvePostgres()
		if err != nil {
			t.Fatalf("ResolvePostgres: %v", err)
		}
		if !pg.PasswordInConfigFile {
			t.Error("PasswordInConfigFile = false, want true so the caller warns")
		}
		u, err := url.Parse(pg.DSN)
		if err != nil {
			t.Fatalf("parse DSN: %v", err)
		}
		if got, _ := u.User.Password(); got != "hunter2" {
			t.Errorf("password = %q, want it lifted into the userinfo", got)
		}
		if u.Query().Get("password") != "" {
			t.Error("the password is still in the query string; it should have moved")
		}
		if strings.Contains(pg.Label, "hunter2") {
			t.Errorf("Label = %q, must not carry the password", pg.Label)
		}
	})

	t.Run("the environment still wins", func(t *testing.T) {
		t.Setenv(DBPasswordEnv, "from-env")
		pg, err := StoreConfig{URL: "postgres://rabbit@db.host/rabbithole?password=from-url"}.ResolvePostgres()
		if err != nil {
			t.Fatalf("ResolvePostgres: %v", err)
		}
		if strings.Contains(pg.DSN, "from-url") {
			t.Errorf("DSN = %q, want the environment's password", pg.DSN)
		}
		if !pg.PasswordInConfigFile {
			t.Error("PasswordInConfigFile = false, want true; the file still holds a secret")
		}
	})
}

// A mistyped DSN is the first thing a new install gets wrong, and the failure is
// printed to a terminal and a journal. url.Error quotes the whole url it failed
// on, so the password has to come out before the message goes anywhere.
func TestResolvePostgresRefusesToEchoThePassword(t *testing.T) {
	for _, raw := range []string{
		"postgres://rabbit:***@db.host:notaport/rabbithole",
		"postgres://rabbit:***@%zz/db",
		"http://rabbit:***@db.host/rabbithole",
	} {
		_, err := StoreConfig{URL: raw}.ResolvePostgres()
		if err == nil {
			t.Fatalf("%q: want an error", raw)
		}
		if strings.Contains(err.Error(), "***") {
			t.Errorf("%q: error leaks the password: %v", raw, err)
		}
		if !strings.Contains(err.Error(), "store.url") {
			t.Errorf("%q: error should name the setting: %v", raw, err)
		}
	}
}

// The pool knobs exist because a hosted database caps connections and nothing
// else lets you fit inside the cap; they shape a Postgres pool and nothing
// else, so setting them beside a sqlite path is a mistake worth naming.
func TestValidateStorePoolKnobs(t *testing.T) {
	base := func() Config {
		return Config{
			Profile:   "p",
			Inference: InferenceConfig{Provider: "heuristic"},
			Store:     StoreConfig{URL: "postgres://rabbit@db.host/rabbithole"},
		}
	}

	ok := base()
	ok.Store.MaxConns = 4
	ok.Store.ConnMaxLifetime = Duration(time.Minute)
	if err := ok.validate(); err != nil {
		t.Errorf("tuned pool rejected: %v", err)
	}

	negative := base()
	negative.Store.MaxConns = -1
	if err := negative.validate(); err == nil {
		t.Error("a negative max_conns was accepted")
	}

	sqlite := Config{
		Profile:   "p",
		Inference: InferenceConfig{Provider: "heuristic"},
		Store:     StoreConfig{DBPath: "./x.db", MaxConns: 4},
	}
	if err := sqlite.validate(); err == nil {
		t.Error("max_conns beside db_path was accepted as if it did something")
	}
}
