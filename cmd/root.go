// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

// Package cmd wires the rabbithole CLI.
package cmd

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"

	buildinfo "runtime/debug"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"

	"github.com/DanielBlei/rabbithole/internal/logger"
)

var (
	configPath string
	envFile    string
	dbFlag     string
	dbMaxConns int
	dbLifetime string
	debug      bool
	trace      bool
	log        zerolog.Logger
)

// version is stamped in at build time.
var version = "dev"

// buildVersion falls back to the module version for `go install` builds.
func buildVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := buildinfo.ReadBuildInfo(); ok {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return version
}

var rootCmd = &cobra.Command{
	Use:   "rabbithole",
	Short: "Ranks what to read from your feeds and paper searches, against your interests",
	Long: "The Rabbit Hole pulls new items from your sources (RSS and Atom feeds, and saved " +
		"searches on arXiv, Crossref and Semantic Scholar), scores each against your interest " +
		"profile with an LLM or the model-free heuristic scorer, and ranks them in a web UI. " +
		"Run `serve` for the UI; `ingest` runs one cycle from the terminal.",
	Version:       buildVersion(),
	SilenceErrors: true,
	SilenceUsage:  true,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		invoked = cmd.CommandPath()
		log = logger.New(debug, trace)
		zerolog.DefaultContextLogger = &log
		return nil
	},
}

func init() {
	rootCmd.PersistentFlags().StringVar(&configPath, "config", "",
		"config YAML applied to the store on every start; the file wins over changes made elsewhere (optional)")
	rootCmd.PersistentFlags().StringVar(&envFile, "env-file", defaultEnvFile,
		"env file read for RABBITHOLE_* variables the environment does not set")
	rootCmd.PersistentFlags().StringVar(&dbFlag, "db", "",
		"store location: a SQLite path or a postgres:// URL (env: "+dbEnv+"); default: a file in your user data dir")
	rootCmd.PersistentFlags().IntVar(&dbMaxConns, "db-max-conns", 0, "Postgres only: cap the connection pool")
	rootCmd.PersistentFlags().StringVar(&dbLifetime, "db-conn-max-lifetime", "",
		"Postgres only: how long one pooled connection may live, e.g. 30m")
	rootCmd.PersistentFlags().BoolVar(&debug, "debug", false, "enable debug logging")
	rootCmd.PersistentFlags().
		BoolVar(&trace, "trace", false, "enable trace logging (raw model prompts/responses, implies --debug)")
	rootCmd.AddCommand(ingestCmd)
}

// withSignalCancel returns a context cancelled on SIGINT/SIGTERM.
func withSignalCancel(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-ctx.Done():
		}
		signal.Stop(sigCh)
	}()
	return ctx, cancel
}

// Execute runs the root command with a signal-aware context.
func Execute() error {
	ctx, cancel := withSignalCancel(context.Background())
	defer cancel()
	err := rootCmd.ExecuteContext(ctx)
	if errors.Is(err, errInterrupted) {
		os.Exit(130) //nolint:gocritic // the conventional SIGINT status; cancel has nothing left to release
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, errNothingCreated) {
		return nil
	}
	return err
}
