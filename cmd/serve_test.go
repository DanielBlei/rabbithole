// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/httpclient"
	"github.com/DanielBlei/rabbithole/internal/ingest"
	"github.com/DanielBlei/rabbithole/internal/library"
	"github.com/DanielBlei/rabbithole/internal/store"
)

func TestCheckListen(t *testing.T) {
	tests := []struct {
		name      string
		addr      string
		cert, key string
		insecure  bool
		wantErr   bool
	}{
		{"loopback v4", "127.0.0.1:8080", "", "", false, false},
		{"loopback range", "127.0.0.2:8080", "", "", false, false},
		{"loopback v6", "[::1]:8080", "", "", false, false},
		{"localhost", "localhost:8080", "", "", false, false},
		{"every interface", ":8080", "", "", false, true},
		{"unspecified v4", "0.0.0.0:8080", "", "", false, true},
		{"lan address", "192.168.1.20:8080", "", "", false, true},
		{"hostname", "rabbithole.lan:8080", "", "", false, true},
		{"exposed with tls", ":8443", "c.pem", "k.pem", false, false},
		{"exposed behind a proxy", ":8080", "", "", true, false},
		{"cert without key", "127.0.0.1:8080", "c.pem", "", false, true},
		{"key without cert", "127.0.0.1:8080", "", "k.pem", false, true},
		{"no port", "127.0.0.1", "", "", false, true},
	}
	for _, tt := range tests {
		err := checkListen(tt.addr, tt.cert, tt.key, tt.insecure)
		if (err != nil) != tt.wantErr {
			t.Errorf("%s: checkListen(%q) = %v, wantErr %v", tt.name, tt.addr, err, tt.wantErr)
		}
	}
}

func TestParseProxies(t *testing.T) {
	got, err := parseProxies([]string{"127.0.0.0/8", " ::1/128 ", "10.1.2.3", "", "192.168.1.77/24"})
	if err != nil {
		t.Fatalf("parseProxies: %v", err)
	}
	want := []string{"127.0.0.0/8", "::1/128", "10.1.2.3/32", "192.168.1.0/24"}
	if len(got) != len(want) {
		t.Fatalf("parseProxies = %v, want %v", got, want)
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Errorf("entry %d = %s, want %s", i, got[i], want[i])
		}
	}
	if nets, err := parseProxies([]string{""}); err != nil || len(nets) != 0 {
		t.Errorf("empty flag = %v, %v; want no trusted proxies", nets, err)
	}
	if _, err := parseProxies([]string{"proxy.lan"}); err == nil {
		t.Error("a hostname was accepted as a proxy network")
	}
}

// RABBITHOLE_LIBRARY_URL=off turns the library off.
func TestNewLibraryLoaderReadsTheEnvironment(t *testing.T) {
	t.Setenv(config.LibraryURLEnv, "off")
	if snap := newLibraryLoader("v1", false).Snapshot(); !snap.Off || snap.Loaded {
		t.Errorf("snapshot = %+v, want off and not loaded", snap)
	}
}

// A mirror named in the environment loads on any port, once private addresses
// are allowed.
func TestNewLibraryLoaderReachesAMirrorOnAnyPort(t *testing.T) {
	srv := httptest.NewServer(http.FileServer(http.Dir("../library")))
	defer srv.Close()
	t.Setenv(config.LibraryURLEnv, srv.URL+"/")
	t.Setenv(httpclient.AllowPrivateEnv, "1")

	snap := newLibraryLoader("v1", false).Get(t.Context())
	if !snap.Loaded || snap.Off || len(snap.Library.Files) == 0 {
		t.Fatalf("snapshot = %+v, want the mirror's library loaded", snap)
	}

	t.Setenv(httpclient.AllowPrivateEnv, "")
	if snap := newLibraryLoader("v1", false).Get(t.Context()); snap.Loaded {
		t.Errorf("loaded a loopback mirror without the private opt-in: %+v", snap)
	}
}

