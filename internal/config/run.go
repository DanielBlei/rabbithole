// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/rs/zerolog/log"
	"gopkg.in/yaml.v3"
)

// RunSchemaVersion is the schema version of a stored Config.
const RunSchemaVersion = 2

// File is the import shape: the whole YAML file as written, including the keys
// that are not stored (store, profile, ingest.feeds). It is decoded leniently,
// so unknown keys are ignored.
type File struct {
	User      string        `yaml:"user"`
	Profile   string        `yaml:"profile"`
	Inference FileInference `yaml:"inference"`
	Ingest    FileIngest    `yaml:"ingest"`
	Store     StoreConfig   `yaml:"store"`
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
	// DigestDir is deprecated: `ingest --markdown --out <dir>` replaces it. Removed in v0.4.0.
	DigestDir string `yaml:"digest_dir,omitempty"`
}

// Legacy holds the keys of an imported old-shape file that are not stored.
type Legacy struct {
	ProfilePath string   // profile, as written
	FeedsPath   string   // ingest.feeds, as written
	Since       Duration // ingest.since; written to the feed defaults on import, 0 when unset
}

// Deprecations lists one warning per deprecated key the file sets, each naming
// where the setting lives now. They are returned, not logged, so a caller can
// show them wherever the import happens.
func (f File) Deprecations() []string {
	var out []string
	if f.Ingest.Since != 0 {
		out = append(out, "ingest.since is deprecated: it is written to the feed defaults "+
			"(Sources, Defaults, since) on import; removed in v0.4.0")
	}
	if f.Ingest.DigestDir != "" {
		out = append(out, "ingest.digest_dir is deprecated: pass --out <dir> to ingest --markdown; removed in v0.4.0")
	}
	if len(f.Inference.Summary) > 0 {
		out = append(out, "inference.summary is not used and is ignored; removed in v0.4.0")
	}
	return out
}

// InlineSystemPrompt replaces a system_prompt that names a file with the file's
// text, read once. A relative path is taken from baseDir, the config file's
// directory. The value counts as a path when LooksLikePath says so, or when it is
// one line and names an existing file; anything else is the prompt text already.
// Only the CLI calls this: a web upload never reads server paths.
func (f *File) InlineSystemPrompt(baseDir string) error {
	v := strings.TrimSpace(f.Inference.SystemPrompt.Text)
	if v == "" {
		return nil
	}
	path := v
	if !filepath.IsAbs(path) {
		path = filepath.Join(baseDir, path)
	}
	if !LooksLikePath(v) {
		if strings.ContainsAny(v, "\r\n") {
			return nil
		}
		if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() {
			return nil
		}
	}
	text, err := loadTrimmedFile(path)
	if err != nil {
		return fmt.Errorf("inference.system_prompt %q looks like a file path but cannot be read "+
			"(write the prompt text instead): %w", v, err)
	}
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("inference.system_prompt file %q is empty", v)
	}
	f.Inference.SystemPrompt.Text = text
	return nil
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
// block and the legacy import directives. The run config is normalized but neither
// defaulted nor validated (see Prepare); dropped lists the dotted keys
// normalization removed.
func (f File) Split() (run Config, store StoreConfig, legacy Legacy, dropped []string) {
	run = Config{
		User:      f.User,
		Inference: f.Inference.InferenceConfig,
		Ingest:    IngestConfig{DigestDir: f.Ingest.DigestDir},
	}
	dropped = run.Normalize()
	legacy.ProfilePath = f.Profile
	legacy.FeedsPath = f.Ingest.Feeds
	legacy.Since = f.Ingest.Since
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
// digest path absolute and clean. It returns the dotted names
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
	if in.Effort != "" && !slices.Contains(efforts, in.Effort) {
		add("inference.effort", "inference.effort %q, must be one of %s", in.Effort, strings.Join(efforts, ", "))
	}
	if in.APIKey != "" {
		if err := ValidRef(in.APIKey); err != nil {
			add(inferenceKeyField, "%s: %s", inferenceKeyField, err)
		}
	}
	return errors.Join(errs...)
}

// ThinkEnabled reports whether reasoning is on. Unset means off; Prepare sets it
// for every provider where it applies.
func (c InferenceConfig) ThinkEnabled() bool { return c.Think != nil && *c.Think }

// MarshalYAML writes the setting back as it was written: false when disabled,
// the text when set.
func (s SystemPromptSetting) MarshalYAML() (any, error) {
	if s.Disabled {
		return false, nil
	}
	return s.Text, nil
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
}

// MarshalYAML renders the minimal form: unset optional fields are omitted.
func (c InferenceConfig) MarshalYAML() (any, error) {
	out := inferenceYAML{
		Provider: c.Provider, Host: c.Host, Model: c.Model, APIKey: c.APIKey,
		Think: c.Think, Effort: c.Effort, StructuredOutput: c.StructuredOutput,
		SystemPrompt: c.SystemPrompt, BatchSize: c.BatchSize, MaxParallel: c.MaxParallel,
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

// Hash returns the hex sha256 of the canonical YAML, so configs that set the same
// fields hash equal whatever their spelling in the file. Call it on a canonical
// config; EffectiveHash compares what a run would use.
func (r Config) Hash() string {
	raw, err := r.Marshal()
	if err != nil { // a fixed struct of plain values cannot fail to marshal
		panic(fmt.Sprintf("config hash: %v", err))
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// EffectiveHash is the Hash of the prepared form: normalized with defaults filled
// in. Two configs with equal EffectiveHash behave identically, even when one pins
// a value the other leaves to the default.
func (r Config) EffectiveHash() string {
	eff := r.Clone()
	_, _ = eff.Prepare() // an invalid config still hashes; callers validate separately
	return eff.Hash()
}

// migrations upgrade stored YAML one schema version at a time: migrations[v]
// turns a version v document into version v+1.
var migrations = map[int]func(raw []byte) ([]byte, error){
	1: migrateV1,
}

// migrateV1 rewrites a version 1 document: ingest.since and inference.summary are
// gone, and a system_prompt that holds a file path becomes the file's text (an
// unreadable file drops the setting with a warning). Everything else is kept.
func migrateV1(raw []byte) ([]byte, error) {
	f, err := ParseFile(raw)
	if err != nil {
		return nil, err
	}
	if err := f.InlineSystemPrompt(""); err != nil {
		log.Warn().Err(err).Msg("stored config: system_prompt dropped while upgrading it")
		f.Inference.SystemPrompt = SystemPromptSetting{}
	}
	c := Config{
		User:      f.User,
		Inference: f.Inference.InferenceConfig,
		Ingest:    IngestConfig{DigestDir: f.Ingest.DigestDir},
	}
	return c.Marshal()
}

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
