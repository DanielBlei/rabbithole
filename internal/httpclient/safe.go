// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package httpclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// AllowPrivateEnv opts a deployment into fetching private address ranges.
const AllowPrivateEnv = "RABBITHOLE_ALLOW_PRIVATE_FETCH"

var (
	// ErrBlockedAddress marks a dial refused by the address or port policy.
	ErrBlockedAddress = errors.New("blocked address")
	// ErrTooManyRedirects marks a redirect chain longer than MaxRedirects.
	ErrTooManyRedirects = errors.New("too many redirects")
	// ErrUnsupportedScheme marks a request or redirect to a non-http(s) URL.
	ErrUnsupportedScheme = errors.New("unsupported url scheme")
	// ErrInsecureRedirect marks a redirect from https to http.
	ErrInsecureRedirect = errors.New("insecure redirect")
	// ErrTooLarge marks a body longer than the limit given to LimitBody.
	ErrTooLarge = errors.New("response body too large")
	// ErrPrivateRedirect marks a redirect hop that would connect to a private
	// or loopback address after an earlier hop reached a public one. It
	// also matches ErrBlockedAddress.
	ErrPrivateRedirect = fmt.Errorf("%w: public to private redirect", ErrBlockedAddress)
)

// Options configures NewSafe. The zero value is the strictest policy.
type Options struct {
	// Timeout bounds a whole request as http.Client.Timeout does; 0 disables it.
	Timeout time.Duration
	// AnyPort allows every destination port; false allows only 80 and 443.
	AnyPort bool
	// MaxRedirects is the number of redirects followed; 0 follows none.
	MaxRedirects int
	// UserAgent is set on every request and redirect that carries none.
	UserAgent string
	// AllowPrivate opens the private ranges. The always-denied ranges stay closed.
	// A chain that has touched a public address may not reach a private one, so
	// only chains that stay private throughout may stay private.
	AllowPrivate bool
	// Proxy selects a proxy per request; nil means no proxy (the environment is
	// never consulted). With a proxy the dial goes to the proxy, so a target
	// hostname is resolved by the proxy and is not checked here; IP-literal
	// targets and localhost still are. Only the proxy's own host:port, for the
	// request that chose it, is exempt from the dial policy.
	Proxy func(*http.Request) (*url.URL, error)
}

var alwaysDenied = []netip.Prefix{
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("fd00:ec2::254/128"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("ff00::/8"),
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("255.255.255.255/32"),
	netip.MustParsePrefix("100.100.100.200/32"),
}

var privateRanges = []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("192.0.0.0/24"),
}

// nat64 is the well-known prefix; the policy judges the embedded IPv4.
var nat64 = netip.MustParsePrefix("64:ff9b::/96")

// AllowPrivateFromEnv reports whether RABBITHOLE_ALLOW_PRIVATE_FETCH is truthy.
func AllowPrivateFromEnv() bool {
	v, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(AllowPrivateEnv)))
	return err == nil && v
}

// checkAddr applies the address policy; IPv4-mapped IPv6 and NAT64 addresses
// are judged as the IPv4 they carry.
func checkAddr(addr netip.Addr, allowPrivate bool) error {
	addr = addr.Unmap().WithZone("")
	if !addr.IsValid() {
		return fmt.Errorf("%w: invalid address", ErrBlockedAddress)
	}
	if nat64.Contains(addr) {
		b := addr.As16()
		v4 := netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})
		if err := checkAddr(v4, allowPrivate); err != nil {
			suffix := ""
			if !inAny(alwaysDenied, v4) {
				suffix = " (private range)"
			}
			return fmt.Errorf("%w: %s (nat64 of %s)%s", ErrBlockedAddress, addr, v4, suffix)
		}
		return nil
	}
	if inAny(alwaysDenied, addr) {
		return fmt.Errorf("%w: %s", ErrBlockedAddress, addr)
	}
	if !allowPrivate && inAny(privateRanges, addr) {
		return fmt.Errorf("%w: %s (private range)", ErrBlockedAddress, addr)
	}
	return nil
}

func inAny(prefixes []netip.Prefix, addr netip.Addr) bool {
	for _, p := range prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

func checkPort(port uint16, anyPort bool) error {
	if anyPort || port == 80 || port == 443 {
		return nil
	}
	return fmt.Errorf("%w: port %d not allowed", ErrBlockedAddress, port)
}

// control runs on the resolved IP just before connect, so DNS rebinding
// cannot bypass the policy.
func control(o Options) func(network, address string, _ syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		ap, err := netip.ParseAddrPort(address)
		if err != nil {
			return fmt.Errorf("%w: %s", ErrBlockedAddress, address)
		}
		if err := checkAddr(ap.Addr(), o.AllowPrivate); err != nil {
			return err
		}
		return checkPort(ap.Port(), o.AnyPort)
	}
}

