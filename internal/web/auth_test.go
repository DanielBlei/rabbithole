// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/config"
)

// browser drives a gated handler the way one browser would, carrying the
// session cookie from response to request.
type browser struct {
	t        *testing.T
	h        http.Handler
	cookie   *http.Cookie
	remember *http.Cookie // the "stay signed in" cookie, when set
	header   http.Header  // sent with every request
	remote   string       // the connection's address, when not httptest's default
}

func newBrowser(t *testing.T, w *Web) *browser {
	return &browser{t: t, h: w.Gate(w.Routes()), header: http.Header{}}
}

func (b *browser) do(method, path string, form url.Values, extra ...string) *httptest.ResponseRecorder {
	b.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range b.header {
		req.Header[k] = v
	}
	for i := 0; i+1 < len(extra); i += 2 {
		req.Header.Set(extra[i], extra[i+1])
	}
	if b.cookie != nil {
		req.AddCookie(b.cookie)
	}
	if b.remember != nil {
		req.AddCookie(b.remember)
	}
	if b.remote != "" {
		req.RemoteAddr = b.remote
	}
	rec := httptest.NewRecorder()
	b.h.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		kept := c
		if c.MaxAge < 0 {
			kept = nil
		}
		switch c.Name {
		case sessionCookie:
			b.cookie = kept
		case rememberCookie:
			b.remember = kept
		}
	}
	return rec
}

func (b *browser) get(path string, extra ...string) *httptest.ResponseRecorder {
	b.t.Helper()
	return b.do(http.MethodGet, path, nil, extra...)
}

func (b *browser) login(user, pass string) *httptest.ResponseRecorder {
	b.t.Helper()
	return b.do(http.MethodPost, "/login", url.Values{"username": {user}, "password": {pass}})
}

func wantRedirect(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != want {
		t.Fatalf("got %d to %q, want 303 to %q", rec.Code, rec.Header().Get("Location"), want)
	}
}

func wantStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, want, rec.Body)
	}
}

// gatedWeb is a test Web with a password already set.
func gatedWeb(t *testing.T) *Web {
	t.Helper()
	w := newTestWeb(t)
	if err := w.db.SetPassword(context.Background(), "alice", "correct horse"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	return w
}

func TestGateSendsVisitorsToLogin(t *testing.T) {
	b := newBrowser(t, gatedWeb(t))

	wantRedirect(t, b.get("/feed"), "/login?next=%2Ffeed")
	wantRedirect(t, b.get("/"), "/login")

	// htmx is sent to the login as a full page, and back to the page it came from.
	rec := b.get("/ingest/status", "HX-Request", "true", "HX-Current-URL", "http://host:8080/maze?x=1")
	wantStatus(t, rec, http.StatusUnauthorized)
	if got := rec.Header().Get("HX-Redirect"); got != "/login?next=%2Fmaze%3Fx%3D1" {
		t.Errorf("HX-Redirect = %q", got)
	}
	wantStatus(t, b.do(http.MethodPost, "/todos", url.Values{"title": {"x"}}), http.StatusUnauthorized)

	wantStatus(t, b.get("/static/style.css"), http.StatusOK)
	page := b.get("/login")
	wantStatus(t, page, http.StatusOK)
	if body := page.Body.String(); !strings.Contains(body, "forgot password") {
		t.Error("the login should point at the reset instructions")
	}
	if page.Header().Get("X-Frame-Options") != "DENY" || page.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("login headers = %v", page.Header())
	}
}

