// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/config"
)

// restart is the same store behind a new process: every in-memory session gone.
func restart(t *testing.T, w *Web, b ...*browser) *Web {
	t.Helper()
	fresh := New(w.db, &config.Config{}, "", testIngestManager(t, w.db))
	for _, br := range b {
		br.h = fresh.Gate(fresh.Routes())
	}
	return fresh
}

func setRemember(t *testing.T, b *browser, on bool) *httptest.ResponseRecorder {
	t.Helper()
	v := "0"
	if on {
		v = "1"
	}
	return b.do(http.MethodPost, "/account/remember", url.Values{"on": {v}}, "HX-Request", "true")
}

func TestStaySignedInSurvivesARestart(t *testing.T) {
	w := gatedWeb(t)
	kept, plain := newBrowser(t, w), newBrowser(t, w)
	kept.login("alice", "correct horse")
	plain.login("alice", "correct horse")

	res := setRemember(t, kept, true)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "is-on") || kept.remember == nil {
		t.Fatalf("turning it on = %d, cookie %v; body %s", res.Code, kept.remember, res.Body)
	}
	if !kept.remember.HttpOnly || kept.remember.SameSite != http.SameSiteLaxMode {
		t.Errorf("remember cookie = %+v, want HttpOnly and SameSite=Lax", kept.remember)
	}

	restart(t, w, kept, plain)
	wantStatus(t, kept.get("/feed"), http.StatusOK)
	if kept.cookie == nil {
		t.Error("a restored browser was not given a session cookie")
	}
	wantRedirect(t, plain.get("/feed"), "/login?next=%2Ffeed")

	// Turning it off means the next restart asks again.
	res = setRemember(t, kept, false)
	if res.Code != http.StatusOK || kept.remember != nil {
		t.Fatalf("turning it off = %d, cookie %v", res.Code, kept.remember)
	}
	restart(t, w, kept)
	wantRedirect(t, kept.get("/feed"), "/login?next=%2Ffeed")
}

func TestLogOutEverywhere(t *testing.T) {
	w := gatedWeb(t)
	me, other := newBrowser(t, w), newBrowser(t, w)
	me.login("alice", "correct horse")
	other.login("alice", "correct horse")
	setRemember(t, me, true)
	setRemember(t, other, true)

	rec := me.do(http.MethodPost, "/account/everywhere", nil, "HX-Request", "true")
	wantStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), "Every other browser is logged out") {
		t.Errorf("no confirmation in %s", rec.Body)
	}
	wantStatus(t, me.get("/feed"), http.StatusOK)
	wantRedirect(t, other.get("/feed"), "/login?next=%2Ffeed")

	// Both halves: this browser's own "stay signed in" came through, the
	// other's cookie is dead even after a restart.
	restart(t, w, me, other)
	wantStatus(t, me.get("/feed"), http.StatusOK)
	wantRedirect(t, other.get("/feed"), "/login?next=%2Ffeed")
}

func TestLogoutAndNewPasswordEndStaySignedIn(t *testing.T) {
	w := gatedWeb(t)
	b := newBrowser(t, w)
	b.login("alice", "correct horse")
	setRemember(t, b, true)
	stolen := *b.remember

	wantRedirect(t, b.do(http.MethodPost, "/logout", nil), "/login")
	if b.remember != nil {
		t.Fatal("logout left the remember cookie in place")
	}

	// A copy taken before the logout still works until the gen moves on...
	thief := newBrowser(t, w)
	thief.remember = &stolen
	wantStatus(t, thief.get("/feed"), http.StatusOK)

	// ...which a new password does, for every copy.
	if err := w.db.SetPassword(context.Background(), "alice", "battery staple"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	late := newBrowser(t, w)
	late.remember = &stolen
	wantRedirect(t, late.get("/feed"), "/login?next=%2Ffeed")
	if late.remember != nil {
		t.Error("a dead remember cookie was not cleared")
	}
}

// A password reset that lands after the gate let a request in wins: the
// Account action turns the browser away rather than re-issuing it a session or
// a "stay signed in" cookie under the reset's gen.
func TestAccountActionsLoseToAResetAfterTheGate(t *testing.T) {
	for _, path := range []string{"/account/everywhere", "/account/remember"} {
		t.Run(path, func(t *testing.T) {
			w := gatedWeb(t)
			b := newBrowser(t, w)
			b.login("alice", "correct horse")
			ctx, routes := context.Background(), w.Routes()
			var resetGen string
			b.h = w.Gate(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
				if err := w.db.SetPassword(ctx, "alice", "battery staple"); err != nil {
					t.Fatalf("SetPassword: %v", err)
				}
				st, _ := w.db.AuthState(ctx)
				resetGen = st.Gen
				routes.ServeHTTP(rw, r)
			}))

			rec := b.do(http.MethodPost, path, url.Values{"on": {"1"}}, "HX-Request", "true")
			wantStatus(t, rec, http.StatusUnauthorized)
			if !strings.HasPrefix(rec.Header().Get("HX-Redirect"), "/login") {
				t.Errorf("HX-Redirect = %q, want the login", rec.Header().Get("HX-Redirect"))
			}
			if st, _ := w.db.AuthState(ctx); st.Gen != resetGen {
				t.Errorf("gen = %q after the action, want the reset's %q", st.Gen, resetGen)
			}
			for _, c := range rec.Result().Cookies() {
				if c.Value != "" {
					t.Errorf("the action issued %s after the reset", c.Name)
				}
			}
		})
	}
}

