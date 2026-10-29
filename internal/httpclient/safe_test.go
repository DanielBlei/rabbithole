// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package httpclient

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
)

func TestCheckAddr(t *testing.T) {
	tests := []struct {
		addr          string
		blockedStrict bool // blocked with AllowPrivate false
		blockedAlways bool // blocked even with AllowPrivate true
	}{
		{"93.184.216.34", false, false},
		{"2606:2800:220:1:248:1893:25c8:1946", false, false},
		{"::ffff:93.184.216.34", false, false},
		{"127.0.0.1", true, false},
		{"127.255.255.254", true, false},
		{"::1", true, false},
		{"::ffff:127.0.0.1", true, false},
		{"10.1.2.3", true, false},
		{"172.16.0.1", true, false},
		{"172.31.255.255", true, false},
		{"172.32.0.1", false, false},
		{"192.168.1.1", true, false},
		{"fc00::1", true, false},
		{"fd12:3456::1", true, false},
		{"100.64.0.1", true, false},
		{"100.127.255.255", true, false},
		{"100.128.0.1", false, false},
		{"64:ff9b::5db8:d822", false, false},
		{"64:ff9b::a00:1", true, false},
		{"64:ff9b::7f00:1", true, false},
		{"64:ff9b::a9fe:a9fe", true, true},
		{"64:ff9b::1", true, true},
		{"64:ff9b:1::1", true, false},
		{"198.18.0.1", true, false},
		{"198.19.255.255", true, false},
		{"198.17.255.255", false, false},
		{"198.20.0.1", false, false},
		{"192.0.0.1", true, false},
		{"192.0.0.255", true, false},
		{"192.0.1.1", false, false},
		{"100.100.100.200", true, true},
		{"100.100.100.201", true, false},
		{"240.0.0.1", true, false},
		{"254.255.255.255", true, false},
		{"169.254.169.254", true, true},
		{"::ffff:169.254.169.254", true, true},
		{"169.254.0.1", true, true},
		{"fe80::1", true, true},
		{"fe80::1%eth0", true, true},
		{"fd00:ec2::254", true, true},
		{"224.0.0.1", true, true},
		{"239.255.255.255", true, true},
		{"ff02::1", true, true},
		{"0.0.0.0", true, true},
		{"0.1.2.3", true, true},
		{"::", true, true},
		{"::ffff:0.0.0.0", true, true},
		{"255.255.255.255", true, true},
	}
	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			addr, err := netip.ParseAddr(tt.addr)
			if err != nil {
				t.Fatal(err)
			}
			strict := checkAddr(addr, false)
			if (strict != nil) != tt.blockedStrict {
				t.Errorf("AllowPrivate=false: err=%v, want blocked=%v", strict, tt.blockedStrict)
			}
			if strict != nil && !errors.Is(strict, ErrBlockedAddress) {
				t.Errorf("error %v does not wrap ErrBlockedAddress", strict)
			}
			open := checkAddr(addr, true)
			if (open != nil) != tt.blockedAlways {
				t.Errorf("AllowPrivate=true: err=%v, want blocked=%v", open, tt.blockedAlways)
			}
		})
	}
}

func TestCheckAddrInvalid(t *testing.T) {
	if err := checkAddr(netip.Addr{}, true); !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("invalid addr err = %v", err)
	}
}

func TestCheckPort(t *testing.T) {
	tests := []struct {
		port    uint16
		anyPort bool
		wantErr bool
	}{
		{80, false, false},
		{443, false, false},
		{8080, false, true},
		{22, false, true},
		{8080, true, false},
	}
	for _, tt := range tests {
		err := checkPort(tt.port, tt.anyPort)
		if (err != nil) != tt.wantErr {
			t.Errorf("checkPort(%d, %v) = %v, wantErr %v", tt.port, tt.anyPort, err, tt.wantErr)
		}
		if err != nil && !errors.Is(err, ErrBlockedAddress) {
			t.Errorf("checkPort error %v does not wrap ErrBlockedAddress", err)
		}
	}
}

func get(t *testing.T, c *http.Client, target string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c.Do(req)
}

func TestSafeClientLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	tests := []struct {
		name    string
		opts    Options
		wantErr bool
	}{
		{"default blocks loopback", Options{}, true},
		{"allow private still needs port", Options{AllowPrivate: true}, true},
		{"any port alone still blocks loopback", Options{AnyPort: true}, true},
		{"allow private and any port", Options{AllowPrivate: true, AnyPort: true}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := get(t, NewSafe(tt.opts), srv.URL)
			if tt.wantErr {
				if !errors.Is(err, ErrBlockedAddress) {
					t.Fatalf("err = %v, want ErrBlockedAddress", err)
				}
				var ue *url.Error
				if !errors.As(err, &ue) {
					t.Errorf("err %T is not a *url.Error", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			body, _ := io.ReadAll(resp.Body)
			if string(body) != "ok" {
				t.Errorf("body = %q", body)
			}
		})
	}
}

func TestSafeClientRedirectDowngrade(t *testing.T) {
	plain := redirectServer(t)
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u := r.URL.Query().Get("u"); u != "" {
			http.Redirect(w, r, u, http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer tls.Close()

	c := NewSafe(Options{AllowPrivate: true, AnyPort: true, MaxRedirects: 3})
	c.Transport.(*safeTransport).base.(*http.Transport).TLSClientConfig = tls.Client().Transport.(*http.Transport).TLSClientConfig

	_, err := get(t, c, tls.URL+"/?u="+url.QueryEscape(plain.URL+"/chain/0"))
	if !errors.Is(err, ErrInsecureRedirect) {
		t.Fatalf("https to http err = %v, want ErrInsecureRedirect", err)
	}

	resp, err := get(t, c, plain.URL+"/to?u="+url.QueryEscape(tls.URL+"/"))
	if err != nil {
		t.Fatalf("http to https: %v", err)
	}
	_ = resp.Body.Close()
}

func TestSafeClientUnsupportedScheme(t *testing.T) {
	c := NewSafe(Options{AllowPrivate: true, AnyPort: true})
	for _, u := range []string{"file:///etc/passwd", "ftp://example.com/x", "gopher://example.com/"} {
		_, err := get(t, c, u)
		if !errors.Is(err, ErrUnsupportedScheme) {
			t.Errorf("%s: err = %v, want ErrUnsupportedScheme", u, err)
		}
	}
}

func redirectServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/chain/{n}", func(w http.ResponseWriter, r *http.Request) {
		var n int
		_, _ = fmt.Sscanf(r.PathValue("n"), "%d", &n)
		if n == 0 {
			_, _ = io.WriteString(w, r.Header.Get("User-Agent"))
			return
		}
		http.Redirect(w, r, fmt.Sprintf("/chain/%d", n-1), http.StatusFound)
	})
	mux.HandleFunc("/to", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.Query().Get("u"), http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestSafeClientRedirects(t *testing.T) {
	srv := redirectServer(t)
	tests := []struct {
		name    string
		chain   int
		max     int
		wantErr bool
	}{
		{"no redirects, none needed", 0, 0, false},
		{"zero max rejects first redirect", 1, 0, true},
		{"within limit", 3, 3, false},
		{"one past limit", 4, 3, true},
		{"well past limit", 10, 2, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewSafe(Options{AllowPrivate: true, AnyPort: true, MaxRedirects: tt.max})
			resp, err := get(t, c, fmt.Sprintf("%s/chain/%d", srv.URL, tt.chain))
			if tt.wantErr {
				if !errors.Is(err, ErrTooManyRedirects) {
					t.Fatalf("err = %v, want ErrTooManyRedirects", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
		})
	}
}

func TestSafeClientRedirectScheme(t *testing.T) {
	srv := redirectServer(t)
	c := NewSafe(Options{AllowPrivate: true, AnyPort: true, MaxRedirects: 5})
	for _, target := range []string{"file:///etc/passwd", "ftp://example.com/x"} {
		_, err := get(t, c, srv.URL+"/to?u="+url.QueryEscape(target))
		if !errors.Is(err, ErrUnsupportedScheme) {
			t.Errorf("%s: err = %v, want ErrUnsupportedScheme", target, err)
		}
	}
}

func TestSafeClientRedirectToBlockedAddress(t *testing.T) {
	srv := redirectServer(t)
	c := NewSafe(Options{AllowPrivate: true, AnyPort: true, MaxRedirects: 5})
	_, err := get(t, c, srv.URL+"/to?u="+url.QueryEscape("http://169.254.169.254/latest/meta-data"))
	if !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("err = %v, want ErrBlockedAddress", err)
	}
}

func TestSafeClientUserAgent(t *testing.T) {
	srv := redirectServer(t)
	c := NewSafe(Options{AllowPrivate: true, AnyPort: true, MaxRedirects: 3, UserAgent: "rabbithole-test/1"})

	read := func(req *http.Request) string {
		t.Helper()
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}

	t.Run("first request", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/chain/0", nil)
		if got := read(req); got != "rabbithole-test/1" {
			t.Errorf("UA = %q", got)
		}
	})
	t.Run("after redirects", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/chain/3", nil)
		if got := read(req); got != "rabbithole-test/1" {
			t.Errorf("UA = %q", got)
		}
	})
	t.Run("explicit kept", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/chain/2", nil)
		req.Header.Set("User-Agent", "custom/2")
		if got := read(req); got != "custom/2" {
			t.Errorf("UA = %q", got)
		}
	})
	t.Run("request not mutated", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/chain/0", nil)
		read(req)
		if got := req.Header.Get("User-Agent"); got != "" {
			t.Errorf("caller request UA = %q, want empty", got)
		}
	})
}