func TestFreshInstallLandsOnSetupWithNoLogin(t *testing.T) {
	b := newBrowser(t, newTestWeb(t))

	// Nobody has claimed the instance, so there is no login to offer and none
	// to pass: every page, and the login itself, points at the setup page.
	wantRedirect(t, b.get("/feed"), "/setup")
	wantRedirect(t, b.get("/login"), "/setup")
	wantRedirect(t, b.login("admin", "admin"), "/setup")
	if b.cookie != nil {
		t.Fatal("a fresh install issued a session cookie")
	}
	rec := b.get("/maze", "HX-Request", "true")
	if rec.Code != http.StatusNoContent || rec.Header().Get("HX-Redirect") != "/setup" {
		t.Errorf("htmx during setup = %d %q", rec.Code, rec.Header().Get("HX-Redirect"))
	}
	// The API has no page to send anyone to and stays shut until setup.
	wantStatus(t, b.get("/api/items"), http.StatusUnauthorized)

	setup := b.get("/setup")
	wantStatus(t, setup, http.StatusOK)
	// The two ways in are a pair on the card, neither hidden behind the other.
	for _, want := range []string{`id="gateStepAccount"`, `id="gateStepOpen"`,
		`id="gateStepPick"`, `Create an account`, `Leave it open`} {
		if !strings.Contains(setup.Body.String(), want) {
			t.Errorf("first-run setup lacks %q", want)
		}
	}
	// Nothing on it hands out a credential to type.
	if strings.Contains(setup.Body.String(), "admin</code>") {
		t.Error("the setup page still quotes a default login")
	}
	// The first run also picks the look, with Settings → Theme's own radios.
	for _, want := range []string{`data-theme-pick`, `theme --first-login`, `/static/js/theme.js`} {
		if !strings.Contains(setup.Body.String(), want) {
			t.Errorf("first-run setup lacks %q", want)
		}
	}
}

