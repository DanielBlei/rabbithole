// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package httpclient

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
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

func TestCheckAddrNAT64Text(t *testing.T) {
	tests := []struct {
		addr        string
		wantPrivate bool
	}{
		{"64:ff9b::a00:5", true},
		{"64:ff9b::7f00:1", true},
		{"64:ff9b::a9fe:a9fe", false},
		{"64:ff9b::1", false},
	}
	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			err := checkAddr(netip.MustParseAddr(tt.addr), false)
			if err == nil {
				t.Fatal("not blocked")
			}
			if got := strings.Contains(err.Error(), "(private range)"); got != tt.wantPrivate {
				t.Errorf("err = %q, private range suffix = %v, want %v", err, got, tt.wantPrivate)
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

// publicHost and privateHost are the IPv6 and IPv4 loopback addresses, which
// bind on every platform; chainClient judges the first public, as a stand-in
// for a routable address.
const (
	publicHost  = "::1"
	privateHost = "127.0.0.1"
)

func chainClient(o Options) *http.Client {
	return newSafe(o, func(a netip.Addr) bool { return a.Unmap().String() == publicHost })
}

type hitServer struct {
	*httptest.Server
	hits *atomic.Int32
}

// serveOn starts a server on host that redirects /to?u=... and counts requests.
func serveOn(t *testing.T, host string) hitServer {
	t.Helper()
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Skipf("cannot listen on %s: %v", host, err)
	}
	hits := new(atomic.Int32)
	mux := http.NewServeMux()
	mux.HandleFunc("/to", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, r.URL.Query().Get("u"), http.StatusFound)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, "ok")
	})
	srv := httptest.NewUnstartedServer(mux)
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return hitServer{srv, hits}
}

func getBody(t *testing.T, c *http.Client, target string) (string, error) {
	t.Helper()
	resp, err := get(t, c, target)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return string(b), nil
}

func redirectTo(from hitServer, to string) string {
	return from.URL + "/to?u=" + url.QueryEscape(to)
}

func TestChainControl(t *testing.T) {
	isPublic := func(a netip.Addr) bool { return a.String() == "93.184.216.34" }
	tests := []struct {
		name    string
		address string
		anyPort bool
		wantErr error
	}{
		{"public passes", "93.184.216.34:443", false, nil},
		{"public port rule still applies", "93.184.216.34:8080", false, ErrBlockedAddress},
		{"public on any port", "93.184.216.34:8080", true, nil},
		{"loopback is a private redirect", "127.0.0.1:443", false, ErrPrivateRedirect},
		{"lan is a private redirect", "192.168.1.5:443", false, ErrPrivateRedirect},
		{"ipv6 loopback", "[::1]:443", false, ErrPrivateRedirect},
		{"mapped private", "[::ffff:10.0.0.1]:443", false, ErrPrivateRedirect},
		{"metadata stays plain blocked", "169.254.169.254:80", false, ErrBlockedAddress},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := chainControl(Options{AllowPrivate: true, AnyPort: tt.anyPort}, isPublic)("tcp", tt.address, nil)
			if !errors.Is(err, tt.wantErr) && (tt.wantErr != nil || err != nil) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr != nil && !errors.Is(err, ErrBlockedAddress) {
				t.Errorf("err %v does not match ErrBlockedAddress", err)
			}
			if tt.name == "metadata stays plain blocked" && errors.Is(err, ErrPrivateRedirect) {
				t.Errorf("always-denied address reported as a private redirect: %v", err)
			}
		})
	}
}

func TestPrivateRedirectErrorMatchesBlockedAddress(t *testing.T) {
	if !errors.Is(ErrPrivateRedirect, ErrBlockedAddress) {
		t.Fatal("ErrPrivateRedirect does not match ErrBlockedAddress")
	}
	if errors.Is(ErrBlockedAddress, ErrPrivateRedirect) {
		t.Fatal("ErrBlockedAddress matches ErrPrivateRedirect")
	}
}

func TestSafeClientPublicToPrivateRedirect(t *testing.T) {
	pub, priv := serveOn(t, publicHost), serveOn(t, privateHost)
	c := chainClient(Options{AllowPrivate: true, AnyPort: true, MaxRedirects: 5})

	_, err := getBody(t, c, redirectTo(pub, priv.URL+"/feed"))
	if !errors.Is(err, ErrPrivateRedirect) || !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("err = %v, want ErrPrivateRedirect and ErrBlockedAddress", err)
	}
	if n := priv.hits.Load(); n != 0 {
		t.Errorf("private server saw %d requests, want 0", n)
	}

	t.Run("later hop of a public chain", func(t *testing.T) {
		mid := redirectTo(pub, redirectTo(pub, priv.URL+"/feed"))
		if _, err := getBody(t, c, mid); !errors.Is(err, ErrPrivateRedirect) {
			t.Fatalf("err = %v, want ErrPrivateRedirect", err)
		}
	})
	t.Run("always-denied stays plain blocked", func(t *testing.T) {
		_, err := getBody(t, c, redirectTo(pub, "http://169.254.169.254/latest"))
		if !errors.Is(err, ErrBlockedAddress) || errors.Is(err, ErrPrivateRedirect) {
			t.Fatalf("err = %v, want ErrBlockedAddress only", err)
		}
	})
}

func TestSafeClientPublicToPublicRedirect(t *testing.T) {
	pub := serveOn(t, publicHost)
	c := chainClient(Options{AllowPrivate: true, AnyPort: true, MaxRedirects: 5})
	for range 2 {
		body, err := getBody(t, c, redirectTo(pub, pub.URL+"/feed"))
		if err != nil || body != "ok" {
			t.Fatalf("body = %q, err = %v", body, err)
		}
	}
}