func TestSafeClientProxy(t *testing.T) {
	var seen []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.String())
		_, _ = io.WriteString(w, "via proxy")
	}))
	defer proxy.Close()
	proxyURL, _ := url.Parse(proxy.URL)

	t.Run("proxy host is exempt, target is not resolved here", func(t *testing.T) {
		c := NewSafe(Options{Proxy: http.ProxyURL(proxyURL)})
		resp, err := get(t, c, "http://feeds.example.test/rss")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		if string(body) != "via proxy" {
			t.Errorf("body = %q", body)
		}
		if len(seen) != 1 || seen[0] != "http://feeds.example.test/rss" {
			t.Errorf("proxy saw %v", seen)
		}
	})

	t.Run("no proxy blocks loopback target", func(t *testing.T) {
		c := NewSafe(Options{})
		_, err := get(t, c, proxy.URL)
		if !errors.Is(err, ErrBlockedAddress) {
			t.Errorf("err = %v, want ErrBlockedAddress", err)
		}
	})

	t.Run("proxy func returning nil dials guarded", func(t *testing.T) {
		c := NewSafe(Options{Proxy: func(*http.Request) (*url.URL, error) { return nil, nil }})
		_, err := get(t, c, proxy.URL)
		if !errors.Is(err, ErrBlockedAddress) {
			t.Errorf("err = %v, want ErrBlockedAddress", err)
		}
	})
}

func TestSafeClientProxyExemptionIsPerRequest(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "via proxy")
	}))
	defer proxy.Close()
	proxyURL, _ := url.Parse(proxy.URL)

	// Only *.test hosts are proxied; everything else is dialed directly, as
	// with NO_PROXY or a loopback target under ProxyFromEnvironment.
	pick := func(r *http.Request) (*url.URL, error) {
		if strings.HasSuffix(r.URL.Hostname(), ".test") {
			return proxyURL, nil
		}
		return nil, nil
	}
	c := NewSafe(Options{Proxy: pick, MaxRedirects: 3})

	resp, err := get(t, c, "http://feeds.example.test/rss")
	if err != nil {
		t.Fatalf("proxied request: %v", err)
	}
	_ = resp.Body.Close()

	// The proxy address was just used as a proxy; a direct request to it is
	// still subject to the policy.
	if _, err := get(t, c, proxy.URL); !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("direct dial to proxy address err = %v, want ErrBlockedAddress", err)
	}

	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, proxy.URL, http.StatusFound)
	}))
	defer redir.Close()
	open := NewSafe(Options{Proxy: pick, AllowPrivate: true, AnyPort: true, MaxRedirects: 3})
	resp, err = get(t, open, redir.URL)
	if err != nil {
		t.Fatalf("allowed private redirect: %v", err)
	}
	_ = resp.Body.Close()
}

