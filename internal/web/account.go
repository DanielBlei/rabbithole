// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/DanielBlei/rabbithole/internal/store"
)

// A "stay signed in" cookie outlives the process that issued it: sessions live
// in memory, and this is how a remembered browser gets past a restart. It is a
// payload and an HMAC-SHA256 of it under the auth row's signing key, so the
// server needs no list of them. The payload carries the gen, so whatever
// retires sessions (a new password, log out everywhere) retires these too.
const (
	rememberCookie = "rh_remember"
	rememberFor    = sessionMax
)

var rememberB64 = base64.RawURLEncoding

// signRemember is "v1|<gen>|<issued>|<expires>" (Unix seconds) and its MAC,
// each base64url, joined by a dot.
func signRemember(key []byte, gen string, issued time.Time) string {
	payload := fmt.Sprintf("v1|%s|%d|%d", gen, issued.Unix(), issued.Add(rememberFor).Unix())
	return rememberB64.EncodeToString([]byte(payload)) + "." + rememberB64.EncodeToString(rememberMAC(key, payload))
}

func rememberMAC(key []byte, payload string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	return mac.Sum(nil)
}

// readRemember verifies a cookie from signRemember against the current key and
// gen, and returns when its login happened.
func readRemember(key []byte, gen, value string, now time.Time) (time.Time, bool) {
	if len(key) == 0 {
		return time.Time{}, false
	}
	p64, s64, found := strings.Cut(value, ".")
	if !found {
		return time.Time{}, false
	}
	payload, err := rememberB64.DecodeString(p64)
	if err != nil {
		return time.Time{}, false
	}
	sig, err := rememberB64.DecodeString(s64)
	if err != nil || !hmac.Equal(sig, rememberMAC(key, string(payload))) {
		return time.Time{}, false
	}
	parts := strings.Split(string(payload), "|")
	if len(parts) != 4 || parts[0] != "v1" || parts[1] != gen {
		return time.Time{}, false
	}
	issued, err1 := strconv.ParseInt(parts[2], 10, 64)
	expires, err2 := strconv.ParseInt(parts[3], 10, 64)
	if err1 != nil || err2 != nil || now.Unix() >= expires {
		return time.Time{}, false
	}
	return time.Unix(issued, 0), true
}

