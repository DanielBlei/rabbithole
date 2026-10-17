// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/profilemgr"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// importOpts carries what an import needs from its caller.
type importOpts struct {
	// beforeSave runs before the save; tests use it to make the save conflict.
	beforeSave func()
}

// importOutcome reports what an import did.
type importOutcome struct {
	Saved    bool
	Version  int // the version saved, or the active one when nothing was
	Previous int // the version that was active before a save, 0 when there was none
	// Replaced is the version a save displaced when it came from somewhere other
	// than this file; nil otherwise.
	Replaced *store.ConfigVersion
}

// replacedSummary names a displaced version: its number, where it came from and
// when it was saved.
func replacedSummary(v store.ConfigVersion) string {
	return fmt.Sprintf("%s, saved %s", describeConfigSource(v.Source), v.CreatedAt.Local().Format("15:04"))
}

// importConfig reads the config file at path and applies it: the same content
// as the active version is a no-op, anything else is saved as a new version. The
// file always wins; when it replaces a version that came from somewhere else,
// that is warned about with the undo command. A literal api_key goes to the env
// file and the stored copy keeps only the reference. Nothing is saved when the
// file is invalid or the secrets cannot be written.
func importConfig(
	ctx context.Context, db *store.Store, env *config.EnvFile, path string, opts importOpts,
) (importOutcome, error) {
	file, err := config.ReadFile(path)
	if err != nil {
		return importOutcome{}, err
	}
	cur, haveCur, err := db.ActiveConfig(ctx)
	if err != nil {
		return importOutcome{}, err
	}

	run, _, legacy, dropped := file.Split()
	if len(dropped) > 0 {
		provider := run.Inference.Provider
		if provider == "" {
			provider = config.ProviderOllama
		}
		log.Warn().Msgf("ignored for provider %s: %s", provider, strings.Join(dropped, ", "))
	}
	secrets := run.ExtractLiterals()
	if _, err := run.Canonical(); err != nil {
		return importOutcome{}, fmt.Errorf("%s is not a valid config, nothing was saved:\n%w", path, err)
	}
	content, err := run.Marshal()
	if err != nil {
		return importOutcome{}, err
	}
	hash := run.Hash()
	abs, err := filepath.Abs(path)
	if err != nil {
		return importOutcome{}, fmt.Errorf("resolve config path: %w", err)
	}
	// A symlink or another spelling of the same file owns the config too.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	source := store.ConfigSourceImport + abs

	var secretsStored bool
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			if cur, haveCur, err = db.ActiveConfig(ctx); err != nil {
				return importOutcome{}, err
			}
		}
		out := importOutcome{Version: cur.Version}
		if haveCur && cur.ContentHash == hash {
			log.Debug().Msgf("config unchanged (version %d)", cur.Version)
			refreshSecrets(env, secrets, path)
			noteUnreadFeeds(file, path)
			return out, nil
		}

		// Checked before anything is written: a profile that cannot load must not
		// leave a saved version behind that a rerun would never retry.
		if _, err := config.LoadProfileFile(legacy.ProfilePath); err != nil {
			return out, fmt.Errorf("%s: nothing was saved: %w", path, err)
		}
		if !secretsStored {
			if err := storeSecrets(env, secrets, path); err != nil {
				return out, err
			}
			secretsStored = true
		}
		if opts.beforeSave != nil {
			opts.beforeSave()
		}
		expected := 0
		if haveCur {
			expected = cur.Version
		}
		saved, err := db.SaveConfig(ctx, string(content), hash, source, expected)
		if errors.Is(err, store.ErrConfigConflict) {
			if attempt == 0 {
				continue
			}
			return out, errors.New("the stored config changed while importing; run the command again")
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
			log.Info().Msgf("config updated from %s (version %d)", path, saved.Version)
		case haveCur:
			replaced := cur
			out.Replaced = &replaced
		default:
			log.Info().Msgf("config imported from %s (version %d)", path, saved.Version)
		}
		runLegacyDirectives(ctx, db, legacy)
		return out, nil
	}
}

// storeSecrets puts each literal api_key from the file into the env file under
// its variable, before an import saves anything, so a failure aborts the import.
func storeSecrets(env *config.EnvFile, secrets []config.Secret, file string) error {
	for _, sec := range secrets {
		if err := writeSecret(env, sec, file); err != nil {
			return fmt.Errorf("saving %s from %s to %s: %w", sec.Name, file, env.Path, err)
		}
	}
	return nil
}

// refreshSecrets does the same when the file equals the stored config. A key changed in the file is rotated into the
// env file; a failure is only warned about, since nothing depends on it.
func refreshSecrets(env *config.EnvFile, secrets []config.Secret, file string) {
	for _, sec := range secrets {
		if err := writeSecret(env, sec, file); err != nil {
			log.Warn().Str("var", sec.Name).Err(err).Msgf("could not update %s in the env file", sec.Name)
		}
	}
}

// writeSecret sets one variable. A variable the real environment sets is left to
// it, and a value already in place is not rewritten. Values are compared but
// never logged.
func writeSecret(env *config.EnvFile, sec config.Secret, file string) error {
	current := os.Getenv(sec.Name)
	switch env.Source(sec.Name) {
	case config.SourceEnvironment:
		if current != sec.Value {
			log.Warn().Str("var", sec.Name).Msgf(
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
	log.Warn().Str("var", sec.Name).Msgf(
		"the %s in %s is kept in %s as %s; the stored config holds only the reference",
		sec.Field, file, env.Path, sec.Name)
	return nil
}

// noteUnreadFeeds says why a feeds path in a file did nothing.
func noteUnreadFeeds(file config.File, path string) {
	if file.Ingest.Feeds != "" {
		log.Info().Msgf("ingest.feeds in %s is read only when the file is imported;"+
			" add feeds from the Sources page", path)
	}
}

// runLegacyDirectives applies the old-shape keys that are not stored: the feed
// seed file and the interest profile. They run once, with the import that saved
// a version, and a failure is only a warning: the config is already saved and a
// rerun would not retry.
func runLegacyDirectives(ctx context.Context, db *store.Store, legacy config.Legacy) {
	seedLegacyFeeds(ctx, db, legacy.FeedsPath)
	if err := profilemgr.New(db).BootstrapLegacy(ctx, legacy.ProfilePath); err != nil {
		log.Warn().
			Err(err).
			Str("profile", legacy.ProfilePath).
			Msg("importing the profile failed; select it in the web UI")
	}
}

// seedLegacyFeeds imports feeds the store has never seen from a legacy seed file.
//
// Every failure here is a warning, never an error: the config is already saved,
// and a missing, unreadable or half-broken seed file is a reason to say so and
// carry on.
func seedLegacyFeeds(ctx context.Context, db *store.Store, path string) {
	if path == "" {
		return
	}
	doc, found, err := config.ReadFeedsFile(path)
	if err != nil {
		log.Warn().Err(err).Str("feeds", path).Msg("reading the feed seed file failed")
		return
	}
	if !found {
		log.Warn().Str("feeds", path).Msg("the feed seed file named by ingest.feeds does not exist")
		return
	}
	result, err := db.SeedFeeds(ctx, *doc)
	if err != nil {
		log.Warn().Err(err).Str("feeds", path).Msg("seeding feeds failed")
		return
	}
	for _, warning := range result.Warnings {
		log.Warn().Str("feeds", path).Msg("skipped while seeding: " + warning)
	}
	log.Info().Str("feeds", path).Int("added", result.Added).Int("skipped", result.Skipped).
		Msg("seeded feeds from the legacy seed file")
}
