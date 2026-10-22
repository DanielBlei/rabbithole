// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package profilemgr

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/rs/zerolog"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// The config import core, shared by the CLI (`config import`, --config) and the
// settings page. Parsing a file into its stored form is Prepare; saving it as a
// new version is ApplyImport.

// ImportOpts carries what an import needs from its caller.
type ImportOpts struct {
	// BeforeSave runs before the save; tests use it to make the save conflict.
	BeforeSave func()
	// Log receives the import's warnings and notes. The zero value is silent.
	Log zerolog.Logger
	// Mode says how the import was asked for; the zero value is an upload.
	Mode ImportMode
	// ExpectedVersion, when set, is the active version the caller reviewed. An
	// upload whose active version differs fails with store.ErrConfigConflict,
	// and an upload never retries a conflicting save.
	ExpectedVersion *int
}

// ImportOutcome reports what an import did.
type ImportOutcome struct {
	// Unchanged is true when the file matched its last-applied marker and nothing
	// was done beyond refreshing secrets.
	Unchanged bool
	Saved     bool
	// FeedSince is true when the upload's ingest.since was applied to the feed
	// defaults without saving a config version.
	FeedSince bool
	Version   int // the version saved, or the active one when nothing was
	Previous  int // the version that was active before a save, 0 when there was none
	// Replaced is the version a save displaced when it came from somewhere other
	// than this file; nil otherwise.
	Replaced *store.ConfigVersion
	// Defaulted names the settings of the file that equal their default and so
	// were not saved.
	Defaulted []string
}

// Prepared is a config file in the form that is stored: canonical, marshaled
// and hashed, with any literal api_key already replaced by its reference.
type Prepared struct {
	Run           config.Config
	Content       []byte
	Hash          string
	EffectiveHash string          // Run.EffectiveHash, what the no-change checks compare
	Secrets       []config.Secret // literal keys lifted out; never log Value
	Legacy        config.Legacy
	Dropped       []string // fields the provider does not read, removed
	Defaulted     []string // fields equal to their default, left unset so they stay defaults
	Warnings      []string // deprecated keys the file used, each naming its new home
}

// Prepare turns a parsed file into its stored form without touching any store.
// label names the file in messages. Nothing is returned for an invalid config.
// A system_prompt is taken as text here; the CLI inlines a prompt file first
// (config.File.InlineSystemPrompt).
func Prepare(file config.File, label string, lg zerolog.Logger) (Prepared, error) {
	run, _, legacy, dropped := file.Split()
	if len(dropped) > 0 {
		provider := run.Inference.Provider
		if provider == "" {
			provider = config.ProviderOllama
		}
		lg.Warn().Msgf("ignored for provider %s: %s", provider, strings.Join(dropped, ", "))
	}
	if legacy.Since < 0 {
		return Prepared{}, fmt.Errorf(
			"%s is not a valid config, nothing was saved:\ningest.since must be positive, got %s",
			label,
			legacy.Since,
		)
	}
	secrets := run.ExtractLiterals()
	defaulted := run.DropDefaults()
	if _, err := run.Canonical(); err != nil {
		return Prepared{}, fmt.Errorf(
			"%s is not a valid config, nothing was saved:\n%w",
			label,
			err,
		)
	}
	content, err := run.Marshal()
	if err != nil {
		return Prepared{}, err
	}
	return Prepared{
		Run: run, Content: content, Hash: run.Hash(), EffectiveHash: run.EffectiveHash(),
		Secrets: secrets, Legacy: legacy, Dropped: dropped, Defaulted: defaulted, Warnings: file.Deprecations(),
	}, nil
}

// PrepareRun is Prepare for a run config that is already split: the canonical
// form of run, with no secrets or legacy directives.
func PrepareRun(run config.Config) (Prepared, error) {
	if _, err := run.Canonical(); err != nil {
		return Prepared{}, err
	}
	content, err := run.Marshal()
	if err != nil {
		return Prepared{}, err
	}
	return Prepared{
		Run:           run,
		Content:       content,
		Hash:          run.Hash(),
		EffectiveHash: run.EffectiveHash(),
	}, nil
}

