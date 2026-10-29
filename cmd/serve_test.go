// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/httpclient"
	"github.com/DanielBlei/rabbithole/internal/library"
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
