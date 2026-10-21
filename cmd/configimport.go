// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/profilemgr"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// importOpts carries what an import needs from its caller.
type importOpts struct {
	// beforeSave runs before the save; tests use it to make the save conflict.
	beforeSave func()
	// onStart marks the implicit --config re-apply, which does nothing while the
	// file is unchanged since it was last applied; `config import` leaves it off.
	onStart bool
}

// importOutcome reports what an import did.
type importOutcome = profilemgr.ImportOutcome

// replacedSummary names a displaced version: its number, where it came from and
// when it was saved.
func replacedSummary(v store.ConfigVersion) string {
	return fmt.Sprintf("%s, saved %s", describeConfigSource(v.Source), v.CreatedAt.Local().Format("15:04"))
}

// importConfig reads the config file at path and applies it through the import
// core the settings page shares (profilemgr.ApplyImport): the same content as the
// active version is a no-op, anything else is saved as a new version. The file
// wins; when it replaces a version that came from somewhere else, that is
// warned about with the undo command. With onStart an unchanged file is skipped
// altogether.
func importConfig(
	ctx context.Context, db *store.Store, env *config.EnvFile, path string, opts importOpts,
) (importOutcome, error) {
	file, err := config.ReadFile(path)
	if err != nil {
		return importOutcome{}, err
	}
	// A system_prompt that names a file is read once, relative to the config file.
	if err := file.InlineSystemPrompt(filepath.Dir(path)); err != nil {
		return importOutcome{}, fmt.Errorf("%s: %w", path, err)
	}
	p, err := profilemgr.Prepare(file, path, log)
	if err != nil {
		return importOutcome{}, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return importOutcome{}, fmt.Errorf("resolve config path: %w", err)
	}
	// A symlink or another spelling of the same file owns the config too.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	mode := profilemgr.ImportCommand
	if opts.onStart {
		mode = profilemgr.ImportOnStart
	}
	return profilemgr.ApplyImport(ctx, db, env, p, path, store.ConfigSourceImport+abs,
		profilemgr.ImportOpts{BeforeSave: opts.beforeSave, Log: log, Mode: mode})
}
