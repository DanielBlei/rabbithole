// Package config loads the YAML run configuration and the interest profile.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
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

// Feed is a single RSS/Atom source.
type Feed struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
}

// Config is the full run configuration loaded from YAML.
type Config struct {
	User        string   `yaml:"user"`         // shell-prompt name on the web UI; blank falls back to the OS user
	Profile     string   `yaml:"profile"`      // path to the interest-profile markdown
	Provider    string   `yaml:"provider"`     // ollama | vllm | heuristic
	ChatHost    string   `yaml:"chat_host"`    // inference server URL
	ChatModel   string   `yaml:"chat_model"`   // chat model name
	APIKey      string   `yaml:"api_key"`      // optional bearer token
	BatchSize   int      `yaml:"batch_size"`   // items per scoring request
	MaxParallel int      `yaml:"max_parallel"` // concurrent scoring requests in flight
	TopN        int      `yaml:"top_n"`        // max items in a digest
	MinScore    int      `yaml:"min_score"`    // inclusion threshold (0-10)
	Since       Duration `yaml:"since"`        // lookback window (e.g. 14d, 168h)
	ListSince   Duration `yaml:"list_since"`   // default display window for `items list`/serve (e.g. 3d)
	OutputDir   string   `yaml:"output_dir"`   // digest output directory
	DBPath      string   `yaml:"db_path"`      // sqlite database path
	Feeds       []Feed   `yaml:"feeds"`
}

// Defaults mirror go-to-rag conventions (Ollama on localhost).
const (
	defaultProvider    = "ollama"
	defaultChatHost    = "http://localhost:11434"
	defaultChatModel   = "qwen3:4b"
	defaultBatchSize   = 5
	defaultMaxParallel = 4
	defaultTopN        = 30
	defaultMinScore    = 6
	defaultSince       = 14 * 24 * time.Hour
	defaultListSince   = 3 * 24 * time.Hour
	defaultOutputDir   = "./data/digests"
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
	if c.Provider == "" {
		c.Provider = defaultProvider
	}
	if c.ChatHost == "" {
		c.ChatHost = defaultChatHost
	}
	if c.ChatModel == "" {
		c.ChatModel = defaultChatModel
	}
	if c.BatchSize <= 0 {
		c.BatchSize = defaultBatchSize
	}
	if c.MaxParallel <= 0 {
		c.MaxParallel = defaultMaxParallel
	}
	if c.TopN <= 0 {
		c.TopN = defaultTopN
	}
	if c.MinScore == 0 {
		c.MinScore = defaultMinScore
	}
	if c.Since == 0 {
		c.Since = Duration(defaultSince)
	}
	if c.ListSince == 0 {
		c.ListSince = Duration(defaultListSince)
	}
	if c.OutputDir == "" {
		c.OutputDir = defaultOutputDir
	}
}

func (c *Config) validate() error {
	switch c.Provider {
	case "ollama", "vllm", "heuristic":
	default:
		return fmt.Errorf("invalid provider %q, must be ollama, vllm or heuristic", c.Provider)
	}
	if c.Profile == "" {
		return fmt.Errorf("profile path is required")
	}
	if c.DBPath == "" {
		return fmt.Errorf("db_path is required")
	}
	if len(c.Feeds) == 0 {
		return fmt.Errorf("at least one feed is required")
	}
	for i, f := range c.Feeds {
		if f.URL == "" {
			return fmt.Errorf("feed %d (%q) has no url", i, f.Name)
		}
	}
	if c.Since < 0 {
		return fmt.Errorf("since must be positive, got %s", c.Since)
	}
	return nil
}

// LoadProfile reads the interest-profile markdown referenced by the config.
func (c *Config) LoadProfile() (string, error) {
	raw, err := os.ReadFile(c.Profile)
	if err != nil {
		return "", fmt.Errorf("read profile %q: %w", c.Profile, err)
	}
	return string(raw), nil
}