func TestSafeClientPrivateToPublicToPrivateRedirect(t *testing.T) {
	a, b := serveOn(t, privateHost), serveOn(t, privateHost)
	pub := serveOn(t, publicHost)
	c := chainClient(Options{AllowPrivate: true, AnyPort: true, MaxRedirects: 5})

	_, err := getBody(t, c, redirectTo(a, redirectTo(pub, b.URL+"/feed")))
	if !errors.Is(err, ErrPrivateRedirect) {
		t.Fatalf("err = %v, want ErrPrivateRedirect", err)
	}
	if n := b.hits.Load(); n != 0 {
		t.Errorf("private server saw %d requests, want 0", n)
	}
}

func TestSafeClientPrivateToPrivateRedirect(t *testing.T) {
	a, b := serveOn(t, privateHost), serveOn(t, privateHost)
	pub := serveOn(t, publicHost)
	c := chainClient(Options{AllowPrivate: true, AnyPort: true, MaxRedirects: 5})

	for name, target := range map[string]string{
		"same host":      redirectTo(a, a.URL+"/feed"),
		"another server": redirectTo(a, b.URL+"/feed"),
		"on to a public": redirectTo(a, pub.URL+"/feed"),
		"back and forth": redirectTo(a, redirectTo(b, a.URL+"/feed")),
	} {
		t.Run(name, func(t *testing.T) {
			body, err := getBody(t, c, target)
			if err != nil || body != "ok" {
				t.Fatalf("body = %q, err = %v", body, err)
			}
		})
	}

	t.Run("blocked without AllowPrivate", func(t *testing.T) {
		_, err := getBody(t, chainClient(Options{AnyPort: true, MaxRedirects: 5}), redirectTo(a, b.URL))
		if !errors.Is(err, ErrBlockedAddress) {
			t.Fatalf("err = %v, want ErrBlockedAddress", err)
		}
	})
}

func TestSafeClientRedirectRuleSkipsPooledConnections(t *testing.T) {
	pub, priv := serveOn(t, publicHost), serveOn(t, privateHost)
	c := chainClient(Options{AllowPrivate: true, AnyPort: true, MaxRedirects: 5})

	// A private fetch leaves an idle connection to priv in the pool.
	if body, err := getBody(t, c, priv.URL+"/warm"); err != nil || body != "ok" {
		t.Fatalf("warm-up: %q, %v", body, err)
	}
	before := priv.hits.Load()

	// The public first hop is also served from the pool on the second pass.
	for i := range 2 {
		_, err := getBody(t, c, redirectTo(pub, priv.URL+"/feed"))
		if !errors.Is(err, ErrPrivateRedirect) {
			t.Fatalf("pass %d: err = %v, want ErrPrivateRedirect", i, err)
		}
	}
	if n := priv.hits.Load(); n != before {
		t.Errorf("private server saw %d requests through a pooled connection", n-before)
	}

	// A private chain keeps using the pool and is not blocked.
	if body, err := getBody(t, c, redirectTo(priv, priv.URL+"/feed")); err != nil || body != "ok" {
		t.Fatalf("private chain: %q, %v", body, err)
	}
}

func TestSafeClientProxiedFirstHopIsPublic(t *testing.T) {
	priv := serveOn(t, privateHost)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, priv.URL+"/feed", http.StatusFound)
	}))
	defer proxy.Close()
	proxyURL, _ := url.Parse(proxy.URL)
	// Only the feed host goes through the proxy; the redirect target is dialled
	// directly, as with a NO_PROXY match.
	pick := func(r *http.Request) (*url.URL, error) {
		if r.URL.Hostname() == "feeds.example.test" {
			return proxyURL, nil
		}
		return nil, nil
	}
	c := chainClient(Options{AllowPrivate: true, AnyPort: true, MaxRedirects: 5, Proxy: pick})

	_, err := getBody(t, c, "http://feeds.example.test/rss")
	if !errors.Is(err, ErrPrivateRedirect) {
		t.Fatalf("err = %v, want ErrPrivateRedirect", err)
	}
	if n := priv.hits.Load(); n != 0 {
		t.Errorf("private server saw %d requests, want 0", n)
	}
}

func TestSafeClientRedirectRuleWithProxy(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "via proxy")
	}))
	defer proxy.Close()
	proxyURL, _ := url.Parse(proxy.URL)
	pick := func(r *http.Request) (*url.URL, error) {
		if r.URL.Hostname() == "10.0.0.5" || strings.HasSuffix(r.URL.Hostname(), ".test") {
			return proxyURL, nil
		}
		return nil, nil
	}
	pub, priv := serveOn(t, publicHost), serveOn(t, privateHost)
	c := chainClient(Options{AllowPrivate: true, AnyPort: true, MaxRedirects: 5, Proxy: pick})

	t.Run("public chain to a private literal", func(t *testing.T) {
		if _, err := getBody(t, c, redirectTo(pub, "http://10.0.0.5/feed")); !errors.Is(err, ErrPrivateRedirect) {
			t.Fatalf("err = %v, want ErrPrivateRedirect", err)
		}
	})
	t.Run("public chain to a proxied hostname", func(t *testing.T) {
		body, err := getBody(t, c, redirectTo(pub, "http://feeds.example.test/rss"))
		if err != nil || body != "via proxy" {
			t.Fatalf("body = %q, err = %v", body, err)
		}
	})
	t.Run("private chain to a private literal", func(t *testing.T) {
		body, err := getBody(t, c, redirectTo(priv, "http://10.0.0.5/feed"))
		if err != nil || body != "via proxy" {
			t.Fatalf("body = %q, err = %v", body, err)
		}
	})
}
