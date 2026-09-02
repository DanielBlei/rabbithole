// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

// Package config loads the YAML run configuration and the interest profile.
package config

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/DanielBlei/rabbithole/internal/rank"
)

// Duration is a time.Duration that unmarshals from YAML strings, additionally
// accepting a "d" (days) suffix on top of Go's standard units, e.g. "14d",
// "7d", "168h", "36h", "1h30m". The standard library has no day unit.
type Duration time.Duration

// UnmarshalYAML parses a duration string such as "14d" or "168h".
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("since must be a duration string like 14d or 168h: %w", err)
	}
	parsed, err := ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

// Std returns the value as a standard time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// String renders the underlying time.Duration.
func (d Duration) String() string { return time.Duration(d).String() }

// Short renders the duration the way it is written in config — "7d", "36h",
// "90m" — rather than Go's "168h0m0s". This is the form the store persists and
// the export emits, so a window survives the round trip looking like what was
// typed instead of expanding into hours.
func (d Duration) Short() string {
	std := time.Duration(d)
	switch {
	case std == 0:
		return "0s"
	case std%(24*time.Hour) == 0:
		return strconv.Itoa(int(std/(24*time.Hour))) + "d"
	case std%time.Hour == 0:
		return strconv.Itoa(int(std/time.Hour)) + "h"
	case std%time.Minute == 0:
		return strconv.Itoa(int(std/time.Minute)) + "m"
	default:
		return std.String()
	}
}

// MarshalYAML writes the duration back as the short string it was parsed from.
// Without this a Duration marshals as its raw nanosecond count, which the
// exported feed set would not be able to read back in.
func (d Duration) MarshalYAML() (any, error) { return d.Short(), nil }

// ParseDuration parses durations with an optional "d" (days) suffix, falling
// back to time.ParseDuration for all standard units (h, m, s, …).
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return 0, fmt.Errorf("invalid day duration %q: want an integer before 'd' (e.g. 14d)", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	dur, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: use a 'd' or 'h' suffix (e.g. 14d, 168h)", s)
	}
	return dur, nil
}

// Config is the full run configuration loaded from YAML. Feeds are not in here:
// they live in the store, and ingest.feeds names only the file new ones are
// seeded from (see feeds.go).
type Config struct {
	User      string          `yaml:"user"`    // shell-prompt name on the web UI; blank falls back to the OS user
	Profile   string          `yaml:"profile"` // path to the interest-profile markdown
	Inference InferenceConfig `yaml:"inference"`
	Ingest    IngestConfig    `yaml:"ingest"`
	Store     StoreConfig     `yaml:"store"`
}

// InferenceConfig configures the scoring backend (the model and how to reach it).
type InferenceConfig struct {
	Provider string `yaml:"provider"` // ollama | vllm | heuristic
	Host     string `yaml:"host"`     // inference server URL
	Model    string `yaml:"model"`    // chat model name
	APIKey   string `yaml:"api_key"`  // optional bearer token
	Think    *bool  `yaml:"think"`    // model reasoning during scoring; defaults true when not set in the config

	// SystemPrompt: unset takes inference's built-in default, a path overrides it, false sends none.
	SystemPrompt SystemPromptSetting `yaml:"system_prompt"`

	// How items are batched through the model. BatchSize also sizes the token
	// budget: ModelTuning.Budget multiplies TokensPerItem by it.
	BatchSize   int `yaml:"batch_size"`   // items per scoring request
	MaxParallel int `yaml:"max_parallel"` // concurrent scoring requests in flight

	// ModelTuning carries the decoding limits.
	// Omit the block, or any field in it, to take rank's defaults.
	ModelTuning rank.ModelTuning `yaml:"model_tuning"`
}

// SystemPromptSetting is the inference.system_prompt config value: a path, false (disabled),
// or unset (the built-in default).
type SystemPromptSetting struct {
	Path     string
	Disabled bool
}

// UnmarshalYAML accepts a path string or the literal false; true is rejected.
func (s *SystemPromptSetting) UnmarshalYAML(value *yaml.Node) error {
	if value.Tag == "!!bool" {
		var b bool
		if err := value.Decode(&b); err != nil {
			return err
		}
		if b {
			return fmt.Errorf("system_prompt: true has no meaning; use a path, false, or omit it")
		}
		s.Disabled = true
		return nil
	}
	return value.Decode(&s.Path)
}

// IngestConfig governs the fetch/score run.
type IngestConfig struct {
	Since     Duration `yaml:"since"`      // lookback window (e.g. 14d, 168h)
	DigestDir string   `yaml:"digest_dir"` // optional: where `ingest --markdown` writes the digest; no default
	Feeds     string   `yaml:"feeds"`      // path to the feed seed file; empty looks for feeds.yaml beside the config
}

