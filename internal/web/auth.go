// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"html/template"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/DanielBlei/rabbithole/internal/store"
)

// gateTmpl renders the login and setup screens. It is its own set: the pages
// stand alone, outside the layout, since they have no chrome to show.
var gateTmpl = template.Must(template.New("login.html").ParseFS(templatesFS, "templates/login.html"))

const (
	sessionCookie = "rh_session"
	// sessionIdle is how long a session survives without a request, and
	// sessionMax how long it survives at all. Sessions also end with the
	// process: a restart asks for the login again, unless the browser chose to
	// stay signed in (see account.go).
	sessionIdle = 7 * 24 * time.Hour
	sessionMax  = 30 * 24 * time.Hour
	// maxSessions bounds the sessions held at once. One user needs a handful;
	// the bound is for the default login, which anyone can use until setup.
	maxSessions = 64
	// cookieResend is how often the cookie is re-sent to slide its Max-Age
	// along with the server-side idle window, rather than on every request.
	cookieResend = time.Hour
	// hashSlots bounds the password hashes running at once. Each argon2id hash
	// takes 19 MiB, and a login costs one before anyone is known.
	hashSlots = 4
)

// session is one logged-in browser. gen is the auth row's generation when it
// was issued: a password change or disable moves the row on, and every session
// from before stops matching.
type session struct {
	gen      string
	created  time.Time
	lastSeen time.Time
	sentAt   time.Time
}

// sessionStore holds sessions in memory only, keyed by the token's SHA-256 so
// the raw tokens live only in the browsers holding them.
type sessionStore struct {
	mu   sync.Mutex
	now  func() time.Time
	byID map[[sha256.Size]byte]*session
}

func newSessionStore() *sessionStore {
	return &sessionStore{now: time.Now, byID: map[[sha256.Size]byte]*session{}}
}

func (s *session) expired(now time.Time) bool {
	return now.Sub(s.lastSeen) > sessionIdle || now.Sub(s.created) > sessionMax
}

// create starts a session under gen and returns its token.
func (ss *sessionStore) create(gen string) (string, error) {
	token, _, err := ss.createSince(gen, time.Time{})
	return token, err
}

// createSince starts a session carrying on a login made at since, which a
// restored "stay signed in" cookie dates from; zero means now. It returns the
// token and the login time it recorded. At the bound, the session seen longest
// ago makes room.
func (ss *sessionStore) createSince(gen string, since time.Time) (string, time.Time, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", time.Time{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw[:])
	ss.mu.Lock()
	defer ss.mu.Unlock()
	now := ss.now()
	var oldest [sha256.Size]byte
	var oldestSeen time.Time
	for k, s := range ss.byID {
		switch {
		case s.expired(now):
			delete(ss.byID, k)
		case oldestSeen.IsZero() || s.lastSeen.Before(oldestSeen):
			oldest, oldestSeen = k, s.lastSeen
		}
	}
	if len(ss.byID) >= maxSessions {
		delete(ss.byID, oldest)
	}
	created := now
	if !since.IsZero() && since.Before(now) {
		created = since
	}
	ss.byID[sha256.Sum256([]byte(token))] = &session{gen: gen, created: created, lastSeen: now, sentAt: now}
	return token, created, nil
}

// check reports whether token is a live session issued under gen, when its
// login happened, and whether its cookie is due to be re-sent. All three come
// from one look under the lock, so a session evicted a moment later can't
// leave the caller with half an answer. A stale or superseded session is
// dropped.
func (ss *sessionStore) check(token, gen string) (since time.Time, ok, resend bool) {
	key := sha256.Sum256([]byte(token))
	ss.mu.Lock()
	defer ss.mu.Unlock()
	s, found := ss.byID[key]
	if !found {
		return time.Time{}, false, false
	}
	now := ss.now()
	if s.gen != gen || s.expired(now) {
		delete(ss.byID, key)
		return time.Time{}, false, false
	}
	s.lastSeen = now
	if now.Sub(s.sentAt) >= cookieResend {
		s.sentAt = now
		resend = true
	}
	return s.created, true, resend
}

func (ss *sessionStore) delete(token string) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	delete(ss.byID, sha256.Sum256([]byte(token)))
}

func (ss *sessionStore) clear() {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	clear(ss.byID)
}

// Login throttling: a few free tries per client, then a lockout that doubles
// with each further miss up to a cap.
const (
	loginFreeTries = 5
	loginLockBase  = 30 * time.Second
	loginLockMax   = 15 * time.Minute
	loginTrackMax  = 1024 // clients tracked at once; past it the quietest is forgotten
)