// ImportMode says how an import was asked for, which decides whether the
// last-applied marker of its source is consulted and kept.
type ImportMode int

const (
	// ImportUpload is an upload on the settings page: it compares against the
	// active version, saves when they differ, and keeps no marker.
	ImportUpload ImportMode = iota
	// ImportCommand is an explicit `config import`: it compares against the
	// active version like an upload, and records the marker.
	ImportCommand
	// ImportOnStart is the implicit --config re-apply: nothing happens while the
	// file is unchanged since it was last applied.
	ImportOnStart
)

// AppliedHash identifies everything an import applies: the stored config plus
// the legacy directives with the content of the files they name, so editing a
// feeds or profile file counts as a change. Secrets are left out; they are
// refreshed on every import.
func (p Prepared) AppliedHash() string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "config:%s\n", p.Hash)
	_, _ = fmt.Fprintf(h, "since:%d\n", int64(p.Legacy.Since))
	for _, ref := range []struct{ kind, path string }{{"feeds", p.Legacy.FeedsPath}, {"profile", p.Legacy.ProfilePath}} {
		if ref.path == "" {
			continue
		}
		raw, err := os.ReadFile(ref.path)
		sum := "unreadable"
		if err == nil {
			sum = fmt.Sprintf("%x", sha256.Sum256(raw))
		}
		_, _ = fmt.Fprintf(h, "%s:%s:%s\n", ref.kind, ref.path, sum)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ApplyImport applies p. label names the origin in messages and source is the
// value stored with a version ("import:<path>").
//
// A new version is saved unless p behaves like the active one (equal
// EffectiveHash). The import wins over the active version; when it replaces one
// that came from somewhere else that is reported through the outcome. A literal
// api_key goes to the env file and the stored copy keeps only the reference.
// Nothing is saved when the profile cannot load or the secrets cannot be
// written.
//
// For a file source (any mode but ImportUpload) the legacy directives (feed
// seed file, ingest.since, profile) run whenever the file's AppliedHash differs
// from the marker, even if no version was saved, and the marker is updated at
// the end. With ImportOnStart an unchanged file only refreshes its secrets.
func ApplyImport(
	ctx context.Context,
	db *store.Store,
	env *config.EnvFile,
	p Prepared,
	label, source string,
	opts ImportOpts,
) (ImportOutcome, error) {
	lg := opts.Log
	cur, haveCur, err := db.ActiveConfig(ctx)
	if err != nil {
		return ImportOutcome{}, err
	}

	if opts.Mode == ImportUpload && opts.ExpectedVersion != nil && cur.Version != *opts.ExpectedVersion {
		return ImportOutcome{}, store.ErrConfigConflict
	}

	marked := opts.Mode != ImportUpload
	applied := p.AppliedHash()
	changed := false
	if marked {
		m, found, err := db.ConfigImport(ctx, source)
		if err != nil {
			return ImportOutcome{}, err
		}
		changed = !found || m.AppliedHash != applied
		if !changed && opts.Mode == ImportOnStart {
			lg.Debug().
				Msgf("config unchanged since last applied from %s (version %d)", label, cur.Version)
			refreshSecrets(lg, env, p.Secrets, label)
			return ImportOutcome{Version: cur.Version, Unchanged: true}, nil
		}
	}
	if changed {
		for _, w := range p.Warnings {
			lg.Warn().Msgf("%s: %s", label, w)
		}
	}

	// An upload keeps no marker, so a changed ingest.since is what makes it a
	// change when the config itself matches.
	sinceChanged := false
	if opts.Mode == ImportUpload {
		if _, sinceChanged, err = SinceChange(ctx, db, p); err != nil {
			return ImportOutcome{}, err
		}
		changed = sinceChanged
	}

	// finish runs the directives for a save or a changed file, then records the
	// marker.
	finish := func(saved bool) error {
		if saved || changed {
			runLegacyDirectives(ctx, lg, db, p.Legacy)
		}
		if marked {
			return db.RecordConfigImport(ctx, source, applied)
		}
		return nil
	}

	var secretsStored bool
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			if cur, haveCur, err = db.ActiveConfig(ctx); err != nil {
				return ImportOutcome{}, err
			}
		}
		out := ImportOutcome{Version: cur.Version, FeedSince: sinceChanged, Defaulted: p.Defaulted}
		equal := haveCur && activeEffectiveHash(cur) == p.EffectiveHash
		if equal && !changed {
			lg.Debug().Msgf("config unchanged (version %d)", cur.Version)
			refreshSecrets(lg, env, p.Secrets, label)
			noteUnreadFeeds(lg, p.Legacy.FeedsPath, label)
			return out, finish(false)
		}

		// Checked before anything is written: a profile that cannot load must not
		// leave a saved version behind that a rerun would never retry.
		if _, err := config.LoadProfileFile(p.Legacy.ProfilePath); err != nil {
			return out, fmt.Errorf("%s: nothing was saved: %w", label, err)
		}
		if equal {
			lg.Debug().Msgf("config unchanged (version %d)", cur.Version)
			refreshSecrets(lg, env, p.Secrets, label)
			return out, finish(false)
		}
		if !secretsStored {
			if err := storeSecrets(lg, env, p.Secrets, label); err != nil {
				return out, err
			}
			secretsStored = true
		}
		if opts.BeforeSave != nil {
			opts.BeforeSave()
		}
		expected := 0
		if haveCur {
			expected = cur.Version
		}
		saved, err := db.SaveConfig(ctx, string(p.Content), p.Hash, source, expected)
		if errors.Is(err, store.ErrConfigConflict) {
			if attempt == 0 && opts.Mode != ImportUpload {
				continue
			}
			if opts.Mode == ImportUpload {
				return out, store.ErrConfigConflict
			}
			return out, errors.New(
				"the stored config changed while importing; run the command again",
			)
		}
		if err != nil {
			return out, err
		}
		out.Saved, out.Version = true, saved.Version
		if haveCur {
			out.Previous = cur.Version
		}
		switch {
		case haveCur && cur.Source == source:
			lg.Info().Msgf("config updated from %s (version %d)", label, saved.Version)
		case haveCur:
			replaced := cur
			out.Replaced = &replaced
		default:
			lg.Info().Msgf("config imported from %s (version %d)", label, saved.Version)
		}
		return out, finish(true)
	}
}

