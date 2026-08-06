// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
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
	Short: "Serve the web UI and the items API over HTTP",
	RunE:  runServe,
}

func init() {
	serveCmd.Flags().StringVar(&serveAddr, "addr", "127.0.0.1:8080", "address to listen on")
	rootCmd.AddCommand(serveCmd)
}

// Time limits for one connection, plus how long shutdown waits.
// Without these, a slow or stuck client can hold a connection open forever.
const (
	readHeaderTimeout = 5 * time.Second   // time to send the request headers
	readTimeout       = 15 * time.Second  // time to send the headers plus the body
	writeTimeout      = 30 * time.Second  // nothing streams, so this covers the slowest full page
	idleTimeout       = 120 * time.Second // how long an open but unused connection is kept
	shutdownTimeout   = 5 * time.Second   // how long we wait for in-flight requests on Ctrl+C
)

// browsableAddr turns a listen address into one you can paste in a browser,
// naming the host when the address only gives a port (":8080").
func browsableAddr(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "localhost" + addr
	}
	return addr
}

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

	// Feeds live in the store; the feeds file only seeds ones it has never seen.
	// Running this every boot means adding an entry to the file is enough to
	// pick it up, while anything you changed on the Sources page — disabled,
	// retuned, deleted — is left exactly as you left it.
	seedFeeds(ctx, db, cfg)

	// Items carry their feed's tags, which the feed page filters on. Syncing at
	// startup means a tag edit reaches items recorded before it, rather than
	// waiting for the next ingest run to notice.
	configured, err := ingest.ResolveFeeds(ctx, db, cfg)
	if err != nil {
		return err
	}
	if err := db.SyncSourceTags(ctx, ingest.ConfiguredTags(configured)); err != nil {
		log.Warn().Err(err).Msg("syncing feed tags failed")
	}

	// The ingest manager owns web-triggered (and later scheduled) ingest runs:
	// single-flight, background context, run history. It also flips any history
	// row a crashed process left as 'running' to an error before serving.
	mgr, err := ingest.NewManager(db, cfg, log.GetLevel())
	if err != nil {
		return err
	}

	srv := server.New(db, cfg, serveAddr, configPath, mgr, log)
	httpSrv := &http.Server{
		Addr:              serveAddr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info().Msg("serving at http://" + browsableAddr(serveAddr))
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
		// Fail readiness first, so anything routing to us can route away while
		// the drain below finishes the requests already in flight.
		srv.Drain()
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

// seedFeeds imports feeds the store has never seen from the seed file.
//
// Every failure here is a warning, never a boot failure: the store already
// holds the feed set, and a missing, unreadable or half-broken seed file is a
// reason to say so and carry on rather than to refuse to serve.
func seedFeeds(ctx context.Context, db *store.Store, cfg *config.Config) {
	path, _ := cfg.FeedsFilePath(configPath)
	doc, found, err := config.ReadFeedsFile(path)
	if err != nil {
		log.Warn().Err(err).Str("feeds", path).Msg("reading the feed seed file failed")
		return
	}
	if !found {
		log.Info().Str("feeds", path).Msg("no feed seed file; feeds come from the store")
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
	// Logged whether or not anything was added from a seed file
	log.Debug().Str("feeds", path).Int("added", result.Added).Int("skipped", result.Skipped).
		Msg("seeded feeds from the seed file")
}