// loginLimiter counts login attempts per client. Behind a trusted proxy the
// client is the forwarded address; otherwise it is the connection's.
type loginLimiter struct {
	mu    sync.Mutex
	now   func() time.Time
	byKey map[string]*loginFails
}

type loginFails struct {
	count int
	until time.Time
	last  time.Time
	busy  bool // an attempt is being checked right now
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{now: time.Now, byKey: map[string]*loginFails{}}
}

// begin claims key's next attempt. It is refused while the key is locked out
// or already has an attempt being checked, so parallel requests cannot race
// past the count. The attempt counts as a miss from here on; done(key, true)
// takes that back.
func (l *loginLimiter) begin(key string) (wait time.Duration, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	f := l.byKey[key]
	if f == nil {
		if len(l.byKey) >= loginTrackMax {
			l.evictLocked(now)
		}
		f = &loginFails{}
		l.byKey[key] = f
	}
	if d := f.until.Sub(now); d > 0 {
		return d, false
	}
	if f.busy {
		return time.Second, false
	}
	f.busy = true
	f.count++
	f.last = now
	if over := f.count - loginFreeTries; over >= 0 {
		f.until = now.Add(lockout(over))
	}
	return 0, true
}

// done ends the attempt begin claimed. A success forgets the key.
func (l *loginLimiter) done(key string, success bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if success {
		delete(l.byKey, key)
		return
	}
	if f := l.byKey[key]; f != nil {
		f.busy = false
	}
}

// lockout is the wait after the over'th miss past the free tries.
func lockout(over int) time.Duration {
	if over >= 6 { // 30s << 5 already passes the cap
		return loginLockMax
	}
	return min(loginLockBase<<over, loginLockMax)
}

// evictLocked makes room: first every client neither locked out nor mid-attempt
// that has been quiet longer than the longest lockout, then, if that freed
// nothing, the one seen longest ago.
func (l *loginLimiter) evictLocked(now time.Time) {
	var oldest string
	var oldestSeen time.Time
	for key, f := range l.byKey {
		if !f.busy && now.After(f.until) && now.Sub(f.last) > loginLockMax {
			delete(l.byKey, key)
			continue
		}
		if oldestSeen.IsZero() || f.last.Before(oldestSeen) {
			oldest, oldestSeen = key, f.last
		}
	}
	if len(l.byKey) >= loginTrackMax {
		delete(l.byKey, oldest)
	}
}

// limitKey buckets a client for the limiter: an IPv4 address as is, an IPv6
// one by its /64, the block a single host is routinely handed whole.
func limitKey(client string) string {
	a, err := netip.ParseAddr(client)
	if err != nil {
		return client
	}
	if a.Is6() {
		if p, err := a.Prefix(64); err == nil {
			return p.String()
		}
	}
	return a.String()
}

// DefaultTrustedProxies are the addresses whose forwarding headers are believed
// unless configured otherwise: loopback, where a reverse proxy on the same
// machine connects from.
var DefaultTrustedProxies = []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("::1/128"),
}

// SetTrustedProxies replaces the networks whose X-Forwarded-For and
// X-Forwarded-Proto headers are believed. Nil or empty trusts none.
func (s *Web) SetTrustedProxies(nets []netip.Prefix) { s.trusted = nets }

func (s *Web) trusts(a netip.Addr) bool {
	for _, p := range s.trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// peerAddr is the connection's remote address.
func peerAddr(r *http.Request) (netip.Addr, bool) {
	if ap, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
		return ap.Addr().Unmap(), true
	}
	if a, err := netip.ParseAddr(r.RemoteAddr); err == nil {
		return a.Unmap(), true
	}
	return netip.Addr{}, false
}

// clientAddr is who a request came from: the connection's peer, or, when that
// peer is a trusted proxy, the nearest X-Forwarded-For hop that is not one.
// Anyone else's forwarding headers are ignored, since any client can set them.
func (s *Web) clientAddr(r *http.Request) string {
	peer, ok := peerAddr(r)
	if !ok {
		return r.RemoteAddr
	}
	client := peer
	if s.trusts(peer) {
		hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
		for i := len(hops) - 1; i >= 0 && s.trusts(client); i-- {
			a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
			if err != nil {
				break
			}
			client = a.Unmap()
		}
	}
	return client.String()
}