func TestSafeClientProxiedTargetChecks(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "via proxy")
	}))
	defer proxy.Close()
	proxyURL, _ := url.Parse(proxy.URL)

	tests := []struct {
		name    string
		opts    Options
		target  string
		blocked bool
	}{
		{"metadata ip", Options{}, "http://169.254.169.254/", true},
		{"metadata ip even when private allowed", Options{AllowPrivate: true}, "http://169.254.169.254/", true},
		{"private ip", Options{}, "http://10.0.0.5/", true},
		{"private ip allowed", Options{AllowPrivate: true}, "http://10.0.0.5/", false},
		{"ipv6 loopback", Options{}, "http://[::1]/", true},
		{"mapped ipv4 metadata", Options{}, "http://[::ffff:169.254.169.254]/", true},
		{"localhost", Options{}, "http://localhost/", true},
		{"localhost trailing dot", Options{}, "http://localhost./", true},
		{"subdomain of localhost", Options{}, "http://app.localhost/", true},
		{"localhost allowed with private", Options{AllowPrivate: true}, "http://localhost/", false},
		{"public hostname", Options{}, "http://feeds.example.test/rss", false},
		{"public ip", Options{}, "http://93.184.216.34/", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.opts.Proxy = http.ProxyURL(proxyURL)
			resp, err := get(t, NewSafe(tt.opts), tt.target)
			if tt.blocked {
				if !errors.Is(err, ErrBlockedAddress) {
					t.Fatalf("err = %v, want ErrBlockedAddress", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
		})
	}

	t.Run("redirect hop is checked", func(t *testing.T) {
		redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://169.254.169.254/latest", http.StatusFound)
		}))
		defer redirecting.Close()
		rURL, _ := url.Parse(redirecting.URL)
		c := NewSafe(Options{Proxy: http.ProxyURL(rURL), MaxRedirects: 3})
		_, err := get(t, c, "http://feeds.example.test/rss")
		if !errors.Is(err, ErrBlockedAddress) {
			t.Errorf("err = %v, want ErrBlockedAddress", err)
		}
	})
}

func TestNewSafeIgnoresProxyEnvironment(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	tr := NewSafe(Options{}).Transport.(*safeTransport).base.(*http.Transport)
	if tr.Proxy != nil {
		t.Error("Proxy set without Options.Proxy")
	}
}

func TestLimitBody(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		limit   int64
		want    string
		wantErr error
	}{
		{"under", "abc", 5, "abc", nil},
		{"exactly n", "abcde", 5, "abcde", nil},
		{"n plus one", "abcdef", 5, "abcde", ErrTooLarge},
		{"far over", strings.Repeat("x", 1000), 10, strings.Repeat("x", 10), ErrTooLarge},
		{"empty", "", 5, "", nil},
		{"zero limit empty", "", 0, "", nil},
		{"zero limit one byte", "a", 0, "", ErrTooLarge},
		{"negative limit", "a", -3, "", ErrTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := io.ReadAll(LimitBody(strings.NewReader(tt.input), tt.limit))
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("err = %v, want %v", err, tt.wantErr)
			}
			if string(got) != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLimitBodyErrorIsSticky(t *testing.T) {
	r := LimitBody(strings.NewReader("abcdef"), 2)
	_, _ = io.ReadAll(r)
	if _, err := r.Read(make([]byte, 4)); !errors.Is(err, ErrTooLarge) {
		t.Errorf("second read err = %v, want ErrTooLarge", err)
	}
}

func TestAllowPrivateFromEnv(t *testing.T) {
	tests := []struct {
		val  string
		want bool
	}{
		{"", false},
		{"1", true},
		{"true", true},
		{"TRUE", true},
		{" 1 ", true},
		{"0", false},
		{"false", false},
		{"yes", false},
		{"garbage", false},
	}
	for _, tt := range tests {
		t.Run(tt.val, func(t *testing.T) {
			t.Setenv(AllowPrivateEnv, tt.val)
			if got := AllowPrivateFromEnv(); got != tt.want {
				t.Errorf("AllowPrivateFromEnv(%q) = %v, want %v", tt.val, got, tt.want)
			}
		})
	}
}
