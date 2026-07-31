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

// Config is the full run configuration loaded from YAML. Feeds live in their
// own file (see feeds.go) — the fields at the bottom are how the two connect.
type Config struct {
	User      string          `yaml:"user"`    // shell-prompt name on the web UI; blank falls back to the OS user
	Profile   string          `yaml:"profile"` // path to the interest-profile markdown
	Inference InferenceConfig `yaml:"inference"`
	Ingest    IngestConfig    `yaml:"ingest"`
	Store     StoreConfig     `yaml:"store"`

	// Feeds is the resolved feed set: the feeds themselves plus the defaults
	// and path they came from. Derived at load, never written in this file —
	// hence a single field rather than declared and computed state mixed
	// together at the top level.
	Feeds FeedSet `yaml:"-"`
}

// InferenceConfig configures the scoring backend (the model and how to reach it).
type InferenceConfig struct {
	Provider string `yaml:"provider"` // ollama | vllm | heuristic
	Host     string `yaml:"host"`     // inference server URL
	Model    string `yaml:"model"`    // chat model name
	APIKey   string `yaml:"api_key"`  // optional bearer token
	Think    *bool  `yaml:"think"`    // model reasoning during scoring; defaults true when not set in the config

	// How items are batched through the model. BatchSize also sizes the token
	// budget: ModelTuning.Budget multiplies TokensPerItem by it.
	BatchSize   int `yaml:"batch_size"`   // items per scoring request
	MaxParallel int `yaml:"max_parallel"` // concurrent scoring requests in flight

	// ModelTuning carries the decoding limits.
	// Omit the block, or any field in it, to take rank's defaults.
	ModelTuning rank.ModelTuning `yaml:"model_tuning"`
}

// IngestConfig governs the fetch/score run.
type IngestConfig struct {
	Since     Duration `yaml:"since"`      // lookback window (e.g. 14d, 168h)
	DigestDir string   `yaml:"digest_dir"` // optional: where `ingest --markdown` writes the digest; no default
	Feeds     string   `yaml:"feeds"`      // path to the feeds file; empty looks for feeds.yaml beside the config
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
	defaultModel       = "qwen3:4b"
	defaultBatchSize   = 5
	defaultMaxParallel = 2
	defaultSince       = 14 * 24 * time.Hour
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
	// Feeds resolve last: the cascade's outermost fallback is ingest.since,
	// so the defaults above must already be applied.
	if err := cfg.loadFeeds(path); err != nil {
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

// LoadProfile reads the interest-profile markdown referenced by the config.
func (c *Config) LoadProfile() (string, error) {
	raw, err := os.ReadFile(c.Profile)
	if err != nil {
		return "", fmt.Errorf("read profile %q: %w", c.Profile, err)
	}
	return stripHTMLComments(string(raw)), nil
}
