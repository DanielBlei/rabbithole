// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// RunSchemaVersion is the schema version of a stored Config.
const RunSchemaVersion = 1

// File is the import shape: the whole YAML file as written, including the keys
// that are not stored (store, profile, ingest.feeds). It is decoded leniently,
// so unknown keys are ignored.
type File struct {
	User      string          `yaml:"user"`
	Profile   string          `yaml:"profile"`
	Inference InferenceConfig `yaml:"inference"`
	Ingest    FileIngest      `yaml:"ingest"`
	Store     StoreConfig     `yaml:"store"`
}

// Config is the stored shape: what the config store keeps and what ingest
// and the web UI read. The store block, profile and ingest.feeds are import
// directives and live elsewhere.
type Config struct {
	User      string          `yaml:"user,omitempty"` // shell-prompt name on the web UI; blank falls back to the OS user
	Inference InferenceConfig `yaml:"inference,omitempty"`
	Ingest    IngestConfig    `yaml:"ingest,omitempty"`
}

// IngestConfig is the stored part of the ingest block.
type IngestConfig struct {
	Since     Duration `yaml:"since,omitempty"`      // lookback window (e.g. 14d, 168h)
	DigestDir string   `yaml:"digest_dir,omitempty"` // optional: where `ingest --markdown` writes the digest
}

// Legacy holds the file paths named by an imported old-shape file.
type Legacy struct {
	ProfilePath string // profile, as written
	FeedsPath   string // ingest.feeds, as written
}

// ParseFile decodes a config file leniently.
func ParseFile(raw []byte) (File, error) {
	var f File
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return File{}, fmt.Errorf("parse config: %w", err)
	}
	return f, nil
}

// ReadFile reads and decodes the config file at path.
func ReadFile(path string) (File, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return File{}, fmt.Errorf("read config: %w", err)
	}
	f, err := ParseFile(raw)
	if err != nil {
		return File{}, err
	}
	return f, nil
}

// Split separates the file into the stored run config, the bootstrap store
// block and the legacy import paths. The run config is normalized but neither
// defaulted nor validated (see Prepare); dropped lists the dotted keys
// normalization removed.
func (f File) Split() (run Config, store StoreConfig, legacy Legacy, dropped []string) {
	run = Config{
		User:      f.User,
		Inference: f.Inference,
		Ingest:    IngestConfig{Since: f.Ingest.Since, DigestDir: f.Ingest.DigestDir},
	}
	dropped = run.Normalize()
	legacy.ProfilePath = f.Profile
	legacy.FeedsPath = f.Ingest.Feeds
	return run, f.Store, legacy, dropped
}

// RunDefaults returns the configuration a store with no saved version uses.
func RunDefaults() Config {
	var r Config
	_, _ = r.Prepare()
	return r
}

// Clone returns a copy that shares no pointers with r, so a caller can adjust
// it for one run without touching a cached original.
func (r Config) Clone() Config {
	c := r
	c.Inference.Think = cloneBool(r.Inference.Think)
	c.Inference.StructuredOutput = cloneBool(r.Inference.StructuredOutput)
	c.Inference.Summary.Think = cloneBool(r.Inference.Summary.Think)
	return c
}

func cloneBool(b *bool) *bool {
	if b == nil {
		return nil
	}
	v := *b
	return &v
}

// Prepare normalizes, applies defaults and validates, in that order. Normalizing
// comes first because defaults would otherwise fill fields the provider ignores.
// dropped lists the non-applicable keys that were set and removed.
func (r *Config) Prepare() (dropped []string, err error) {
	dropped = r.Normalize()
	r.applyDefaults()
	return dropped, r.validate()
}

// Canonical is the form that is stored, marshaled and hashed: normalized but
// not defaulted, so it holds only what the user set and a later release's new
// defaults still reach them. It validates a defaulted copy, so the errors match
// Prepare's.
func (r *Config) Canonical() (dropped []string, err error) {
	dropped = r.Normalize()
	eff := *r
	eff.applyDefaults()
	return dropped, eff.validate()
}