// countedMirror serves the real library files and counts every request.
func countedMirror(t *testing.T) (url string, hits *atomic.Int32) {
	t.Helper()
	hits = new(atomic.Int32)
	files := http.FileServer(http.Dir("../library"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		files.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Setenv(httpclient.AllowPrivateEnv, "1")
	return srv.URL + "/", hits
}

// Serve starts the library load at boot; building the loader alone fetches
// nothing, and a library that is off is never fetched.
func TestStartLibraryPreloadsInTheBackground(t *testing.T) {
	url, hits := countedMirror(t)
	t.Setenv(config.LibraryURLEnv, url)

	newLibraryLoader("v1", false)
	time.Sleep(100 * time.Millisecond)
	if n := hits.Load(); n != 0 {
		t.Fatalf("building the loader made %d requests, want none", n)
	}

	l := newLibraryLoader("v1", false)
	startLibrary(l)
	deadline := time.Now().Add(5 * time.Second)
	for !l.Snapshot().Loaded {
		if time.Now().After(deadline) {
			t.Fatal("the preload did not load the library")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if hits.Load() == 0 {
		t.Error("the library loaded without a request")
	}
}

func TestStartLibraryRespectsOff(t *testing.T) {
	url, hits := countedMirror(t)
	t.Setenv(config.LibraryURLEnv, "off")
	l := newLibraryLoader("v1", false)
	startLibrary(l)
	time.Sleep(100 * time.Millisecond)
	if snap := l.Snapshot(); !snap.Off || snap.Loaded {
		t.Errorf("snapshot = %+v, want off and not loaded", snap)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("an off library made %d requests (mirror %s)", n, url)
	}
}

// The preload returns without waiting for a slow host.
func TestStartLibraryDoesNotBlock(t *testing.T) {
	gate := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-gate }))
	t.Cleanup(func() { close(gate); srv.Close() })
	t.Setenv(httpclient.AllowPrivateEnv, "1")
	t.Setenv(config.LibraryURLEnv, srv.URL+"/")
	start := time.Now()
	startLibrary(newLibraryLoader("v1", false))
	if d := time.Since(start); d > time.Second {
		t.Errorf("startLibrary took %v with the host held, want no wait", d)
	}
}

// --dev reads ./library from disk, and an explicit library URL still wins.
func TestNewLibraryLoaderDevReadsTheLibraryDirectory(t *testing.T) {
	t.Chdir("..")
	t.Setenv(config.LibraryURLEnv, "")
	if snap := newLibraryLoader("v1", true).Get(t.Context()); !snap.Loaded || len(snap.Library.Files) == 0 {
		t.Errorf("dev snapshot = %+v, want the disk library loaded", snap)
	}

	t.Setenv(config.LibraryURLEnv, "off")
	if snap := newLibraryLoader("v1", true).Snapshot(); !snap.Off || snap.Loaded {
		t.Errorf("snapshot with off = %+v, want off and not loaded", snap)
	}
}

func TestLibraryBase(t *testing.T) {
	tests := []struct {
		raw     string
		base    string
		custom  bool
		wantErr bool
	}{
		{"", library.DefaultBaseURL, false, false},
		{"  ", library.DefaultBaseURL, false, false},
		{"off", "off", false, false},
		{" OFF ", "off", false, false},
		{library.DefaultBaseURL, library.DefaultBaseURL, false, false},
		{
			strings.TrimSuffix(library.DefaultBaseURL, "/"),
			strings.TrimSuffix(library.DefaultBaseURL, "/"),
			false,
			false,
		},
		{"https://mirror.example/library/", "https://mirror.example/library/", true, false},
		{"http://127.0.0.1:8081/lib", "http://127.0.0.1:8081/lib", true, false},
		{"ftp://mirror.example/library/", library.DefaultBaseURL, false, true},
		{"mirror.example/library", library.DefaultBaseURL, false, true},
		{"https:///library", library.DefaultBaseURL, false, true},
		{"/library", library.DefaultBaseURL, false, true},
		{"://bad", library.DefaultBaseURL, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			base, custom, err := libraryBase(tt.raw)
			if base != tt.base || custom != tt.custom || (err != nil) != tt.wantErr {
				t.Errorf("libraryBase(%q) = %q, %v, %v; want %q, %v, err=%v",
					tt.raw, base, custom, err, tt.base, tt.custom, tt.wantErr)
			}
		})
	}
}

