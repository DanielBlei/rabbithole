// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/store"
)

const exampleConfigHint = "Every field and its default: docs/examples/config.yaml"

var (
	configExportOut  string
	configHistoryMax int
	sourcesExportOut string
)

var configGroupCmd = &cobra.Command{
	Use:   "config",
	Short: "Export, import, inspect and roll back the saved run config",
	Long: "The run config lives in the store as a numbered history; the highest version is the one in force " +
		"and no saved version means every setting uses its default. These commands work on that history. " +
		"A global --config only locates the store here and is never imported.",
}

var sourcesGroupCmd = &cobra.Command{
	Use:   "sources",
	Short: "Import and export the feed set as a feeds YAML file",
}

func init() {
	exportCmd := &cobra.Command{
		Use:   "export",
		Short: "Write the active config as YAML to stdout or a file",
		Long: "Writes the active config version as an import file. It holds only $RABBITHOLE_*_API_KEY " +
			"references, never a key. With no saved config the output is a comment saying so, which is " +
			"still a valid import file.",
		Args: cobra.NoArgs,
		RunE: runConfigExport,
	}
	exportCmd.Flags().StringVarP(&configExportOut, "output", "o", "", "write to this file instead of stdout")

	importCmd := &cobra.Command{
		Use:   "import <file>",
		Short: "Save a config file as the new active version",
		Long: "Saves the file as a new config version and makes it active. It never asks: the file wins, and " +
			"a version saved elsewhere that it replaces is named with the command to undo it. A file equal " +
			"to the active config changes nothing. The file's store block locates the store unless --db or " +
			dbEnv + " is given.",
		Args: cobra.ExactArgs(1),
		RunE: runConfigImport,
	}

	historyCmd := &cobra.Command{
		Use:   "history",
		Short: "List saved config versions, newest first",
		Args:  cobra.NoArgs,
		RunE:  runConfigHistory,
	}
	historyCmd.Flags().IntVar(&configHistoryMax, "limit", 20, "max versions to show (0 shows all)")

	rollbackCmd := &cobra.Command{
		Use:   "rollback <version>",
		Short: "Make a copy of an earlier config version the active one",
		Long: "Copies the version forward as a new latest version, so the history is kept and a rollback " +
			"can itself be rolled back.",
		Args: cobra.ExactArgs(1),
		RunE: runConfigRollback,
	}

	configGroupCmd.AddCommand(exportCmd, importCmd, historyCmd, rollbackCmd)

	feedsImportCmd := &cobra.Command{
		Use:   "import <file>",
		Short: "Add the feeds of a feeds YAML file the store has not seen",
		Long: "Adds feeds whose name and URL the store has never held; known feeds, deleted ones included, " +
			"are skipped, so running it twice adds nothing the second time.",
		Args: cobra.ExactArgs(1),
		RunE: runSourcesImport,
	}
	feedsExportCmd := &cobra.Command{
		Use:   "export",
		Short: "Write the feed set as a feeds YAML file to stdout or a file",
		Args:  cobra.NoArgs,
		RunE:  runSourcesExport,
	}
	feedsExportCmd.Flags().StringVarP(&sourcesExportOut, "output", "o", "", "write to this file instead of stdout")
	sourcesGroupCmd.AddCommand(feedsImportCmd, feedsExportCmd)

	rootCmd.AddCommand(configGroupCmd, sourcesGroupCmd)
}

// openReadOnly opens the store for a command that only reads: a SQLite store
// that is not there yields a storeless session instead of a new file, and a
// global --config only locates the store.
func openReadOnly(cmd *cobra.Command) (*session, error) {
	return openSession(cmd, sessionOpts{quiet: true, optional: true, noImport: true})
}