func TestOpenInstanceSetupHasNoLookPicker(t *testing.T) {
	w := newTestWeb(t)
	if err := w.db.DisableAuth(context.Background()); err != nil {
		t.Fatalf("DisableAuth: %v", err)
	}
	b := newBrowser(t, w)
	setup := b.get("/setup")
	wantStatus(t, setup, http.StatusOK)
	for _, unwanted := range []string{`data-theme-pick`, `/static/js/theme.js`} {
		if strings.Contains(setup.Body.String(), unwanted) {
			t.Errorf("setup on an open instance has %q", unwanted)
		}
	}
	// The login page carries no script beyond the pre-paint one.
	if err := w.db.SetPassword(context.Background(), "alice", "correct horse"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	if login := b.get("/login").Body.String(); strings.Contains(login, "theme.js") {
		t.Error("the login page loads theme.js")
	}
}

func TestSetupPasswordEndsEveryOtherSession(t *testing.T) {
	w := newTestWeb(t)
	a, other := newBrowser(t, w), newBrowser(t, w)

	mismatch := url.Values{"username": {"alice"}, "password": {"correct horse"}, "confirm": {"correct horsE"}}
	rec := a.do(http.MethodPost, "/setup", mismatch)
	wantStatus(t, rec, http.StatusBadRequest)
	if !strings.Contains(rec.Body.String(), "don&#39;t match") {
		t.Errorf("mismatch body lacks the message: %s", rec.Body)
	}
	short := url.Values{"username": {"alice"}, "password": {"short"}, "confirm": {"short"}}
	rec = a.do(http.MethodPost, "/setup", short)
	wantStatus(t, rec, http.StatusBadRequest)
	if !strings.Contains(rec.Body.String(), "at least 8 characters") {
		t.Errorf("short password body lacks the message: %s", rec.Body)
	}
	tiny := url.Values{"username": {"al"}, "password": {"correct horse"}, "confirm": {"correct horse"}}
	rec = a.do(http.MethodPost, "/setup", tiny)
	wantStatus(t, rec, http.StatusBadRequest)
	if !strings.Contains(rec.Body.String(), "at least 3 characters") {
		t.Errorf("short username body lacks the message: %s", rec.Body)
	}

	good := url.Values{"username": {"alice"}, "password": {"correct horse"}, "confirm": {"correct horse"}}
	wantRedirect(t, a.do(http.MethodPost, "/setup", good), "/")
	wantStatus(t, a.get("/feed"), http.StatusOK)
	wantRedirect(t, other.get("/feed"), "/login?next=%2Ffeed")

	// The new login works, and setup has nothing left to do.
	wantStatus(t, other.login("admin", "admin"), http.StatusUnauthorized)
	wantRedirect(t, other.login("alice", "correct horse"), "/")
	wantRedirect(t, other.get("/setup"), "/")
}

func TestSetupCanLeaveTheInstanceOpen(t *testing.T) {
	w := newTestWeb(t)
	b := newBrowser(t, w)
	b.login("admin", "admin")

	wantRedirect(t, b.do(http.MethodPost, "/setup", url.Values{"action": {"open"}}), "/")
	stranger := newBrowser(t, w)
	page := stranger.get("/feed")
	wantStatus(t, page, http.StatusOK)
	if !strings.Contains(page.Body.String(), `href="/setup"`) || strings.Contains(page.Body.String(), "log out") {
		t.Error("an open instance should offer to set a password, and no logout")
	}
	wantRedirect(t, stranger.get("/login"), "/")
	// Locking it again is allowed from the page, since it only tightens access.
	good := url.Values{"username": {"alice"}, "password": {"correct horse"}, "confirm": {"correct horse"}}
	wantRedirect(t, stranger.do(http.MethodPost, "/setup", good), "/")
	wantRedirect(t, newBrowser(t, w).get("/feed"), "/login?next=%2Ffeed")
}

func TestLoginReturnsToNextAndLogoutEndsTheSession(t *testing.T) {
	w := gatedWeb(t)
	b := newBrowser(t, w)

	form := url.Values{"username": {"alice"}, "password": {"correct horse"}, "next": {"/maze"}}
	wantRedirect(t, b.do(http.MethodPost, "/login", form), "/maze")
	page := b.get("/feed")
	wantStatus(t, page, http.StatusOK)
	if !strings.Contains(page.Body.String(), "log out") {
		t.Error("settings should offer log out when a password is set")
	}
	if !strings.Contains(b.get("/login").Header().Get("Location"), "/") {
		t.Error("a logged-in visit to /login should go home")
	}

	stale := *b.cookie
	wantRedirect(t, b.do(http.MethodPost, "/logout", nil), "/login")
	if b.cookie != nil {
		t.Fatal("logout did not clear the cookie")
	}
	b.cookie = &stale
	wantRedirect(t, b.get("/feed"), "/login?next=%2Ffeed")
}

func TestLoginPageOffersResetInstructions(t *testing.T) {
	body := newBrowser(t, gatedWeb(t)).get("/login").Body.String()
	for _, want := range []string{"forgot password?", "rabbithole auth reset"} {
		if !strings.Contains(body, want) {
			t.Errorf("login page lacks %q", want)
		}
	}
	if strings.Contains(body, "first run") {
		t.Error("the default-login hint shows after a password is set")
	}
}

// Sessions live in memory: a new process starts with none.
func TestRestartForgetsSessions(t *testing.T) {
	w := gatedWeb(t)
	b := newBrowser(t, w)
	b.login("alice", "correct horse")

	restarted := New(w.db, &config.Config{}, ":8080", "", testIngestManager(t, w.db))
	b.h = restarted.Gate(restarted.Routes())
	wantRedirect(t, b.get("/feed"), "/login?next=%2Ffeed")
}

// `rabbithole auth reset` runs in another process, with no reach into this
// one's memory; the auth row's generation is what ends the live sessions.
func TestResetFromAnotherProcessEndsSessions(t *testing.T) {
	w := gatedWeb(t)
	b := newBrowser(t, w)
	b.login("alice", "correct horse")
	wantStatus(t, b.get("/feed"), http.StatusOK)

	if err := w.db.SetPassword(context.Background(), "alice", "battery staple"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	wantRedirect(t, b.get("/feed"), "/login?next=%2Ffeed")
	wantRedirect(t, b.login("alice", "battery staple"), "/")
}

// `rabbithole auth disable` from another process opens the instance; locking
// it again from the page leaves no session from before alive.
func TestDisableFromAnotherProcessThenLockAgain(t *testing.T) {
	w := gatedWeb(t)
	b := newBrowser(t, w)
	b.login("alice", "correct horse")
	if err := w.db.DisableAuth(context.Background()); err != nil {
		t.Fatalf("DisableAuth: %v", err)
	}
	stranger := newBrowser(t, w)
	wantStatus(t, stranger.get("/feed"), http.StatusOK)

	good := url.Values{"username": {"alice"}, "password": {"new horse!"}, "confirm": {"new horse!"}}
	wantRedirect(t, stranger.do(http.MethodPost, "/setup", good), "/")
	wantRedirect(t, b.get("/feed"), "/login?next=%2Ffeed")
	wantStatus(t, stranger.get("/feed"), http.StatusOK)
}

func TestSessionsExpireWhenIdle(t *testing.T) {
	w := gatedWeb(t)
	now := time.Now()
	w.sessions.now = func() time.Time { return now }
	b := newBrowser(t, w)
	b.login("alice", "correct horse")

	// Use keeps it alive: two gaps shorter than the idle limit add up past it.
	now = now.Add(sessionIdle - time.Hour)
	wantStatus(t, b.get("/feed"), http.StatusOK)
	if b.cookie == nil || b.cookie.MaxAge != int(sessionIdle/time.Second) {
		t.Fatalf("an active session's cookie was not re-sent: %+v", b.cookie)
	}
	now = now.Add(sessionIdle - time.Hour)
	wantStatus(t, b.get("/feed"), http.StatusOK)

	now = now.Add(sessionIdle + time.Minute)
	wantRedirect(t, b.get("/feed"), "/login?next=%2Ffeed")
}

func TestRepeatedFailuresLockTheClientOut(t *testing.T) {
	w := gatedWeb(t)
	now := time.Now()
	w.limiter.now = func() time.Time { return now }
	b := newBrowser(t, w)

	for i := range loginFreeTries {
		if rec := b.login("alice", "wrong"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i+1, rec.Code)
		}
	}
	rec := b.login("alice", "correct horse")
	wantStatus(t, rec, http.StatusTooManyRequests)
	if rec.Header().Get("Retry-After") != "30" || !strings.Contains(rec.Body.String(), "try again in 30s") {
		t.Errorf("lockout = %q, body lacks the wait", rec.Header().Get("Retry-After"))
	}

	now = now.Add(loginLockBase + time.Second)
	wantRedirect(t, b.login("alice", "correct horse"), "/")
}

func TestLockoutDoublesUpToTheCap(t *testing.T) {
	l := newLoginLimiter()
	now := time.Now()
	l.now = func() time.Time { return now }
	miss := func() {
		t.Helper()
		if wait, ok := l.begin("1.2.3.4"); !ok {
			t.Fatalf("begin refused with %v left", wait)
		}
		l.done("1.2.3.4", false)
	}
	for range loginFreeTries {
		miss()
	}
	if wait, ok := l.begin("1.2.3.4"); ok || wait != loginLockBase {
		t.Fatalf("after the free tries: begin = %v, %v; want refused for %v", wait, ok, loginLockBase)
	}
	now = now.Add(loginLockBase)
	miss()
	if wait, _ := l.begin("1.2.3.4"); wait != 2*loginLockBase {
		t.Fatalf("second lockout = %v, want %v", wait, 2*loginLockBase)
	}
	for range 20 {
		now = now.Add(loginLockMax)
		miss()
	}
	if wait, _ := l.begin("1.2.3.4"); wait != loginLockMax {
		t.Fatalf("capped lockout = %v, want %v", wait, loginLockMax)
	}
	if _, ok := l.begin("5.6.7.8"); !ok {
		t.Error("another client inherited the lockout")
	}
	now = now.Add(loginLockMax)
	if _, ok := l.begin("1.2.3.4"); !ok {
		t.Fatal("begin refused after the lockout ran out")
	}
	l.done("1.2.3.4", true)
	if _, ok := l.begin("1.2.3.4"); !ok {
		t.Error("a success did not clear the count")
	}
}

// One attempt per client is checked at a time: a burst cannot all pass the
// count before any of it is recorded.
func TestConcurrentLoginsCannotOutrunTheLimiter(t *testing.T) {
	w := gatedWeb(t)
	h := w.Gate(w.Routes())
	const burst = 20
	codes := make(chan int, burst)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range burst {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			form := url.Values{"username": {"alice"}, "password": {"wrong"}}
			req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			codes <- rec.Code
		}()
	}
	close(start)
	wg.Wait()
	close(codes)
	checked := 0
	for code := range codes {
		switch code {
		case http.StatusUnauthorized:
			checked++
		case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		default:
			t.Errorf("unexpected status %d", code)
		}
	}
	if checked < 1 || checked > loginFreeTries {
		t.Errorf("%d of %d parallel attempts were checked, want 1 to %d", checked, burst, loginFreeTries)
	}
}