func (s *Web) setRememberCookie(w http.ResponseWriter, r *http.Request, key []byte, gen string, issued time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     rememberCookie,
		Value:    signRemember(key, gen, issued),
		Path:     "/",
		MaxAge:   int(time.Until(issued.Add(rememberFor)) / time.Second),
		HttpOnly: true,
		Secure:   s.isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// restore picks a session back up from a "stay signed in" cookie. The session
// dates from the cookie's login, so it ends when the cookie would have. A
// cookie that no longer verifies is cleared.
func (s *Web) restore(w http.ResponseWriter, r *http.Request, st store.AuthState) (string, time.Time, bool) {
	c, err := r.Cookie(rememberCookie)
	if err != nil || c.Value == "" {
		return "", time.Time{}, false
	}
	issued, ok := readRemember(st.SigningKey, st.Gen, c.Value, s.sessions.now())
	if !ok || st.Mode != store.AuthEnabled {
		s.clearCookie(w, r, rememberCookie)
		return "", time.Time{}, false
	}
	token, since, err := s.sessions.createSince(st.Gen, issued)
	if err != nil {
		log.Error().Err(err).Msg("restoring a remembered session")
		return "", time.Time{}, false
	}
	s.setSessionCookie(w, r, token)
	return token, since, true
}

// remembered reports whether this browser holds a "stay signed in" cookie that
// still verifies.
func (s *Web) remembered(r *http.Request, st store.AuthState) bool {
	c, err := r.Cookie(rememberCookie)
	if err != nil {
		return false
	}
	_, ok := readRemember(st.SigningKey, st.Gen, c.Value, s.sessions.now())
	return ok
}

// authInfo is what the gate learned about a request, for the handlers behind it.
type authInfo struct {
	Mode       store.AuthMode // "" when no gate ran
	Username   string
	Gen        string // the auth row's gen the session was checked against
	Token      string // this browser's session
	Since      time.Time
	Remembered bool
}

type authCtxKey struct{}

func withAuth(ctx context.Context, info authInfo) context.Context {
	return context.WithValue(ctx, authCtxKey{}, info)
}

func authFrom(ctx context.Context) authInfo {
	info, _ := ctx.Value(authCtxKey{}).(authInfo)
	return info
}

// accountData drives the Settings → Account section.
type accountData struct {
	Mode       string // "enabled" or "disabled"; "" hides the section
	Username   string
	Since      string // "signed in 2h ago"
	Remembered bool
	Note       string // what the last action did
}

func accountView(info authInfo, note string) accountData {
	d := accountData{Mode: string(info.Mode), Username: info.Username, Remembered: info.Remembered, Note: note}
	if !info.Since.IsZero() {
		d.Since = "signed in " + agoPhrase(info.Since, time.Now())
	}
	return d
}

func (s *Web) renderAccount(w http.ResponseWriter, info authInfo, note string) {
	if err := feedTmpl.ExecuteTemplate(w, "accountSect", accountView(info, note)); err != nil {
		log.Error().Err(err).Msg("rendering account section")
	}
}

// accountState is the request's gate info and the auth row behind it, for the
// Account actions, which only apply to a login with a password. A row whose gen
// moved on after the gate let the request in (a password reset in between)
// retired this session, so the request is turned away as logged out.
func (s *Web) accountState(w http.ResponseWriter, r *http.Request) (authInfo, store.AuthState, bool) {
	info := authFrom(r.Context())
	st, ok := s.authState(w, r)
	if !ok {
		return info, st, false
	}
	if info.Mode != store.AuthEnabled || st.Mode != store.AuthEnabled {
		http.Error(w, "there is no login to change", http.StatusConflict)
		return info, st, false
	}
	if st.Gen != info.Gen {
		deny(w, r)
		return info, st, false
	}
	return info, st, true
}

// handleRemember switches "stay signed in" for this browser: on signs a cookie
// under the current gen, off clears it.
func (s *Web) handleRemember(w http.ResponseWriter, r *http.Request) {
	info, st, ok := s.accountState(w, r)
	if !ok {
		return
	}
	info.Remembered = r.PostFormValue("on") == "1"
	note := "This browser asks again after a restart."
	if info.Remembered {
		s.setRememberCookie(w, r, st.SigningKey, st.Gen, info.Since)
		note = "This browser stays signed in through restarts, for up to 30 days."
	} else {
		s.clearCookie(w, r, rememberCookie)
	}
	s.renderAccount(w, info, note)
}

// handleEverywhere retires every session and "stay signed in" cookie, then
// signs this browser back in under the new gen, keeping its own choice. The
// retire is conditional on the gen this session was checked against, so a
// password reset that lands first wins and this browser stays logged out.
func (s *Web) handleEverywhere(w http.ResponseWriter, r *http.Request) {
	info, st, ok := s.accountState(w, r)
	if !ok {
		return
	}
	gen, err := s.db.RetireSessionsIf(r.Context(), info.Gen)
	if errors.Is(err, store.ErrAuthChanged) {
		deny(w, r)
		return
	}
	if err != nil {
		log.Error().Err(err).Msg("logging out everywhere")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.sessions.clear()
	token, _, err := s.sessions.createSince(gen, info.Since)
	if err != nil {
		log.Error().Err(err).Msg("creating session")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.setSessionCookie(w, r, token)
	if info.Remembered {
		s.setRememberCookie(w, r, st.SigningKey, gen, info.Since)
	}
	info.Token = token
	log.Info().Str("ip", s.clientAddr(r)).Msg("logged out everywhere")
	s.renderAccount(w, info, "Every other browser is logged out.")
}
