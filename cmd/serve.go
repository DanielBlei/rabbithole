// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/ingest"
	"github.com/DanielBlei/rabbithole/internal/profilemgr"
	"github.com/DanielBlei/rabbithole/internal/server"
	"github.com/DanielBlei/rabbithole/internal/store"
)

var (
	serveAddr         string
	serveTLSCert      string
	serveTLSKey       string
	serveInsecureHTTP bool
	serveDev          bool
	serveProxies      []string
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Serve the web UI and the items API over HTTP",
	RunE:  runServe,
}

func init() {
	serveCmd.Flags().StringVar(&serveAddr, "addr", "127.0.0.1:8080", "address to listen on")
	serveCmd.Flags().
		StringVar(&serveTLSCert, "tls-cert", "", "certificate file (PEM) to serve HTTPS with, alongside --tls-key")
	serveCmd.Flags().StringVar(&serveTLSKey, "tls-key", "", "private key file (PEM) for --tls-cert")
	serveCmd.Flags().BoolVar(&serveInsecureHTTP, "insecure-http", false,
		"allow plain HTTP on an address other machines can reach, for when a TLS proxy sits in front")
	serveCmd.Flags().StringSliceVar(&serveProxies, "trusted-proxies", []string{"127.0.0.0/8", "::1/128"},
		"networks whose X-Forwarded-For and X-Forwarded-Proto are believed; empty trusts none")
	serveCmd.Flags().BoolVar(&serveDev, "dev", false,
		"serve the CSS, scripts and fonts no-cache, so an edit shows on the next reload")
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

// checkListen refuses plain HTTP on an address other machines can reach, where
// the login would cross the network readable. HTTPS, an explicit opt-out for a
// TLS proxy in front, or a loopback address all pass.
func checkListen(addr, cert, key string, insecureHTTP bool) error {
	if (cert == "") != (key == "") {
		return errors.New("--tls-cert and --tls-key must be given together")
	}
	if cert != "" || insecureHTTP {
		return nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("--addr %q: %w", addr, err)
	}
	if isLoopback(host) {
		return nil
	}
	return fmt.Errorf("refusing plain HTTP on %s, which other machines can reach: the login would cross "+
		"the network unencrypted. Serve HTTPS with --tls-cert and --tls-key, put a TLS proxy in front and "+
		"pass --insecure-http, or listen on 127.0.0.1", addr)
}

// isLoopback reports a host only this machine can reach. An empty host (":8080")
// is every interface, so it is not.
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// parseProxies reads --trusted-proxies: CIDR networks or bare addresses.
func parseProxies(list []string) ([]netip.Prefix, error) {
	nets := make([]netip.Prefix, 0, len(list))
	for _, entry := range list {
		if entry = strings.TrimSpace(entry); entry == "" {
			continue
		}
		if p, err := netip.ParsePrefix(entry); err == nil {
			nets = append(nets, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(entry)
		if err != nil {
			return nil, fmt.Errorf("--trusted-proxies: %q is neither a network nor an address", entry)
		}
		nets = append(nets, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
	}
	return nets, nil
}

// loadTLSConfig reads the certificate pair serve presents, or returns nil for
// plain HTTP. It runs before anything else starts, so a bad file fails the
// command rather than a goroutine.
func loadTLSConfig(cert, key string) (*tls.Config, error) {
	if cert == "" {
		return nil, nil
	}
	r, err := newCertReloader(cert, key)
	if err != nil {
		return nil, err
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: r.getCertificate}, nil
}

// certCheckEvery is how often the certificate files are looked at for a renewal.
const certCheckEvery = 30 * time.Second

// certReloader serves the certificate pair from disk and picks up a renewed one
// when either file changes, so a renewal needs no restart and logs no one out.
// A pair that fails to load leaves the last good one in service.
type certReloader struct {
	certFile, keyFile string
	now               func() time.Time

	mu      sync.Mutex
	cert    *tls.Certificate
	mod     time.Time // the newer of the two files' modification times, as loaded
	checked time.Time
}

func newCertReloader(certFile, keyFile string) (*certReloader, error) {
	r := &certReloader{certFile: certFile, keyFile: keyFile, now: time.Now}
	if err := r.load(); err != nil {
		return nil, err
	}
	r.checked = r.now()
	return r, nil
}

func (r *certReloader) load() error {
	mod, err := r.modTime()
	if err != nil {
		return fmt.Errorf("loading TLS certificate: %w", err)
	}
	pair, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return fmt.Errorf("loading TLS certificate: %w", err)
	}
	r.cert, r.mod = &pair, mod
	return nil
}

func (r *certReloader) modTime() (time.Time, error) {
	var newest time.Time
	for _, f := range []string{r.certFile, r.keyFile} {
		info, err := os.Stat(f)
		if err != nil {
			return time.Time{}, err
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	return newest, nil
}

func (r *certReloader) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if now := r.now(); now.Sub(r.checked) >= certCheckEvery {
		r.checked = now
		if mod, err := r.modTime(); err == nil && !mod.Equal(r.mod) {
			if err := r.load(); err != nil {
				log.Warn().Err(err).Msg("TLS certificate changed but did not load; still serving the previous one")
			} else {
				log.Info().Msg("TLS certificate reloaded")
			}
		}
	}
	return r.cert, nil
}

// newHTTPServer is the server serve runs: h behind the connection time limits,
// over TLS when tlsConfig is set.
func newHTTPServer(addr string, h http.Handler, tlsConfig *tls.Config) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		TLSConfig:         tlsConfig,
	}
}

// serveOn serves srv on ln, over TLS when srv carries a TLS config.
func serveOn(srv *http.Server, ln net.Listener) error {
	if srv.TLSConfig != nil {
		return srv.ServeTLS(ln, "", "")
	}
	return srv.Serve(ln)
}

func runServe(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()

	if err := checkListen(serveAddr, serveTLSCert, serveTLSKey, serveInsecureHTTP); err != nil {
		return err
	}
	tlsConfig, err := loadTLSConfig(serveTLSCert, serveTLSKey)
	if err != nil {
		return err
	}
	proxies, err := parseProxies(serveProxies)
	if err != nil {
		return err
	}

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

	// A legacy profile path is a one-time bootstrap source. Validation still
	// runs on every boot, but an existing web selection is never overwritten.
	if err := profilemgr.New(db).BootstrapLegacy(ctx, cfg); err != nil {
		return err
	}

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
	srv.TrustProxies(proxies)
	srv.Dev(serveDev)
	httpSrv := newHTTPServer(serveAddr, srv.Routes(), tlsConfig)
	// Bound here rather than in the goroutine, so a taken port fails the command.
	ln, err := net.Listen("tcp", serveAddr)
	if err != nil {
		return err
	}

	scheme := "http"
	if tlsConfig != nil {
		scheme = "https"
	} else if host, _, _ := net.SplitHostPort(serveAddr); !isLoopback(host) {
		log.Warn().Msg("serving plain HTTP on a reachable address: logins stay private only if a TLS proxy is in front")
	}
	errCh := make(chan error, 1)
	go func() {
		log.Info().Msg("serving at " + scheme + "://" + browsableAddr(serveAddr))
		if err := serveOn(httpSrv, ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
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