func runConfigExport(cmd *cobra.Command, _ []string) error {
	s, err := openReadOnly(cmd)
	if err != nil {
		return err
	}
	defer s.Close()
	var (
		cur  store.ConfigVersion
		have bool
	)
	if s.db != nil {
		if cur, have, err = s.db.ActiveConfig(cmd.Context()); err != nil {
			return err
		}
	}
	return writeOutput(cmd, configExportOut, renderConfigExport(cur, have))
}

// renderConfigExport is the import file for the active version. The header is a
// comment, so the output parses as a config either way.
func renderConfigExport(cur store.ConfigVersion, have bool) string {
	if !have {
		return "# rabbithole: no config is saved, so every setting uses its default. " + exampleConfigHint + "\n"
	}
	header := fmt.Sprintf("# rabbithole config version %d (%s, %s). %s\n",
		cur.Version, cur.Source, cur.CreatedAt.UTC().Format(time.RFC3339), exampleConfigHint)
	return header + cur.Content
}

func runConfigImport(cmd *cobra.Command, args []string) error {
	path := args[0]
	if configPath != "" && filepath.Clean(configPath) != filepath.Clean(path) {
		return fmt.Errorf("--config %s differs from the file to import (%s); pass only one", configPath, path)
	}
	// The file behaves like --config for bootstrap: its store block locates the
	// store unless --db or the environment names one.
	oldPath := configPath
	configPath = path
	defer func() { configPath = oldPath }()

	s, err := openSession(cmd, sessionOpts{quiet: true, noImport: true})
	if err != nil {
		return err
	}
	defer s.Close()
	ctx, out := cmd.Context(), cmd.OutOrStdout()
	res, err := importConfig(ctx, s.db, s.env, path, importOpts{})
	if err != nil {
		return err
	}
	switch {
	case !res.Saved:
		_, err = fmt.Fprintf(out, "config unchanged, version %d is active\n", res.Version)
	case res.Previous == 0:
		_, err = fmt.Fprintf(out, "config version %d saved from %s (first saved version)\n", res.Version, path)
	case res.Replaced != nil:
		_, err = fmt.Fprintf(
			out,
			"config version %d saved from %s, replacing version %d (%s); undo with: rabbithole config rollback %d\n",
			res.Version,
			path,
			res.Previous,
			replacedSummary(*res.Replaced),
			res.Previous,
		)
	default:
		_, err = fmt.Fprintf(
			out,
			"config version %d saved from %s (was version %d); undo with: rabbithole config rollback %d\n",
			res.Version,
			path,
			res.Previous,
			res.Previous,
		)
	}
	if err != nil {
		return err
	}
	return warnMissingKeys(ctx, s, cmd.ErrOrStderr())
}

func runConfigHistory(cmd *cobra.Command, _ []string) error {
	s, err := openReadOnly(cmd)
	if err != nil {
		return err
	}
	defer s.Close()
	var rows []store.ConfigVersion
	if s.db != nil {
		if rows, err = s.db.ConfigHistory(cmd.Context(), configHistoryMax); err != nil {
			return err
		}
	}
	out := cmd.OutOrStdout()
	if len(rows) == 0 {
		_, err = fmt.Fprintln(out, "no saved config; every setting uses its default")
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "VERSION\tSOURCE\tCREATED\tHASH")
	for i, v := range rows {
		marker := " "
		if i == 0 { // the newest row is the active one
			marker = "*"
		}
		_, _ = fmt.Fprintf(tw, "%s%d\t%s\t%s\t%s\n", marker, v.Version, v.Source,
			v.CreatedAt.Local().Format("2006-01-02 15:04:05"), shortHash(v.ContentHash))
	}
	return tw.Flush()
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

func runConfigRollback(cmd *cobra.Command, args []string) error {
	n, err := strconv.Atoi(args[0])
	if err != nil || n < 1 {
		return fmt.Errorf("version must be a positive number, got %q", args[0])
	}
	s, err := openSession(cmd, sessionOpts{quiet: true, mustExist: true, noImport: true})
	if err != nil {
		return err
	}
	defer s.Close()
	ctx, out := cmd.Context(), cmd.OutOrStdout()
	target, err := s.db.ConfigVersion(ctx, n)
	if errors.Is(err, store.ErrConfigVersionNotFound) {
		return fmt.Errorf("no config version %d; see rabbithole config history", n)
	}
	if err != nil {
		return err
	}
	// An unreadable row must not become the active config.
	if _, err := config.ParseStored([]byte(target.Content), target.SchemaVersion); err != nil {
		return fmt.Errorf("config version %d cannot be read, so it cannot be restored: %w", n, err)
	}
	cur, have, err := s.db.ActiveConfig(ctx)
	if err != nil {
		return err
	}
	if have && cur.ContentHash == target.ContentHash {
		_, err = fmt.Fprintf(
			out,
			"config version %d is already active (version %d has the same content); nothing saved\n",
			cur.Version,
			n,
		)
		return err
	}
	saved, err := s.db.RollbackConfig(ctx, n, cur.Version)
	if errors.Is(err, store.ErrConfigConflict) {
		return errors.New("the stored config changed meanwhile; run the command again")
	}
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "config version %d is now active (copy of version %d); the next ingest run uses it\n",
		saved.Version, n); err != nil {
		return err
	}
	return warnMissingKeys(ctx, s, cmd.ErrOrStderr())
}