// SinceChange reports the feed defaults lookback an upload would replace. changed
// is false when the upload sets no ingest.since or it already matches; from is
// the current value, zero when none is set.
func SinceChange(ctx context.Context, db *store.Store, p Prepared) (from config.Duration, changed bool, err error) {
	if p.Legacy.Since <= 0 {
		return 0, false, nil
	}
	d, err := db.FeedDefaults(ctx)
	if err != nil {
		return 0, false, err
	}
	if d.Since != nil {
		from = *d.Since
	}
	return from, from != p.Legacy.Since, nil
}

// activeEffectiveHash is the EffectiveHash of a stored version, "" when it
// cannot be read, which never equals a prepared import's hash.
func activeEffectiveHash(v store.ConfigVersion) string {
	cfg, err := config.ParseStoredCanonical([]byte(v.Content), v.SchemaVersion)
	if err != nil {
		return ""
	}
	return cfg.EffectiveHash()
}

// storeSecrets puts each literal api_key from the file into the env file under
// its variable, before an import saves anything, so a failure aborts the import.
func storeSecrets(
	lg zerolog.Logger,
	env *config.EnvFile,
	secrets []config.Secret,
	file string,
) error {
	for _, sec := range secrets {
		if err := writeSecret(lg, env, sec, file); err != nil {
			return fmt.Errorf("saving %s from %s to %s: %w", sec.Name, file, env.Path, err)
		}
	}
	return nil
}