func TestRememberCookieVerification(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	now := time.Now()
	good := signRemember(key, "gen-1", now)

	if issued, ok := readRemember(key, "gen-1", good, now); !ok || issued.Unix() != now.Unix() {
		t.Fatalf("a fresh cookie = %v, %v", issued, ok)
	}
	payload, sig, _ := strings.Cut(good, ".")
	forged := rememberB64.EncodeToString([]byte(strings.Replace(
		mustDecode(t, payload), "gen-1", "gen-2", 1))) + "." + sig
	tests := map[string]struct {
		key   []byte
		gen   string
		value string
		at    time.Time
	}{
		"other key":      {[]byte("fedcba9876543210fedcba9876543210"), "gen-1", good, now},
		"retired gen":    {key, "gen-2", good, now},
		"expired":        {key, "gen-1", good, now.Add(rememberFor + time.Second)},
		"edited payload": {key, "gen-2", forged, now},
		"no signature":   {key, "gen-1", payload, now},
		"garbage":        {key, "gen-1", "!!.??", now},
		"no key":         {nil, "gen-1", good, now},
	}
	for name, tt := range tests {
		if _, ok := readRemember(tt.key, tt.gen, tt.value, tt.at); ok {
			t.Errorf("%s: accepted", name)
		}
	}
}

func mustDecode(t *testing.T, s string) string {
	t.Helper()
	b, err := rememberB64.DecodeString(s)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return string(b)
}

func TestAccountSectionFollowsTheMode(t *testing.T) {
	w := gatedWeb(t)
	b := newBrowser(t, w)
	b.login("alice", "correct horse")
	body := b.get("/feed").Body.String()
	for _, want := range []string{
		`id="stgAccount"`, `class="acct__name">alice`, "signed in just now",
		"Stay signed in after a restart", "log out everywhere", "rabbithole auth reset", `action="/logout"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("account section lacks %q", want)
		}
	}
	// The Account tab's button, and the settings footer's shortcut.
	if n := strings.Count(body, `action="/logout"`); n != 2 {
		t.Errorf("page has %d log out forms, want 2", n)
	}

	if err := w.db.DisableAuth(context.Background()); err != nil {
		t.Fatalf("DisableAuth: %v", err)
	}
	open := newBrowser(t, w)
	body = open.get("/feed").Body.String()
	for _, want := range []string{"Open instance", `href="/setup"`} {
		if !strings.Contains(body, want) {
			t.Errorf("open account section lacks %q", want)
		}
	}
	for _, unwanted := range []string{"log out everywhere", "Stay signed in", `action="/logout"`} {
		if strings.Contains(body, unwanted) {
			t.Errorf("open account section offers %q", unwanted)
		}
	}
	// With no login there is nothing for the account actions to change.
	wantStatus(t, open.do(http.MethodPost, "/account/remember", url.Values{"on": {"1"}}), http.StatusConflict)
	wantStatus(t, open.do(http.MethodPost, "/account/everywhere", nil), http.StatusConflict)
}

func TestNoGateMeansNoAccountTab(t *testing.T) {
	w := newTestWeb(t)
	rec := getPage(t, w, "/feed")
	if strings.Contains(rec, `id="stgAccount"`) {
		t.Error("the account tab rendered with no gate in front")
	}
}

func getPage(t *testing.T, w *Web, path string) string {
	t.Helper()
	b := &browser{t: t, h: w.Routes(), header: http.Header{}}
	rec := b.get(path)
	wantStatus(t, rec, http.StatusOK)
	return rec.Body.String()
}