// TestRecoverBriefings pins the serve boot step for interrupted briefings:
// orphaned generating rows are failed before serving starts, and a failing
// sweep is an error that aborts the boot instead of a log line the server
// outlives.
func TestRecoverBriefings(t *testing.T) {
	t.Run("fails rows a previous process left generating", func(t *testing.T) {
		db, err := store.Open(filepath.Join(t.TempDir(), "restart.db"))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		start := time.Date(2026, 1, 6, 0, 0, 0, 0, time.UTC)
		seed := func(id string, offset int, status string) {
			t.Helper()
			s := start.AddDate(0, 0, offset)
			if _, err := db.SaveBriefing(context.Background(), store.Briefing{
				ID: id, PeriodKind: "daily", PeriodStart: s, PeriodEnd: s.AddDate(0, 0, 1),
				Status: status,
			}); err != nil {
				t.Fatalf("SaveBriefing(%s): %v", id, err)
			}
		}
		seed("briefing-generating", 0, store.BriefingGenerating)
		seed("briefing-ready", 1, store.BriefingReady)

		if err := recoverBriefings(context.Background(), db); err != nil {
			t.Fatalf("recoverBriefings: %v", err)
		}
		recovered, err := db.BriefingForPeriod(context.Background(), "daily", start)
		if err != nil {
			t.Fatalf("BriefingForPeriod: %v", err)
		}
		if recovered.Status != store.BriefingFailed ||
			!strings.Contains(recovered.Error, "generation was interrupted") {
			t.Errorf("recovered row = %+v, want failed with the retry hint", recovered)
		}
		ready, err := db.BriefingForPeriod(context.Background(), "daily", start.AddDate(0, 0, 1))
		if err != nil {
			t.Fatalf("BriefingForPeriod(ready): %v", err)
		}
		if ready.Status != store.BriefingReady || ready.Error != "" {
			t.Errorf("ready row was touched: %+v", ready)
		}
	})

	t.Run("a failing sweep aborts the boot", func(t *testing.T) {
		db, err := store.Open(filepath.Join(t.TempDir(), "closed.db"))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if err := recoverBriefings(context.Background(), db); err == nil {
			t.Fatal("recoverBriefings on a closed store = nil, want the boot to abort")
		}
	})
}

func iconTestPNG() []byte {
	img := image.NewNRGBA(image.Rect(0, 0, 16, 16))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = 200, 30, 30, 255
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		panic(err)
	}
	return b.Bytes()
}

func countedIconServer(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	hits := new(atomic.Int32)
	png := iconTestPNG()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path == "/favicon.ico" {
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(png)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><head></head><body></body></html>"))
	}))
	t.Cleanup(srv.Close)
	t.Setenv(httpclient.AllowPrivateEnv, "1")
	return srv.URL, hits
}

func iconTestStore(t *testing.T, feedURL string) *store.Store {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "icons.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	doc := config.FeedsDoc{Feeds: []config.Feed{{Name: "Test", URL: feedURL}}}
	if _, err := db.SeedFeeds(context.Background(), doc); err != nil {
		t.Fatalf("SeedFeeds: %v", err)
	}
	return db
}

func iconTestManager(t *testing.T, db *store.Store, client *http.Client) *ingest.Manager {
	t.Helper()
	m, err := ingest.NewManager(db, config.Static(config.Config{}), 0)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	m.SetClient(client)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		m.Shutdown(ctx)
	})
	return m
}

func TestStartIconsPreloadsInTheBackground(t *testing.T) {
	url, hits := countedIconServer(t)
	feedURL := url + "/rss"
	db := iconTestStore(t, feedURL)
	m := iconTestManager(t, db, feeds.NewClient("test"))

	if n := hits.Load(); n != 0 {
		t.Fatalf("building the manager made %d requests, want none", n)
	}
	startIcons(m)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("icon pass did not record a row")
		}
		meta, _ := db.FeedIconMeta(context.Background())
		if len(meta) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if hits.Load() == 0 {
		t.Error("icon pass completed without a request")
	}
}

func TestStartIconsSkipsFreshIcons(t *testing.T) {
	url, hits := countedIconServer(t)
	feedURL := url + "/rss"
	db := iconTestStore(t, feedURL)
	m := iconTestManager(t, db, feeds.NewClient("test"))

	startIcons(m)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("first pass did not record a row")
		}
		meta, _ := db.FeedIconMeta(context.Background())
		if len(meta) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Wait until the first pass fully settles.
	time.Sleep(100 * time.Millisecond)
	hits.Store(0)
	startIcons(m)
	time.Sleep(200 * time.Millisecond)
	if n := hits.Load(); n != 0 {
		t.Errorf("fresh icons made %d requests, want none (normal restart contacts no sites)", n)
	}
}

func TestStartIconsDoesNotBlock(t *testing.T) {
	gate := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-gate }))
	t.Cleanup(func() { close(gate); srv.Close() })
	t.Setenv(httpclient.AllowPrivateEnv, "1")
	feedURL := srv.URL + "/rss"
	db := iconTestStore(t, feedURL)
	m := iconTestManager(t, db, feeds.NewClient("test"))
	start := time.Now()
	startIcons(m)
	if d := time.Since(start); d > time.Second {
		t.Errorf("startIcons took %v with the host held, want no wait", d)
	}
}

func TestStartIconsToleratesNilManager(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("startIcons(nil) panicked: %v", r)
		}
	}()
	startIcons(nil)
}
