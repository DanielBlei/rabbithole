package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/ingest"
	"github.com/DanielBlei/rabbithole/internal/server"
	"github.com/DanielBlei/rabbithole/internal/store"
)

var serveAddr string

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Serve the items API over HTTP (JSON; a frontend is a later phase)",
	RunE:  runServe,
}

func init() {
	serveCmd.Flags().StringVar(&serveAddr, "addr", "127.0.0.1:8080", "address to listen on")
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
	log.Debug().Str("config", configPath).Str("addr", serveAddr).Msg("config loaded")

	db, err := store.Open(cfg.Store.DBPath)
	if err != nil {
		return err
	}
	defer func() {
		if err := db.Close(); err != nil {
			log.Warn().Err(err).Msg("db close failed")
		}
	}()
	log.Debug().Str("db", cfg.Store.DBPath).Msg("store opened")

	// The ingest manager owns web-triggered (and later scheduled) ingest runs:
	// single-flight, background context, run history. It also flips any history
	// row a crashed process left as 'running' to an error before serving.
	mgr, err := ingest.NewManager(db, cfg, log.GetLevel())
	if err != nil {
		return err
	}

	httpSrv := &http.Server{
		Addr:              serveAddr,
		Handler:           server.New(db, cfg, serveAddr, configPath, mgr).Routes(),
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

		// Drain any in-flight ingest run before the deferred db.Close() runs,
		// so it never writes through a closed *sql.DB. Its own timeout budget,
		// separate from the HTTP drain above, so a slow HTTP shutdown doesn't
		// shortchange the run's chance to wind down cleanly.
		mgrShutdownCtx, mgrCancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer mgrCancel()
		mgr.Shutdown(mgrShutdownCtx)

		err := <-errCh
		log.Info().Msg("server stopped")
		return err
	case err := <-errCh:
		return err
	}
}
