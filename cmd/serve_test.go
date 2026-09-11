// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package cmd

import "testing"

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