// StoreConfig configures item persistence. Local sqlite for now; host/credentials
// can join here if a remote store is added.
type StoreConfig struct {
	DBPath string `yaml:"db_path"` // sqlite database path
}

// Defaults (Ollama on localhost).
const (
	defaultProvider    = "ollama"
	defaultHost        = "http://localhost:11434"
	defaultModel       = "qwen3.5:4b"
	defaultBatchSize   = 1
	defaultMaxParallel = 1
	defaultSince       = 7 * 24 * time.Hour
)

// Load reads and validates the config at path, applying defaults for unset fields.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Inference.Provider == "" {
		c.Inference.Provider = defaultProvider
	}
	if c.Inference.Host == "" {
		c.Inference.Host = defaultHost
	}
	if c.Inference.Model == "" {
		c.Inference.Model = defaultModel
	}
	if c.Inference.Think == nil {
		t := true
		c.Inference.Think = &t
	}
	if c.Inference.BatchSize <= 0 {
		c.Inference.BatchSize = defaultBatchSize
	}
	if c.Inference.MaxParallel <= 0 {
		c.Inference.MaxParallel = defaultMaxParallel
	}
	if c.Ingest.Since == 0 {
		c.Ingest.Since = Duration(defaultSince)
	}
}

func (c *Config) validate() error {
	// "claude" is deliberately absent: it is an eval-only reference scorer,
	// reachable through `eval benchmark --provider claude` and nowhere else.
	switch c.Inference.Provider {
	case "ollama", "vllm", "heuristic":
	default:
		return fmt.Errorf("invalid provider %q, must be ollama, vllm or heuristic", c.Inference.Provider)
	}
	if c.Profile == "" {
		return fmt.Errorf("profile path is required")
	}
	if c.Store.DBPath == "" {
		return fmt.Errorf("store.db_path is required")
	}
	if c.Ingest.Since < 0 {
		return fmt.Errorf("since must be positive, got %s", c.Ingest.Since)
	}
	return nil
}

var htmlComment = regexp.MustCompile(`(?s)<!--.*?-->`)

// stripHTMLComments drops <!-- --> blocks, so comments never reach the model
func stripHTMLComments(md string) string {
	return strings.TrimSpace(htmlComment.ReplaceAllString(md, ""))
}

// loadTrimmedFile reads path and strips HTML comments, so notes to yourself
// (in a profile or a system prompt override) never reach the model.
func loadTrimmedFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return stripHTMLComments(string(raw)), nil
}

// LoadProfile reads the interest-profile markdown referenced by the config.
func (c *Config) LoadProfile() (string, error) {
	profile, err := loadTrimmedFile(c.Profile)
	if err != nil {
		return "", fmt.Errorf("read profile %q: %w", c.Profile, err)
	}
	// The shipped template is mostly an HTML comment, so a file that looks
	// filled in can strip to nothing. Without it every score is arbitrary while
	// the run still looks like it worked.
	if strings.TrimSpace(profile) == "" {
		return "", fmt.Errorf(
			"profile %q is empty; it is required in order to make meaningful suggestions",
			c.Profile,
		)
	}
	return profile, nil
}

// LoadOverride reads Path, if set, stripping HTML comments like LoadProfile. Unset returns
// ("", nil); the caller decides what that means.
func (s *SystemPromptSetting) LoadOverride() (string, error) {
	if s.Path == "" {
		return "", nil
	}
	prompt, err := loadTrimmedFile(s.Path)
	if err != nil {
		return "", fmt.Errorf("read system prompt %q: %w", s.Path, err)
	}
	if strings.TrimSpace(prompt) == "" {
		return "", fmt.Errorf("system prompt %q is empty", s.Path)
	}
	return prompt, nil
}

// LoadSystemPrompt resolves the effective inference.system_prompt: disabled returns "", a path
// returns that file's contents, and unset returns the built-in default. The heuristic provider
// never looks at a system prompt, so it always gets "" without touching SystemPrompt at all —
// a bad or missing override path must not block a heuristic run.
func (c *InferenceConfig) LoadSystemPrompt() (string, error) {
	if c.Provider == "heuristic" {
		return "", nil
	}
	if c.SystemPrompt.Disabled {
		return "", nil
	}
	if c.SystemPrompt.Path != "" {
		return c.SystemPrompt.LoadOverride()
	}
	return rank.DefaultSystemPrompt, nil
}
