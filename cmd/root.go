// Package cmd wires the rabbithole CLI.
package cmd

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"

	"github.com/DanielBlei/rabbithole/internal/logger"
)

var (
	configPath string
	debug      bool
	trace      bool
	log        zerolog.Logger
)

const defaultConfigPath = "./configs/config.yaml"

var rootCmd = &cobra.Command{
	Use:   "rabbithole",
	Short: "Personal RSS reading assistant: ranks feeds against your interests",
	Long: "The Rabbit Hole fetches your RSS/Atom feeds, scores each new item against an " +
		"interest profile using an LLM, and writes a daily markdown digest of what to read.",
	Version:       "0.1.0",
	SilenceErrors: true,
	SilenceUsage:  true,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		log = logger.New(debug, trace)
		return nil
	},
}

func init() {
	rootCmd.PersistentFlags().StringVar(&configPath, "config", defaultConfigPath, "path to config YAML")
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
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