type proxyKey struct{}

// proxyChoice is the proxy decided for one request, carried in its context so
// the transport's Proxy func and the dialer act on the same decision.
type proxyChoice struct {
	u    *url.URL
	addr string
}

// checkProxiedTarget screens a target the proxy will resolve: IP literals go
// through the address policy and localhost names count as loopback.
func checkProxiedTarget(u *url.URL, allowPrivate bool) error {
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		if allowPrivate {
			return nil
		}
		return fmt.Errorf("%w: %s (private range)", ErrBlockedAddress, host)
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return checkAddr(addr, allowPrivate)
	}
	return nil
}

func proxyDialAddr(u *url.URL) string {
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "https":
			port = "443"
		case "socks5", "socks5h":
			port = "1080"
		default:
			port = "80"
		}
	}
	return strings.ToLower(net.JoinHostPort(u.Hostname(), port))
}

// NewSafe returns a client that refuses unsafe destinations, schemes and
// redirect chains.
func NewSafe(o Options) *http.Client {
	return newSafe(o, isPublicAddr)
}

// isPublicAddr reports whether addr is outside every private and always-denied
// range.
func isPublicAddr(addr netip.Addr) bool {
	return checkAddr(addr, false) == nil
}

// newSafe builds the client with a replaceable public-address classifier, so
// tests can treat one loopback address as public without touching the policy.
func newSafe(o Options, isPublic func(netip.Addr) bool) *http.Client {
	base := &net.Dialer{Timeout: boundedTimeout(30*time.Second, o.Timeout), KeepAlive: 30 * time.Second}
	guarded := &net.Dialer{Timeout: base.Timeout, KeepAlive: base.KeepAlive, Control: control(o)}
	// Later hops of a chain that began public dial fresh through this dialer.
	strictDialer := &net.Dialer{Timeout: base.Timeout, KeepAlive: base.KeepAlive, Control: chainControl(o, isPublic)}

	newTransport := func() *http.Transport {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.TLSHandshakeTimeout = boundedTimeout(tr.TLSHandshakeTimeout, o.Timeout)
		tr.ForceAttemptHTTP2 = true
		if o.Timeout > 0 {
			tr.ResponseHeaderTimeout = o.Timeout
		}
		return tr
	}

	tr := newTransport()
	if o.Proxy == nil {
		tr.Proxy = nil
		tr.DialContext = guarded.DialContext
	} else {
		tr.Proxy = func(r *http.Request) (*url.URL, error) {
			if c, ok := r.Context().Value(proxyKey{}).(proxyChoice); ok {
				return c.u, nil
			}
			return nil, nil
		}
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if c, ok := ctx.Value(proxyKey{}).(proxyChoice); ok && strings.EqualFold(c.addr, addr) {
				return base.DialContext(ctx, network, addr)
			}
			return guarded.DialContext(ctx, network, addr)
		}
	}

	// A pooled connection skips the dial and so the address check. Hops that
	// must be judged on their own address never share a pool: this transport
	// keeps nothing idle, so each of them dials and the dial policy applies.
	strict := newTransport()
	strict.Proxy = nil
	strict.DisableKeepAlives = true
	strict.DialContext = strictDialer.DialContext

	return &http.Client{
		Transport: &safeTransport{
			base: tr, strict: strict, isPublic: isPublic,
			userAgent: o.UserAgent, proxy: o.Proxy, allowPrivate: o.AllowPrivate,
		},
		Timeout: o.Timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > o.MaxRedirects {
				return fmt.Errorf("%w: stopped after %d", ErrTooManyRedirects, o.MaxRedirects)
			}
			if err := checkScheme(req.URL); err != nil {
				return err
			}
			if last := via[len(via)-1].URL; last.Scheme == "https" && req.URL.Scheme == "http" {
				return fmt.Errorf("%w: %s to %s", ErrInsecureRedirect, last.Host, req.URL.Host)
			}
			return nil
		},
	}
}

// chainControl is the dial policy for the later hops of a chain that has
// reached a public address: private and loopback targets fail as a redirect
// into a private address, the always-denied ranges and the port rule as usual.
func chainControl(o Options, isPublic func(netip.Addr) bool) func(network, address string, _ syscall.RawConn) error {
	normal := control(o)
	return func(network, address string, c syscall.RawConn) error {
		if ap, err := netip.ParseAddrPort(address); err == nil && !isPublic(ap.Addr()) {
			if err := checkAddr(ap.Addr(), true); err != nil {
				return err
			}
			return fmt.Errorf("%w: %s", ErrPrivateRedirect, ap.Addr().Unmap().WithZone(""))
		}
		return normal(network, address, c)
	}
}