// isHTTPS reports whether the browser reached us over TLS: directly, or
// through a trusted proxy that says it terminated TLS.
func (s *Web) isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	peer, ok := peerAddr(r)
	if !ok || !s.trusts(peer) {
		return false
	}
	proto, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Proto"), ",")
	return strings.EqualFold(strings.TrimSpace(proto), "https")
}

// tryHash claims one of the password-hash slots, or reports that all are busy.
func (s *Web) tryHash() (release func(), ok bool) {
	select {
	case s.hashing <- struct{}{}:
		return func() { <-s.hashing }, true
	default:
		return nil, false
	}
}

// gatePublic reports the paths the gate lets through without a session: the
// screens that establish one, and the assets they are drawn with.
func gatePublic(path string) bool {
	return path == "/login" || path == "/logout" || strings.HasPrefix(path, "/static/")
}

// hstsYear pins HTTPS for a year. Sent when the request came over HTTPS, served
// directly or through a trusted proxy that says so, and never for localhost or
// an IP address, where it would outlive a test certificate and break plain HTTP
// on the same name.
const hstsYear = "max-age=31536000"

func hstsHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(host, ".")
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return false
	}
	_, err := netip.ParseAddr(strings.Trim(host, "[]"))
	return err != nil
}

// Gate wraps the whole app, the JSON API included, in the login. Unless the
// owner switched it off, a request needs a live session, and one made with the
// default login is held on the setup page until a real password is set.
// It fails closed: if the auth state can't be read, nothing is served.
func (s *Web) Gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		if s.isHTTPS(r) && hstsHost(r.Host) {
			w.Header().Set("Strict-Transport-Security", hstsYear)
		}
		if gatePublic(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		st, err := s.db.AuthState(r.Context())
		if err != nil {
			log.Error().Err(err).Msg("reading auth state")
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if st.Mode == store.AuthDisabled {
			info := authInfo{Mode: st.Mode, Username: st.Username}
			next.ServeHTTP(w, r.WithContext(withAuth(r.Context(), info)))
			return
		}
		// Nothing behind a login is cached, so the back button after a logout
		// shows the login rather than the last page.
		w.Header().Set("Cache-Control", "private, no-store")
		token, since, ok := s.hasSession(w, r, st)
		if !ok {
			deny(w, r)
			return
		}
		if st.Mode == store.AuthInitial && r.URL.Path != "/setup" {
			redirect(w, r, "/setup")
			return
		}
		info := authInfo{
			Mode: st.Mode, Username: st.Username, Gen: st.Gen,
			Token: token, Since: since, Remembered: s.remembered(r, st),
		}
		next.ServeHTTP(w, r.WithContext(withAuth(r.Context(), info)))
	})
}

// hasSession finds this browser's session and returns its token and when its
// login happened: a live one from the session cookie, re-sent when its sliding
// Max-Age is due, or one picked back up from a "stay signed in" cookie after a
// restart.
func (s *Web) hasSession(w http.ResponseWriter, r *http.Request, st store.AuthState) (string, time.Time, bool) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		if since, ok, resend := s.sessions.check(c.Value, st.Gen); ok {
			if resend {
				s.setSessionCookie(w, r, c.Value)
			}
			return c.Value, since, true
		}
	}
	return s.restore(w, r, st)
}

// deny turns away a request without a session in the shape its caller can
// act on. htmx gets a full-page redirect rather than a login screen swapped
// into a fragment; a page load goes to the login and comes back after.
func deny(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Header.Get("HX-Request") == "true":
		w.Header().Set("HX-Redirect", "/login"+nextQuery(htmxPage(r)))
		unauthorized(w)
		w.WriteHeader(http.StatusUnauthorized)
	case (r.Method == http.MethodGet || r.Method == http.MethodHead) && !strings.HasPrefix(r.URL.Path, "/api/"):
		http.Redirect(w, r, "/login"+nextQuery(r.URL.RequestURI()), http.StatusSeeOther)
	default:
		unauthorized(w)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}
}