// warnMissingKeys names, never shows, the API key variables the active config
// references that nothing sets.
func warnMissingKeys(ctx context.Context, s *session, w io.Writer) error {
	cfg, _, err := s.src.Active(ctx)
	if err != nil {
		return nil // the command already reported what it did; an unreadable row is surfaced elsewhere
	}
	for _, ref := range []string{cfg.Inference.APIKey, cfg.Inference.Summary.APIKey} {
		name, ok := config.RefName(ref)
		if !ok {
			continue
		}
		if s.env.Source(name) == config.SourceMissing {
			if _, err := fmt.Fprintf(
				w,
				"warning: the config uses $%s, which is not set; set it in the environment or %s\n",
				name,
				s.env.Path,
			); err != nil {
				return err
			}
		}
	}
	return nil
}

func runSourcesImport(cmd *cobra.Command, args []string) error {
	raw, err := os.ReadFile(args[0])
	if err != nil {
		return fmt.Errorf("read feeds file: %w", err)
	}
	doc, err := config.ParseFeeds(raw)
	if err != nil {
		return fmt.Errorf("%s: %w", args[0], err)
	}
	s, err := openSession(cmd, sessionOpts{quiet: true, noImport: true})
	if err != nil {
		return err
	}
	defer s.Close()
	res, err := s.db.SeedFeeds(cmd.Context(), *doc)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if _, err := fmt.Fprintf(out, "added %d feed(s), skipped %d already known (including deleted ones)\n",
		res.Added, res.Skipped); err != nil {
		return err
	}
	for _, w := range res.Warnings {
		if _, err := fmt.Fprintf(out, "warning: %s\n", w); err != nil {
			return err
		}
	}
	return nil
}

func runSourcesExport(cmd *cobra.Command, _ []string) error {
	s, err := openReadOnly(cmd)
	if err != nil {
		return err
	}
	defer s.Close()
	var doc config.FeedsDoc
	if s.db != nil {
		if doc, err = s.db.FeedsDoc(cmd.Context()); err != nil {
			return err
		}
	}
	text, err := config.MarshalFeeds(doc)
	if err != nil {
		return err
	}
	return writeOutput(cmd, sourcesExportOut, text)
}

// writeOutput prints text to stdout, or writes it to path through a temp file
// and a rename so a reader never sees half a file.
func writeOutput(cmd *cobra.Command, path, text string) error {
	if path == "" {
		_, err := io.WriteString(cmd.OutOrStdout(), text)
		return err
	}
	return writeFileAtomic(path, []byte(text), 0o644)
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".rabbithole-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	fail := func(err error) error {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		return fail(err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}
