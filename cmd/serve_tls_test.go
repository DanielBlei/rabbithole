// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/ingest"
	"github.com/DanielBlei/rabbithole/internal/server"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// writeTestCert writes a self-signed certificate for 127.0.0.1 and localhost
// into a new directory, and returns its files and a pool that trusts it.
func writeTestCert(t *testing.T) (certFile, keyFile string, pool *x509.CertPool) {
	t.Helper()
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	return certFile, keyFile, writeTestCertTo(t, certFile, keyFile, 1)
}

// writeTestCertTo writes a self-signed certificate with the given serial to the
// two paths, replacing whatever is there, and returns a pool that trusts it.
func writeTestCertTo(t *testing.T, certFile, keyFile string, serial int64) *x509.CertPool {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	writePEM(t, certFile, "CERTIFICATE", der)
	writePEM(t, keyFile, "PRIVATE KEY", keyDER)

	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(parsed)
	return pool
}

func writePEM(t *testing.T, path, kind string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// serveTLSForTest runs the real route table the way serve does, from the same
// certificate loading and serving path, on a free loopback port. The store is
// fresh, so the login gate is in its default-login state.
func serveTLSForTest(t *testing.T, certFile, keyFile string) string {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	think := false
	cfg := &config.Config{}
	cfg.Inference.Think = &think
	mgr, err := ingest.NewManager(db, cfg, zerolog.InfoLevel)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	tlsConfig, err := loadTLSConfig(certFile, keyFile)
	if err != nil {
		t.Fatalf("loadTLSConfig: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	srv := newHTTPServer(ln.Addr().String(),
		server.New(db, cfg, ln.Addr().String(), "config.yaml", mgr, zerolog.Nop()).Routes(), tlsConfig)
	done := make(chan error, 1)
	go func() { done <- serveOn(srv, ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		if err := <-done; err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("serveOn: %v", err)
		}
	})
	return ln.Addr().String()
}

// httpsClient trusts pool and hands redirects back rather than following them.
func httpsClient(pool *x509.CertPool, maxVersion uint16) *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MaxVersion: maxVersion},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func TestServeOverTLS(t *testing.T) {
	certFile, keyFile, pool := writeTestCert(t)
	addr := serveTLSForTest(t, certFile, keyFile)
	client := httpsClient(pool, 0)

	resp, err := client.Get("https://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz over TLS: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz = %d, want 200", resp.StatusCode)
	}
	if resp.TLS == nil || resp.TLS.Version < tls.VersionTLS12 || len(resp.TLS.VerifiedChains) == 0 {
		t.Fatalf("connection state = %+v, want a verified TLS 1.2+ connection", resp.TLS)
	}

	// Claiming the instance is the first run's one way in, and the session it
	// hands out is marked Secure over TLS.
	form := url.Values{
		"action":   {"password"},
		"username": {"admin"},
		"password": {"correct horse"},
		"confirm":  {"correct horse"},
	}
	resp, err = client.PostForm("https://"+addr+"/setup", form)
	if err != nil {
		t.Fatalf("POST /setup over TLS: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("setup = %d to %q, want 303 to /", resp.StatusCode, resp.Header.Get("Location"))
	}
	var session *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "rh_session" {
			session = c
		}
	}
	if session == nil || !session.Secure || !session.HttpOnly {
		t.Fatalf("session cookie = %+v, want Secure and HttpOnly", session)
	}

	req, _ := http.NewRequest(http.MethodGet, "https://"+addr+"/feed", nil)
	req.AddCookie(session)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("GET /feed over TLS: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /feed with the session = %d, want 200", resp.StatusCode)
	}
}

func TestServeOverTLSRefusesWhatItShould(t *testing.T) {
	certFile, keyFile, pool := writeTestCert(t)
	addr := serveTLSForTest(t, certFile, keyFile)

	if _, err := httpsClient(pool, tls.VersionTLS11).Get("https://" + addr + "/healthz"); err == nil {
		t.Error("a TLS 1.1 client got through; the floor is 1.2")
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("plain HTTP to the TLS port: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("plain HTTP to the TLS port = %d, want 400", resp.StatusCode)
	}
}

func TestServeOnWithoutTLSIsPlainHTTP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	srv := newHTTPServer(ln.Addr().String(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), nil)
	go func() { _ = serveOn(srv, ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Get("http://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || resp.TLS != nil {
		t.Errorf("plain serve = %d, TLS %v", resp.StatusCode, resp.TLS)
	}
}

func TestLoadTLSConfig(t *testing.T) {
	if cfg, err := loadTLSConfig("", ""); cfg != nil || err != nil {
		t.Fatalf("no certificate = %v, %v; want plain HTTP (nil, nil)", cfg, err)
	}

	certFile, keyFile, _ := writeTestCert(t)
	cfg, err := loadTLSConfig(certFile, keyFile)
	if err != nil {
		t.Fatalf("loadTLSConfig: %v", err)
	}
	if cfg.MinVersion != tls.VersionTLS12 || cfg.GetCertificate == nil {
		t.Errorf("config = min %x, GetCertificate set %v; want TLS 1.2 and the reloader",
			cfg.MinVersion, cfg.GetCertificate != nil)
	}

	_, otherKey, _ := writeTestCert(t)
	tests := map[string][2]string{
		"missing cert":    {filepath.Join(t.TempDir(), "none.pem"), keyFile},
		"missing key":     {certFile, filepath.Join(t.TempDir(), "none.pem")},
		"mismatched pair": {certFile, otherKey},
		"key as cert":     {keyFile, keyFile},
	}
	for name, files := range tests {
		_, err := loadTLSConfig(files[0], files[1])
		if err == nil || !strings.Contains(err.Error(), "loading TLS certificate") {
			t.Errorf("%s: err = %v, want a certificate load error", name, err)
		}
	}
}

// A renewed pair on disk is picked up at the next check, with no restart; a
// pair that fails to load leaves the last good one serving.
func TestCertReloadPicksUpARenewal(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	writeTestCertTo(t, certFile, keyFile, 1)
	r, err := newCertReloader(certFile, keyFile)
	if err != nil {
		t.Fatalf("newCertReloader: %v", err)
	}
	now := time.Now()
	r.now = func() time.Time { return now }
	serial := func() int64 {
		t.Helper()
		c, err := r.getCertificate(nil)
		if err != nil {
			t.Fatalf("getCertificate: %v", err)
		}
		leaf, err := x509.ParseCertificate(c.Certificate[0])
		if err != nil {
			t.Fatalf("ParseCertificate: %v", err)
		}
		return leaf.SerialNumber.Int64()
	}
	touch := func() {
		t.Helper()
		later := now.Add(time.Hour)
		for _, f := range []string{certFile, keyFile} {
			if err := os.Chtimes(f, later, later); err != nil {
				t.Fatalf("Chtimes: %v", err)
			}
		}
	}

	writeTestCertTo(t, certFile, keyFile, 2)
	touch()
	if got := serial(); got != 1 {
		t.Fatalf("serial before the check interval = %d, want the loaded 1", got)
	}
	now = now.Add(certCheckEvery)
	if got := serial(); got != 2 {
		t.Fatalf("serial after a renewal = %d, want 2", got)
	}

	if err := os.WriteFile(certFile, []byte("not a certificate"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	now = now.Add(2 * time.Hour)
	touch()
	now = now.Add(certCheckEvery)
	if got := serial(); got != 2 {
		t.Fatalf("serial after a broken renewal = %d, want the last good 2", got)
	}
}
