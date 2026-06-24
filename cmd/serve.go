package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/DanielBlei/ai-searcher/internal/config"
	"github.com/DanielBlei/ai-searcher/internal/server"
	"github.com/DanielBlei/ai-searcher/internal/store"
)

var serveAddr string

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Serve the items API over HTTP (JSON; a frontend is a later phase)",
	RunE:  runServe,
}

func init() {
	serveCmd.Flags().StringVar(&serveAddr, "addr", ":8080", "address to listen on")
	rootCmd.AddCommand(serveCmd)
}

// shutdownTimeout bounds how long runServe waits for in-flight requests to
// finish once the signal-aware context (see root.go's withSignalCancel) is
// cancelled.
const shutdownTimeout = 5 * time.Second

// readHeaderTimeout bounds how long a client may take to send its request
// headers, guarding against Slowloris-style connection holding that the
// zero-value http.Server otherwise leaves unbounded.
const readHeaderTimeout = 5 * time.Second

func runServe(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()

	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	log.Debug().Str("config", configPath).Str("addr", serveAddr).
		Str("list_since", cfg.ListSince.String()).Msg("config loaded")

	db, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	log.Debug().Str("db", cfg.DBPath).Msg("store opened")

	httpSrv := &http.Server{
		Addr:              serveAddr,
		Handler:           server.New(db, cfg, serveAddr).Routes(),
		ReadHeaderTimeout: readHeaderTimeout,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info().Msg(fmt.Sprintf("serving at http://localhost%s", serveAddr))
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		// ctx is the signal-aware context from root.go's withSignalCancel —
		// this fires on SIGINT (Ctrl+C) or SIGTERM.
		log.Info().Msg("shutdown signal received, shutting down gracefully...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		err := <-errCh
		log.Info().Msg("server stopped")
		return err
	case err := <-errCh:
		return err
	}
}
