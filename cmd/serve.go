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
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/httpclient"
	"github.com/DanielBlei/rabbithole/internal/ingest"
	"github.com/DanielBlei/rabbithole/internal/library"
	"github.com/DanielBlei/rabbithole/internal/server"
	"github.com/DanielBlei/rabbithole/internal/store"
)

var (
	serveAddr         string
	serveTLSCert      string
	serveTLSKey       string
	serveInsecureHTTP bool
	serveDev          bool
	serveNoEnvWrite   bool
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
	serveCmd.Flags().BoolVar(&serveNoEnvWrite, "no-env-write", false,
		"never write a pasted API key to .env: hides the paste action in Settings and refuses it")
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

	// `serve` is the process that owns the store: it reconciles interrupted ingest
	// runs at startup and holds the sessions, so two of them reaching the same
	// database would undo each other. Ask for the single-writer guard, which
	// Postgres enforces and SQLite has nothing to enforce it with.
	s, err := openSession(cmd, sessionOpts{exclusive: true})
	if err != nil {
		return err
	}
	defer s.Close()
	db, src := s.db, s.src

	_, version, err := src.Active(ctx)
	if err != nil {
		return err
	}
	logActiveConfig(ctx, db, version)
	logManagedConfig(configPath)

	// Items carry their feed's tags, which the feed page filters on. Syncing at
	// startup means a tag edit reaches items recorded before it, rather than
	// waiting for the next ingest run to notice.
	configured, err := ingest.ResolveFeeds(ctx, db)
	if err != nil {
		return err
	}
	if err := db.SyncSourceTags(ctx, ingest.ConfiguredTags(configured)); err != nil {
		log.Warn().Err(err).Msg("syncing feed tags failed")
	}

	// The ingest manager owns web-triggered (and later scheduled) ingest runs:
	// single-flight, background context, run history. It also flips any history
	// row a crashed process left as 'running' to an error before serving.
	mgr, err := ingest.NewManager(db, src, log.GetLevel())
	if err != nil {
		return err
	}
	mgr.SetClient(feeds.NewClient(buildVersion()))

	srv := server.New(db, src, serveAddr, mgr, log)
	srv.TrustProxies(proxies)
	srv.Dev(serveDev)
	srv.ManagedBy(configPath)
	srv.EnvFile(s.env, serveNoEnvWrite)
	srv.StoreInfo(s.storeInfo)
	srv.Library(newLibraryLoader(buildVersion()))
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

// libraryTimeout bounds one request to the library host.
const libraryTimeout = 5 * time.Second

// libraryBase reads RABBITHOLE_LIBRARY_URL: empty is the default, "off" turns the
// library off, anything else must be an http(s) URL with a host. custom is true
// for a mirror other than the default. A bad value is an error and the default
// is returned with it.
func libraryBase(raw string) (base string, custom bool, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return library.DefaultBaseURL, false, nil
	}
	if strings.EqualFold(raw, "off") {
		return "off", false, nil
	}
	u, perr := url.Parse(raw)
	if perr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return library.DefaultBaseURL, false, fmt.Errorf(
			"%s must be off or an http(s) URL with a host, got %q", config.LibraryURLEnv, raw)
	}
	same := strings.EqualFold(strings.TrimRight(raw, "/"), strings.TrimRight(library.DefaultBaseURL, "/"))
	return raw, !same, nil
}

// newLibraryLoader builds the loader of the source library. Nothing is fetched
// until the Sources page asks. The default host is reached on 80 and 443 only;
// a mirror chosen through the environment may use any port.
func newLibraryLoader(version string) *library.Loader {
	base, custom, err := libraryBase(os.Getenv(config.LibraryURLEnv))
	if err != nil {
		log.Warn().Err(err).Msg("ignoring the library URL, using the default")
	}
	client := httpclient.NewSafe(httpclient.Options{
		Timeout:      libraryTimeout,
		MaxRedirects: 3,
		UserAgent:    "rabbithole/" + version + " (+https://github.com/DanielBlei/rabbithole)",
		AllowPrivate: httpclient.AllowPrivateFromEnv(),
		AnyPort:      custom,
		Proxy:        http.ProxyFromEnvironment,
	})
	return library.NewLoader(library.Options{
		Client:  client,
		BaseURL: base,
		Log:     log,
	})
}

// logActiveConfig says which config version is in force, or that the defaults are.
// The version comes from the snapshot serve already took; the row only adds how
// and when it was saved, and is left out if the config moved on in between.
func logActiveConfig(ctx context.Context, db *store.Store, version int) {
	if version == 0 {
		log.Info().Msg("no config saved in the store, using the defaults")
		return
	}
	v, ok, err := db.ActiveConfig(ctx)
	if err != nil || !ok || v.Version != version {
		log.Info().Int("version", version).Msg("config loaded from the store")
		return
	}
	log.Info().Int("version", v.Version).
		Str("saved", v.CreatedAt.Local().Format("2006-01-02 15:04")).
		Str("by", describeConfigSource(v.Source)).
		Msg("config loaded from the store")
}

// logManagedConfig says that a --config file is applied when it changes.
// Without one there is nothing to say.
func logManagedConfig(path string) {
	if path == "" {
		return
	}
	log.Info().Msgf("config imported from %s; edits in Settings are kept until the file changes", path)
}

// describeConfigSource turns a stored source into how the version was saved,
// with a path under the working directory shown relative to it.
func describeConfigSource(src string) string {
	switch {
	case src == store.ConfigSourceUI:
		return "settings UI"
	case strings.HasPrefix(src, store.ConfigSourceRollback):
		return "rollback to version " + strings.TrimPrefix(src, store.ConfigSourceRollback)
	case strings.HasPrefix(src, store.ConfigSourceImport):
		return "import of " + displayPath(strings.TrimPrefix(src, store.ConfigSourceImport))
	}
	return src
}

// displayPath shortens an absolute path under the working directory to a
// relative one; anything else is returned as is.
func displayPath(p string) string {
	wd, err := os.Getwd()
	if err != nil {
		return p
	}
	if rel, err := filepath.Rel(wd, p); err == nil && !strings.HasPrefix(rel, "..") {
		return "./" + rel
	}
	return p
}