// Normalize clears inference fields the provider does not read and makes the
// system prompt and digest paths absolute and clean. It returns the dotted names
// of the fields it cleared that were set. An unknown provider is left alone for
// validate to report.
func (r *Config) Normalize() (dropped []string) {
	in := &r.Inference
	provider := in.Provider
	if provider == "" {
		provider = defaultProvider
	}
	if slices.Contains(Providers, provider) {
		for _, f := range inferenceFields {
			if !slices.Contains(f.providers, provider) && f.isSet(in) {
				f.clear(in)
				dropped = append(dropped, "inference."+f.name)
			}
		}
	}
	in.SystemPrompt.Path = absClean(in.SystemPrompt.Path)
	r.Ingest.DigestDir = absClean(r.Ingest.DigestDir)
	return dropped
}

// absClean resolves p against the working directory, as the files are read
// today. An empty path stays empty.
func absClean(p string) string {
	if p == "" {
		return ""
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	return abs
}

// applyDefaults fills unset fields that apply to the provider.
func (r *Config) applyDefaults() {
	in := &r.Inference
	if in.Provider == "" {
		in.Provider = defaultProvider
	}
	if Applies(in.Provider, "host") && in.Host == "" {
		in.Host = defaultHost
	}
	if Applies(in.Provider, "model") && in.Model == "" {
		in.Model = defaultModel
	}
	if Applies(in.Provider, "think") && in.Think == nil {
		t := true
		in.Think = &t
	}
	if Applies(in.Provider, "effort") && in.Effort == "" {
		in.Effort = defaultEffort
	}
	if Applies(in.Provider, "structured_output") && in.StructuredOutput == nil {
		t := true
		in.StructuredOutput = &t
	}
	if in.BatchSize <= 0 {
		in.BatchSize = defaultBatchSize
	}
	if in.MaxParallel <= 0 {
		in.MaxParallel = defaultMaxParallel
	}
	if r.Ingest.Since == 0 {
		r.Ingest.Since = Duration(defaultSince)
	}
}

// FieldError is one validation failure, keyed by the dotted field name so a UI
// can show it inline.
type FieldError struct {
	Field string
	Msg   string
}

// Error returns the message, which already names the field where it helps.
func (e FieldError) Error() string { return e.Msg }

// FieldErrors extracts every FieldError from an error built with errors.Join.
func FieldErrors(err error) []FieldError {
	var out []FieldError
	var walk func(error)
	walk = func(e error) {
		switch u := e.(type) {
		case nil:
		case FieldError:
			out = append(out, u)
		case interface{ Unwrap() []error }:
			for _, c := range u.Unwrap() {
				walk(c)
			}
		default:
			if inner := errors.Unwrap(e); inner != nil {
				walk(inner)
			}
		}
	}
	walk(err)
	return out
}

// validate checks the run config and reports every failure, not only the first.
func (r *Config) validate() error {
	var errs []error
	add := func(field, format string, args ...any) {
		errs = append(errs, FieldError{Field: field, Msg: fmt.Sprintf(format, args...)})
	}
	in := r.Inference
	if !slices.Contains(Providers, in.Provider) {
		add("inference.provider", "invalid provider %q, must be ollama, openai, vllm or heuristic", in.Provider)
	}
	for _, e := range []struct{ field, value string }{
		{"inference.effort", in.Effort},
		{"inference.summary.effort", in.Summary.Effort},
	} {
		if e.value != "" && !slices.Contains(efforts, e.value) {
			add(e.field, "%s %q, must be one of %s", e.field, e.value, strings.Join(efforts, ", "))
		}
	}
	if in.Summary != (SummaryConfig{}) && in.Summary.Model == "" {
		add(
			"inference.summary.model",
			"inference.summary.model is required when other inference.summary fields are set",
		)
	}
	for _, k := range []struct{ field, value string }{
		{inferenceKeyField, in.APIKey},
		{summaryKeyField, in.Summary.APIKey},
	} {
		if k.value == "" {
			continue
		}
		if err := ValidRef(k.value); err != nil {
			add(k.field, "%s: %s", k.field, err)
		}
	}
	if r.Ingest.Since < 0 {
		add("ingest.since", "since must be positive, got %s", r.Ingest.Since)
	}
	return errors.Join(errs...)
}

// ThinkEnabled reports whether reasoning is on. Unset means off; Prepare sets it
// for every provider where it applies.
func (c InferenceConfig) ThinkEnabled() bool { return c.Think != nil && *c.Think }

// MarshalYAML writes the setting back as it was written: false when disabled,
// the path when set.
func (s SystemPromptSetting) MarshalYAML() (any, error) {
	if s.Disabled {
		return false, nil
	}
	return s.Path, nil
}

// tuningYAML is ModelTuning with unset fields omitted.
type tuningYAML struct {
	NumCtx         int `yaml:"num_ctx,omitempty"`
	MaxTokens      int `yaml:"max_tokens,omitempty"`
	TokensPerItem  int `yaml:"tokens_per_item,omitempty"`
	TokensOverhead int `yaml:"tokens_overhead,omitempty"`
	TokensThinking int `yaml:"tokens_thinking,omitempty"`
	ReasonMaxChars int `yaml:"reason_max_chars,omitempty"`
}

// inferenceYAML is InferenceConfig with unset fields omitted, so the canonical
// YAML stays minimal.
type inferenceYAML struct {
	Provider         string              `yaml:"provider,omitempty"`
	Host             string              `yaml:"host,omitempty"`
	Model            string              `yaml:"model,omitempty"`
	APIKey           string              `yaml:"api_key,omitempty"`
	Think            *bool               `yaml:"think,omitempty"`
	Effort           string              `yaml:"effort,omitempty"`
	StructuredOutput *bool               `yaml:"structured_output,omitempty"`
	SystemPrompt     SystemPromptSetting `yaml:"system_prompt,omitempty"`
	BatchSize        int                 `yaml:"batch_size,omitempty"`
	MaxParallel      int                 `yaml:"max_parallel,omitempty"`
	ModelTuning      *tuningYAML         `yaml:"model_tuning,omitempty"`
	Summary          summaryYAML         `yaml:"summary,omitempty"`
}

type summaryYAML struct {
	Provider string `yaml:"provider,omitempty"`
	Host     string `yaml:"host,omitempty"`
	Model    string `yaml:"model,omitempty"`
	APIKey   string `yaml:"api_key,omitempty"`
	Think    *bool  `yaml:"think,omitempty"`
	Effort   string `yaml:"effort,omitempty"`
}

// MarshalYAML renders the minimal form: unset optional fields are omitted.
func (c InferenceConfig) MarshalYAML() (any, error) {
	out := inferenceYAML{
		Provider: c.Provider, Host: c.Host, Model: c.Model, APIKey: c.APIKey,
		Think: c.Think, Effort: c.Effort, StructuredOutput: c.StructuredOutput,
		SystemPrompt: c.SystemPrompt, BatchSize: c.BatchSize, MaxParallel: c.MaxParallel,
		Summary: summaryYAML(c.Summary),
	}
	t := tuningYAML{
		NumCtx: c.ModelTuning.NumCtx, MaxTokens: c.ModelTuning.MaxTokens,
		TokensPerItem: c.ModelTuning.TokensPerItem, TokensOverhead: c.ModelTuning.TokensOverhead,
		TokensThinking: c.ModelTuning.TokensThinking, ReasonMaxChars: c.ModelTuning.ReasonMaxChars,
	}
	if t != (tuningYAML{}) {
		out.ModelTuning = &t
	}
	return out, nil
}

// Marshal renders the YAML of a canonical (un-defaulted) config, see Canonical.
func (r Config) Marshal() ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(r); err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}
	return buf.Bytes(), nil
}