// With every hash slot taken, a login is turned away before any hashing, and
// the turned-away attempt does not count against the client.
func TestLoginWhenEveryHashSlotIsBusy(t *testing.T) {
	w := gatedWeb(t)
	w.hashing = make(chan struct{}, 1)
	w.hashing <- struct{}{}
	b := newBrowser(t, w)

	rec := b.login("alice", "correct horse")
	wantStatus(t, rec, http.StatusServiceUnavailable)
	if rec.Header().Get("Retry-After") != "1" {
		t.Errorf("Retry-After = %q, want 1", rec.Header().Get("Retry-After"))
	}
	<-w.hashing
	for range loginFreeTries - 1 {
		wantStatus(t, b.login("alice", "wrong"), http.StatusUnauthorized)
	}
	wantRedirect(t, b.login("alice", "correct horse"), "/")
}

// Behind a trusted proxy each forwarded client is limited on its own, so one
// attacker cannot lock the owner out through the shared proxy address.
func TestLockoutBehindAProxyIsPerClient(t *testing.T) {
	w := gatedWeb(t)
	attacker, owner := newBrowser(t, w), newBrowser(t, w)
	for _, b := range []*browser{attacker, owner} {
		b.remote = "127.0.0.1:40000"
	}
	attacker.header.Set("X-Forwarded-For", "203.0.113.7")
	owner.header.Set("X-Forwarded-For", "198.51.100.20")

	for range loginFreeTries {
		attacker.login("alice", "wrong")
	}
	wantStatus(t, attacker.login("alice", "correct horse"), http.StatusTooManyRequests)
	wantRedirect(t, owner.login("alice", "correct horse"), "/")
}