// refreshSecrets does the same when the file equals the stored config. A key
// changed in the file is rotated into the env file; a failure is only warned
// about, since nothing depends on it.
func refreshSecrets(lg zerolog.Logger, env *config.EnvFile, secrets []config.Secret, file string) {
	for _, sec := range secrets {
		if err := writeSecret(lg, env, sec, file); err != nil {
			lg.Warn().
				Str("var", sec.Name).
				Err(err).
				Msgf("could not update %s in the env file", sec.Name)
		}
	}
}

// writeSecret sets one variable. A variable the real environment sets is left to
// it, and a value already in place is not rewritten. Values are compared but
// never logged.
func writeSecret(lg zerolog.Logger, env *config.EnvFile, sec config.Secret, file string) error {
	current := os.Getenv(sec.Name)
	switch env.Source(sec.Name) {
	case config.SourceEnvironment:
		if current != sec.Value {
			lg.Warn().Str("var", sec.Name).Msgf(
				"%s is set in the environment, so the %s in %s is ignored", sec.Name, sec.Field, file)
		}
		return nil
	case config.SourceEnvFile:
		if current == sec.Value {
			return nil
		}
	}
	if err := env.Set(sec.Name, sec.Value); err != nil {
		return err
	}
	lg.Warn().Str("var", sec.Name).Msgf(
		"the %s in %s is kept in %s as %s; the stored config holds only the reference",
		sec.Field, file, env.Path, sec.Name)
	return nil
}

// noteUnreadFeeds says why a feeds path in a file did nothing.
func noteUnreadFeeds(lg zerolog.Logger, feedsPath, label string) {
	if feedsPath != "" {
		lg.Info().Msgf("ingest.feeds in %s is read only when the file is imported;"+
			" add feeds from the Sources page", label)
	}
}

// runLegacyDirectives applies the old-shape keys that are not stored: the feed
// seed file, the feed-defaults lookback (ingest.since) and the interest profile.
// They run when the import saved a version or its file changed, and a failure is
// only a warning: the config is already saved and a rerun would not retry. A
// feeds file that sets defaults.since wins over ingest.since.
func runLegacyDirectives(
	ctx context.Context,
	lg zerolog.Logger,
	db *store.Store,
	legacy config.Legacy,
) {
	feedsSince := seedLegacyFeeds(ctx, lg, db, legacy.FeedsPath)
	if legacy.Since > 0 && !feedsSince {
		if err := db.SetDefaultSince(ctx, legacy.Since); err != nil {
			lg.Warn().Err(err).Msg("setting the feed defaults since from ingest.since failed")
		}
	}
	if err := New(db).BootstrapLegacy(ctx, legacy.ProfilePath); err != nil {
		lg.Warn().
			Err(err).
			Str("profile", legacy.ProfilePath).
			Msg("importing the profile failed; select it in the web UI")
	}
}

// seedLegacyFeeds imports feeds the store has never seen from a legacy seed file
// and reports whether the file sets defaults.since.
//
// Every failure here is a warning, never an error: the config is already saved,
// and a missing, unreadable or half-broken seed file is a reason to say so and
// carry on.
func seedLegacyFeeds(ctx context.Context, lg zerolog.Logger, db *store.Store, path string) bool {
	if path == "" {
		return false
	}
	doc, found, err := config.ReadFeedsFile(path)
	if err != nil {
		lg.Warn().Err(err).Str("feeds", path).Msg("reading the feed seed file failed")
		return false
	}
	if !found {
		lg.Warn().Str("feeds", path).Msg("the feed seed file named by ingest.feeds does not exist")
		return false
	}
	result, err := db.SeedFeeds(ctx, *doc)
	if err != nil {
		lg.Warn().Err(err).Str("feeds", path).Msg("seeding feeds failed")
		return false
	}
	for _, warning := range result.Warnings {
		lg.Warn().Str("feeds", path).Msg("skipped while seeding: " + warning)
	}
	lg.Info().Str("feeds", path).Int("added", result.Added).Int("skipped", result.Skipped).
		Msg("seeded feeds from the legacy seed file")
	return doc.Defaults.Since != nil
}