// hashView is the fixed JSON shape Hash digests, independent of YAML encoding.
type hashView struct {
	User      string `json:"user,omitempty"`
	Inference struct {
		Provider         string `json:"provider,omitempty"`
		Host             string `json:"host,omitempty"`
		Model            string `json:"model,omitempty"`
		APIKey           string `json:"api_key,omitempty"`
		Think            *bool  `json:"think,omitempty"`
		Effort           string `json:"effort,omitempty"`
		StructuredOutput *bool  `json:"structured_output,omitempty"`
		SystemPrompt     struct {
			Path     string `json:"path,omitempty"`
			Disabled bool   `json:"disabled,omitempty"`
		} `json:"system_prompt"`
		BatchSize   int        `json:"batch_size,omitempty"`
		MaxParallel int        `json:"max_parallel,omitempty"`
		ModelTuning tuningJSON `json:"model_tuning"`
		Summary     struct {
			Provider string `json:"provider,omitempty"`
			Host     string `json:"host,omitempty"`
			Model    string `json:"model,omitempty"`
			APIKey   string `json:"api_key,omitempty"`
			Think    *bool  `json:"think,omitempty"`
			Effort   string `json:"effort,omitempty"`
		} `json:"summary"`
	} `json:"inference"`
	Ingest struct {
		SinceNS   int64  `json:"since_ns,omitempty"`
		DigestDir string `json:"digest_dir,omitempty"`
	} `json:"ingest"`
}