func TestClientAddrTrustsOnlyConfiguredProxies(t *testing.T) {
	w := newTestWeb(t)
	tests := []struct {
		name, peer, xff, want string
		trusted               []netip.Prefix
	}{
		{"direct client ignores the header", "203.0.113.9:5000", "1.1.1.1", "203.0.113.9", nil},
		{"loopback proxy", "127.0.0.1:5000", "203.0.113.5", "203.0.113.5", nil},
		{"spoofed leftmost hop", "127.0.0.1:5000", "1.1.1.1, 203.0.113.5", "203.0.113.5", nil},
		{"chain of trusted proxies", "127.0.0.1:5000", "203.0.113.5, 10.0.0.2", "203.0.113.5",
			[]netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8")}},
		{"trusting none", "127.0.0.1:5000", "203.0.113.5", "127.0.0.1", []netip.Prefix{}},
		{"garbage hop stops the walk", "127.0.0.1:5000", "203.0.113.5, not-an-ip", "127.0.0.1", nil},
		{"ipv6 peer", "[2001:db8::1]:443", "", "2001:db8::1", nil},
	}
	for _, tt := range tests {
		w.SetTrustedProxies(DefaultTrustedProxies)
		if tt.trusted != nil {
			w.SetTrustedProxies(tt.trusted)
		}
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = tt.peer
		if tt.xff != "" {
			req.Header.Set("X-Forwarded-For", tt.xff)
		}
		if got := w.clientAddr(req); got != tt.want {
			t.Errorf("%s: clientAddr = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestLimitKeyGroupsIPv6By64(t *testing.T) {
	a, b := limitKey("2001:db8:1:2:aaaa::1"), limitKey("2001:db8:1:2:bbbb::9")
	if a != b {
		t.Errorf("one /64 gave two keys: %q, %q", a, b)
	}
	if limitKey("2001:db8:1:3::1") == a {
		t.Error("two /64s shared a key")
	}
	if got := limitKey("203.0.113.5"); got != "203.0.113.5" {
		t.Errorf("IPv4 key = %q", got)
	}
}

func TestLimiterStaysBounded(t *testing.T) {
	l := newLoginLimiter()
	for i := range loginTrackMax + 50 {
		key := netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)}).String()
		for range loginFreeTries {
			if _, ok := l.begin(key); ok {
				l.done(key, false)
			}
		}
	}
	if n := len(l.byKey); n > loginTrackMax {
		t.Errorf("limiter tracks %d clients, want at most %d", n, loginTrackMax)
	}
}

func TestSessionStoreStaysBounded(t *testing.T) {
	ss := newSessionStore()
	now := time.Now()
	ss.now = func() time.Time { return now }
	first, _ := ss.create("g")
	for range maxSessions + 10 {
		now = now.Add(time.Second)
		if _, err := ss.create("g"); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	if n := len(ss.byID); n > maxSessions {
		t.Errorf("%d sessions held, want at most %d", n, maxSessions)
	}
	if _, ok, _ := ss.check(first, "g"); ok {
		t.Error("the session seen longest ago survived the bound")
	}
}

func TestSessionsEndAtTheAbsoluteLimit(t *testing.T) {
	ss := newSessionStore()
	start := time.Now()
	now := start
	ss.now = func() time.Time { return now }
	token, _ := ss.create("g")
	// Used every six days: never idle long enough to lapse, until the cap.
	for range 4 {
		now = now.Add(6 * 24 * time.Hour)
		if _, ok, _ := ss.check(token, "g"); !ok {
			t.Fatalf("an active session ended %v in", now.Sub(start))
		}
	}
	now = start.Add(sessionMax + time.Minute)
	if _, ok, _ := ss.check(token, "g"); ok {
		t.Error("a session outlived its absolute limit")
	}
}

func TestGateFailsClosedWhenTheStoreFails(t *testing.T) {
	w := gatedWeb(t)
	b := newBrowser(t, w)
	b.login("alice", "correct horse")
	_ = w.db.Close()

	wantStatus(t, b.get("/feed"), http.StatusInternalServerError)
	wantStatus(t, b.get("/api/items"), http.StatusInternalServerError)
	wantStatus(t, b.get("/static/style.css"), http.StatusOK)
}

func TestGateHeaders(t *testing.T) {
	w := gatedWeb(t)
	h := w.Gate(w.Routes())
	get := func(path, host string, tlsOn bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = host
		if tlsOn {
			req.TLS = &tls.ConnectionState{}
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if got := get("/api/items", "rabbithole.lan", false).Header().Get("WWW-Authenticate"); got == "" {
		t.Error("a 401 from the API names no auth scheme")
	}
	if got := get("/feed", "rabbithole.lan", false).Header().Get("Cache-Control"); got != "private, no-store" {
		t.Errorf("gated page Cache-Control = %q", got)
	}
	for host, want := range map[string]bool{
		"rabbithole.lan":      true,
		"rabbithole.lan:8443": true,
		"localhost:8443":      false,
		"127.0.0.1:8443":      false,
		"[::1]:8443":          false,
	} {
		if got := get("/login", host, true).Header().Get("Strict-Transport-Security") != ""; got != want {
			t.Errorf("HSTS on %s = %v, want %v", host, got, want)
		}
	}
	if get("/login", "rabbithole.lan", false).Header().Get("Strict-Transport-Security") != "" {
		t.Error("HSTS sent over plain HTTP")
	}

	// Behind a TLS-terminating proxy: HSTS when a trusted proxy says the
	// browser used HTTPS, not when anyone else claims it.
	proxied := func(remote string) string {
		req := httptest.NewRequest(http.MethodGet, "/login", nil)
		req.Host, req.RemoteAddr = "rabbithole.lan", remote
		req.Header.Set("X-Forwarded-Proto", "https")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Header().Get("Strict-Transport-Security")
	}
	if proxied("127.0.0.1:40000") == "" {
		t.Error("no HSTS behind a trusted proxy serving HTTPS")
	}
	if proxied("203.0.113.9:40000") != "" {
		t.Error("an untrusted client's X-Forwarded-Proto earned HSTS")
	}
}

func TestSessionCheckReportsTheLogin(t *testing.T) {
	ss := newSessionStore()
	login := time.Now().Add(-3 * time.Hour)
	token, recorded, err := ss.createSince("g", login)
	if err != nil {
		t.Fatalf("createSince: %v", err)
	}
	since, ok, _ := ss.check(token, "g")
	if !ok || !since.Equal(login) || !recorded.Equal(login) {
		t.Errorf("check = %v %v, recorded %v; want the login at %v", since, ok, recorded, login)
	}
	ss.delete(token)
	if since, ok, _ := ss.check(token, "g"); ok || !since.IsZero() {
		t.Errorf("a deleted session checked as %v %v", since, ok)
	}
}

func TestLoginPageClearsADeadCookie(t *testing.T) {
	b := newBrowser(t, gatedWeb(t))
	b.cookie = &http.Cookie{Name: sessionCookie, Value: "stale"}
	wantStatus(t, b.get("/login"), http.StatusOK)
	if b.cookie != nil {
		t.Error("the login page left a dead session cookie in place")
	}
}

func TestSafeNextStaysOnSite(t *testing.T) {
	tests := map[string]string{
		"":                  "/",
		"/maze":             "/maze",
		"/feed?source=Go":   "/feed?source=Go",
		"//evil.example":    "/",
		"/\\evil.example":   "/",
		"https://evil.test": "/",
		"feed":              "/",
		"/login":            "/",
		"/logout":           "/",
		"/setup?x=1":        "/",
	}
	for in, want := range tests {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoginWarnsOnUnencryptedRemoteAccess(t *testing.T) {
	w := gatedWeb(t)
	h := w.Gate(w.Routes())
	const warning = "isn't encrypted"
	tests := []struct {
		host, proto string
		warn        bool
	}{
		{"localhost:8080", "", false},
		{"127.0.0.1:8080", "", false},
		{"[::1]:8080", "", false},
		{"192.168.1.20:8080", "", true},
		{"rabbithole.lan", "", true},
		{"rabbithole.lan", "https", false},
	}
	for _, tt := range tests {
		req := httptest.NewRequest(http.MethodGet, "/login", nil)
		req.RemoteAddr = "127.0.0.1:40000" // a proxy on this machine, trusted by default
		req.Host = tt.host
		if tt.proto != "" {
			req.Header.Set("X-Forwarded-Proto", tt.proto)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if got := strings.Contains(rec.Body.String(), warning); got != tt.warn {
			t.Errorf("host %s proto %q: warning shown = %v, want %v", tt.host, tt.proto, got, tt.warn)
		}
	}
}

func TestSessionCookieIsSecureBehindTLS(t *testing.T) {
	w := gatedWeb(t)
	proxied := newBrowser(t, w)
	proxied.remote = "127.0.0.1:40000"
	proxied.header.Set("X-Forwarded-Proto", "https")
	proxied.login("alice", "correct horse")
	if proxied.cookie == nil || !proxied.cookie.Secure {
		t.Fatalf("cookie behind a trusted proxy = %+v, want Secure", proxied.cookie)
	}

	// The header from anyone but a trusted proxy is ignored.
	direct := newBrowser(t, w)
	direct.header.Set("X-Forwarded-Proto", "https")
	direct.login("alice", "correct horse")
	if direct.cookie == nil || direct.cookie.Secure {
		t.Fatalf("cookie from an untrusted peer = %+v, want not Secure", direct.cookie)
	}
}