// unauthorized names the scheme a 401 expects, as the status requires: the
// session cookie the login page hands out.
func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Cookie realm="rabbithole"`)
}

// redirect sends the browser to target, as a full-page navigation for htmx.
func redirect(w http.ResponseWriter, r *http.Request, target string) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// htmxPage is the page an htmx request was made from, which is where the login
// should return to (the request's own URL is only a fragment).
func htmxPage(r *http.Request) string {
	u, err := url.Parse(r.Header.Get("HX-Current-URL"))
	if err != nil {
		return ""
	}
	return u.RequestURI()
}

func nextQuery(target string) string {
	target = safeNext(target)
	if target == "/" {
		return ""
	}
	return "?next=" + url.QueryEscape(target)
}

// safeNext keeps a post-login redirect on this site: a path, never a URL with
// a host of its own, and never back to the auth screens.
func safeNext(target string) string {
	if target == "" || target[0] != '/' || strings.HasPrefix(target, "//") || strings.HasPrefix(target, "/\\") {
		return "/"
	}
	u, err := url.Parse(target)
	if err != nil || u.IsAbs() || u.Host != "" {
		return "/"
	}
	switch u.Path {
	case "/login", "/logout", "/setup":
		return "/"
	}
	return target
}

func (s *Web) setSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   int(sessionIdle / time.Second),
		HttpOnly: true,
		Secure:   s.isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Web) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	s.clearCookie(w, r, sessionCookie)
}

func (s *Web) clearCookie(w http.ResponseWriter, r *http.Request, name string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.isHTTPS(r), SameSite: http.SameSiteLaxMode,
	})
}

// plainRemote reports a login over unencrypted HTTP to anything but this
// machine, where the password crosses the network readable.
func (s *Web) plainRemote(r *http.Request) bool {
	if s.isHTTPS(r) {
		return false
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "localhost" {
		return false
	}
	ip := net.ParseIP(host)
	return ip == nil || !ip.IsLoopback()
}

// gateData drives the login and setup screens.
type gateData struct {
	Step     string // "login" or "setup"
	Initial  bool   // the default login is still the one that works
	Username string
	Next     string
	Error    string
	BadLogin bool // the error is a failed login, worded per layout by the template
	Plain    bool // unencrypted HTTP from another machine
}

func (s *Web) renderGate(w http.ResponseWriter, r *http.Request, status int, data gateData) {
	data.Plain = s.plainRemote(r)
	var buf bytes.Buffer
	if err := gateTmpl.ExecuteTemplate(&buf, "gate", data); err != nil {
		log.Error().Err(err).Str("step", data.Step).Msg("rendering auth screen")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// busy answers a request that found every hash slot taken.
func (s *Web) busy(w http.ResponseWriter, r *http.Request, data gateData) {
	w.Header().Set("Retry-After", "1")
	data.Error = "busy checking other logins, try again in a moment"
	s.renderGate(w, r, http.StatusServiceUnavailable, data)
}

// authState reads the gate's state for the auth screens, which sit outside the
// gate and so read it themselves.
func (s *Web) authState(w http.ResponseWriter, r *http.Request) (store.AuthState, bool) {
	st, err := s.db.AuthState(r.Context())
	if err != nil {
		log.Error().Err(err).Msg("reading auth state")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return st, false
	}
	return st, true
}

// handleLoginPage shows the login, or skips it when there is nothing to do. A
// cookie that no longer names a session is cleared on the way.
func (s *Web) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	st, ok := s.authState(w, r)
	if !ok {
		return
	}
	if st.Mode == store.AuthDisabled {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if _, _, ok := s.hasSession(w, r, st); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	for _, name := range []string{sessionCookie, rememberCookie} {
		if _, err := r.Cookie(name); err == nil {
			s.clearCookie(w, r, name)
		}
	}
	s.renderGate(w, r, http.StatusOK, gateData{
		Step: "login", Initial: st.Mode == store.AuthInitial, Next: safeNext(r.URL.Query().Get("next")),
	})
}

// handleLogin checks a login. Only the client's address and the outcome are
// logged: never the username, which is where a password gets typed by mistake.
func (s *Web) handleLogin(w http.ResponseWriter, r *http.Request) {
	st, ok := s.authState(w, r)
	if !ok {
		return
	}
	if st.Mode == store.AuthDisabled {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	client := s.clientAddr(r)
	key := limitKey(client)
	data := gateData{
		Step: "login", Initial: st.Mode == store.AuthInitial,
		Username: r.PostFormValue("username"), Next: safeNext(r.PostFormValue("next")),
	}
	release, ok := s.tryHash()
	if !ok {
		s.busy(w, r, data)
		return
	}
	defer release()
	if wait, ok := s.limiter.begin(key); !ok {
		secs := int((wait + time.Second - 1) / time.Second)
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		data.Error = "too many attempts, try again in " + waitPhrase(wait)
		s.renderGate(w, r, http.StatusTooManyRequests, data)
		return
	}
	st, err := s.db.VerifyLogin(r.Context(), r.PostFormValue("username"), r.PostFormValue("password"))
	s.limiter.done(key, err == nil)
	if errors.Is(err, store.ErrBadCredentials) {
		log.Warn().Str("ip", client).Msg("login failed")
		data.Error, data.BadLogin = "wrong username or password", true
		s.renderGate(w, r, http.StatusUnauthorized, data)
		return
	}
	if err != nil {
		log.Error().Err(err).Msg("checking login")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !s.startSession(w, r, st.Gen) {
		return
	}
	log.Info().Str("ip", client).Msg("login")
	dest := data.Next
	if st.Mode == store.AuthInitial {
		dest = "/setup"
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// startSession issues a session under gen and hands the browser its cookie.
func (s *Web) startSession(w http.ResponseWriter, r *http.Request, gen string) bool {
	token, err := s.sessions.create(gen)
	if err != nil {
		log.Error().Err(err).Msg("creating session")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return false
	}
	s.setSessionCookie(w, r, token)
	return true
}

// handleLogout ends this browser's session and forgets its "stay signed in"
// cookie. It is outside the gate, so a logout with an already-dead session
// still lands on the login.
func (s *Web) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.delete(c.Value)
	}
	s.clearSessionCookie(w, r)
	s.clearCookie(w, r, rememberCookie)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// handleSetupPage asks for a real password: right after the default login, or
// when an open instance is being locked. A gate that already has a password
// has nothing to set up here; changing it goes through `rabbithole auth reset`.
func (s *Web) handleSetupPage(w http.ResponseWriter, r *http.Request) {
	st, ok := s.authState(w, r)
	if !ok {
		return
	}
	if st.Mode == store.AuthEnabled {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.renderGate(w, r, http.StatusOK, gateData{
		Step: "setup", Initial: st.Mode == store.AuthInitial, Username: st.Username,
	})
}

// handleSetup either sets the password or, from the default login only, leaves
// the instance open. Both write only over the state this request read, so two
// setup pages racing cannot undo each other, and both end every other session.
func (s *Web) handleSetup(w http.ResponseWriter, r *http.Request) {
	st, ok := s.authState(w, r)
	if !ok {
		return
	}
	if st.Mode == store.AuthEnabled {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	ctx := r.Context()
	if r.PostFormValue("action") == "open" {
		if st.Mode == store.AuthInitial {
			err := s.db.DisableAuthIf(ctx, st.Gen)
			if err != nil && !errors.Is(err, store.ErrAuthChanged) {
				log.Error().Err(err).Msg("disabling auth")
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			if err == nil {
				s.sessions.clear()
				s.clearSessionCookie(w, r)
				log.Info().Str("ip", s.clientAddr(r)).Msg("login gate switched off")
			}
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	username := r.PostFormValue("username")
	data := gateData{Step: "setup", Initial: st.Mode == store.AuthInitial, Username: username}
	if r.PostFormValue("password") != r.PostFormValue("confirm") {
		data.Error = "the two passwords don't match"
		s.renderGate(w, r, http.StatusBadRequest, data)
		return
	}
	release, ok := s.tryHash()
	if !ok {
		s.busy(w, r, data)
		return
	}
	err := s.db.SetPasswordIf(ctx, st.Gen, username, r.PostFormValue("password"))
	release()
	switch {
	case errors.Is(err, store.ErrInvalidAuth):
		data.Error = strings.TrimPrefix(err.Error(), store.ErrInvalidAuth.Error()+": ")
		s.renderGate(w, r, http.StatusBadRequest, data)
		return
	case errors.Is(err, store.ErrAuthChanged):
		// Someone else set it first; the gate routes this browser from here.
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	case err != nil:
		log.Error().Err(err).Msg("setting password")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.sessions.clear()
	fresh, ok := s.authState(w, r)
	if !ok || !s.startSession(w, r, fresh.Gen) {
		return
	}
	log.Info().Str("ip", s.clientAddr(r)).Msg("password set")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// waitPhrase renders a lockout as "45s" or "3 min", rounding up.
func waitPhrase(d time.Duration) string {
	if d < time.Minute {
		return strconv.Itoa(int((d+time.Second-1)/time.Second)) + "s"
	}
	return strconv.Itoa(int((d+time.Minute-1)/time.Minute)) + " min"
}