type tuningJSON struct {
	NumCtx         int `json:"num_ctx,omitempty"`
	MaxTokens      int `json:"max_tokens,omitempty"`
	TokensPerItem  int `json:"tokens_per_item,omitempty"`
	TokensOverhead int `json:"tokens_overhead,omitempty"`
	TokensThinking int `json:"tokens_thinking,omitempty"`
	ReasonMaxChars int `json:"reason_max_chars,omitempty"`
}

// Hash returns the hex sha256 of a fixed JSON form of the config, so equal
// configs hash equal whatever their YAML spelling. Call it on a prepared config.
func (r Config) Hash() string {
	var v hashView
	v.User = r.User
	in := r.Inference
	v.Inference.Provider, v.Inference.Host, v.Inference.Model = in.Provider, in.Host, in.Model
	v.Inference.APIKey, v.Inference.Think, v.Inference.Effort = in.APIKey, in.Think, in.Effort
	v.Inference.StructuredOutput = in.StructuredOutput
	v.Inference.SystemPrompt.Path, v.Inference.SystemPrompt.Disabled = in.SystemPrompt.Path, in.SystemPrompt.Disabled
	v.Inference.BatchSize, v.Inference.MaxParallel = in.BatchSize, in.MaxParallel
	v.Inference.ModelTuning = tuningJSON{
		NumCtx: in.ModelTuning.NumCtx, MaxTokens: in.ModelTuning.MaxTokens,
		TokensPerItem: in.ModelTuning.TokensPerItem, TokensOverhead: in.ModelTuning.TokensOverhead,
		TokensThinking: in.ModelTuning.TokensThinking, ReasonMaxChars: in.ModelTuning.ReasonMaxChars,
	}
	v.Inference.Summary.Provider, v.Inference.Summary.Host = in.Summary.Provider, in.Summary.Host
	v.Inference.Summary.Model, v.Inference.Summary.APIKey = in.Summary.Model, in.Summary.APIKey
	v.Inference.Summary.Think, v.Inference.Summary.Effort = in.Summary.Think, in.Summary.Effort
	v.Ingest.SinceNS = int64(r.Ingest.Since)
	v.Ingest.DigestDir = r.Ingest.DigestDir
	raw, err := json.Marshal(v)
	if err != nil { // a fixed struct of plain values cannot fail to marshal
		panic(fmt.Sprintf("config hash: %v", err))
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// migrations upgrade stored YAML one schema version at a time: migrations[v]
// turns a version v document into version v+1.
var migrations = map[int]func(raw []byte) ([]byte, error){}

// ParseStored strictly decodes a stored config and returns the effective
// config: ParseStoredCanonical, then Prepare.
func ParseStored(raw []byte, schemaVersion int) (Config, error) {
	r, err := ParseStoredCanonical(raw, schemaVersion)
	if err != nil {
		return Config{}, err
	}
	if _, err := r.Prepare(); err != nil {
		return Config{}, err
	}
	return r, nil
}

// ParseStoredCanonical strictly decodes a stored config written at
// schemaVersion, runs any migrations and returns the canonical form (only what
// was set). Unknown keys are an error.
func ParseStoredCanonical(raw []byte, schemaVersion int) (Config, error) {
	if schemaVersion < 1 {
		return Config{}, fmt.Errorf("invalid config schema version %d", schemaVersion)
	}
	if schemaVersion > RunSchemaVersion {
		return Config{}, fmt.Errorf(
			"config schema version %d was written by a newer rabbithole (this one reads up to %d); upgrade rabbithole",
			schemaVersion, RunSchemaVersion)
	}
	for v := schemaVersion; v < RunSchemaVersion; v++ {
		migrate, ok := migrations[v]
		if !ok {
			return Config{}, fmt.Errorf("no migration from config schema version %d", v)
		}
		var err error
		if raw, err = migrate(raw); err != nil {
			return Config{}, fmt.Errorf("migrate config from schema version %d: %w", v, err)
		}
	}
	var r Config
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&r); err != nil && !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("parse stored config: %w", err)
	}
	if _, err := r.Canonical(); err != nil {
		return Config{}, err
	}
	return r, nil
}