type chainKey struct{}

// chain is the verdict over one request's redirect chain, shared by every hop
// through the request context. It only moves from private to public.
type chain struct {
	public atomic.Bool // some hop's connection was to a public address
}

// observe records a hop's connection. It runs for new and pooled connections
// alike, so the verdict does not depend on a dial.
func (c *chain) observe(isPublic func(netip.Addr) bool) func(httptrace.GotConnInfo) {
	return func(info httptrace.GotConnInfo) {
		ap, err := netip.ParseAddrPort(info.Conn.RemoteAddr().String())
		// An address that cannot be read is judged as public, the stricter side.
		if err != nil || isPublic(ap.Addr()) {
			c.public.Store(true)
		}
	}
}

// chainFrom returns the chain a redirect request continues. The redirect's
// Response holds the request the previous hop sent through this transport, whose
// context carries the chain.
func chainFrom(r *http.Request) *chain {
	if r.Response == nil || r.Response.Request == nil {
		return nil
	}
	c, _ := r.Response.Request.Context().Value(chainKey{}).(*chain)
	return c
}

func boundedTimeout(d, limit time.Duration) time.Duration {
	if limit > 0 && (d <= 0 || limit < d) {
		return limit
	}
	return d
}

func checkScheme(u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%w: %q", ErrUnsupportedScheme, u.Scheme)
	}
	return nil
}

// safeTransport rejects non-http(s) requests, decides the proxy once per
// request, screens proxied targets and applies the User-Agent. It runs on every
// redirect hop. It also tracks the redirect chain: once any hop reached a
// public address, later hops go to private or loopback addresses nowhere.
type safeTransport struct {
	base         http.RoundTripper
	strict       http.RoundTripper
	isPublic     func(netip.Addr) bool
	userAgent    string
	proxy        func(*http.Request) (*url.URL, error)
	allowPrivate bool
}

func (t *safeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	fail := func(err error) (*http.Response, error) {
		if r.Body != nil {
			_ = r.Body.Close()
		}
		return nil, err
	}
	if err := checkScheme(r.URL); err != nil {
		return fail(err)
	}
	ctx := r.Context()
	proxied := false
	if t.proxy != nil {
		u, err := t.proxy(r)
		if err != nil {
			return fail(err)
		}
		if u != nil {
			if err := checkProxiedTarget(r.URL, t.allowPrivate); err != nil {
				return fail(err)
			}
			proxied = true
			ctx = context.WithValue(ctx, proxyKey{}, proxyChoice{u: u, addr: proxyDialAddr(u)})
		}
	}

	ch := chainFrom(r)
	if ch == nil {
		ch = &chain{}
		// A redirect whose chain is lost is judged as public, the stricter side.
		ch.public.Store(r.Response != nil)
	}
	ctx = context.WithValue(ctx, chainKey{}, ch)
	// A proxied hop is public unless its target is a private IP literal; a
	// hostname is the proxy's to resolve and counts as public, the stricter side.
	privateLiteral := proxied && checkProxiedTarget(r.URL, false) != nil
	rt := t.base
	switch {
	case ch.public.Load() && proxied:
		if privateLiteral {
			return fail(fmt.Errorf("%w: %s", ErrPrivateRedirect, r.URL.Hostname()))
		}
	case ch.public.Load():
		rt = t.strict
	case proxied:
		if !privateLiteral {
			ch.public.Store(true)
		}
	default:
		ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: ch.observe(t.isPublic)})
	}

	needUA := t.userAgent != "" && r.Header.Get("User-Agent") == ""
	if needUA || ctx != r.Context() {
		r = r.Clone(ctx)
		if needUA {
			r.Header.Set("User-Agent", t.userAgent)
		}
	}
	return rt.RoundTrip(r)
}

// LimitBody returns a reader that yields at most n bytes of r and then fails
// with ErrTooLarge if r holds more, so callers can tell truncation from EOF.
func LimitBody(r io.Reader, n int64) io.Reader {
	return &limitReader{r: r, n: max(n, 0)}
}

type limitReader struct {
	r io.Reader
	n int64 // bytes still allowed; -1 once the limit was exceeded
}

func (l *limitReader) Read(p []byte) (int, error) {
	if l.n < 0 {
		return 0, ErrTooLarge
	}
	// Read one byte past the limit to detect overflow.
	if int64(len(p))-1 > l.n {
		p = p[:l.n+1]
	}
	n, err := l.r.Read(p)
	l.n -= int64(n)
	if l.n < 0 {
		return n - 1, ErrTooLarge
	}
	return n, err
}
