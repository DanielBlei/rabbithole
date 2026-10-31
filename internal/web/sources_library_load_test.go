// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/library"
)

const navGroupMarker = `<details class="sh-nav__grp"`

// countingLibrary serves the real library files and counts the requests for
// rss.yaml, one per load. gate, when set, holds every request until it closes.
type countingLibrary struct {
	srv   *httptest.Server
	loads atomic.Int32
	gate  chan struct{}
	// canceled is set when a held request resumes with its context done.
	canceled atomic.Bool
}

func newCountingLibrary(t *testing.T, gate chan struct{}) *countingLibrary {
	t.Helper()
	c := &countingLibrary{gate: gate}
	files := http.FileServer(http.Dir("../../library"))
	c.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rss.yaml" {
			c.loads.Add(1)
		}
		if c.gate != nil {
			<-c.gate
			if r.Context().Err() != nil {
				c.canceled.Store(true)
			}
		}
		files.ServeHTTP(rw, r)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *countingLibrary) web(t *testing.T) *Web {
	t.Helper()
	return c.webWith(t, config.FeedsDoc{})
}

func (c *countingLibrary) webWith(t *testing.T, doc config.FeedsDoc) *Web {
	t.Helper()
	w, _ := sourcesTestWeb(t, doc)
	w.SetLibrary(libraryLoader(c.srv))
	return w
}

// A page never waits for the library: with the first load held, it renders at
// once with the nav placeholder (and the Overview one), and the load it started
// is not repeated by later visits.
func TestSourcesPageRendersWithoutWaitingForTheLibrary(t *testing.T) {
	gate := make(chan struct{})
	c := newCountingLibrary(t, gate)
	w := c.webWith(t, config.FeedsDoc{Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}}})
	t.Cleanup(func() { close(gate) })

	for _, path := range []string{"/sources", "/sources/manage", "/ingestion"} {
		start := time.Now()
		out := get(t, w, path)
		if d := time.Since(start); d > w.libraryWait/2 {
			t.Errorf("%s took %v with the library held, want no wait", path, d)
		}
		if strings.Contains(out, navGroupMarker) {
			t.Errorf("%s lists groups of a library that has not loaded", path)
		}
		if !regexp.MustCompile(`id="srcLibNav" hx-get="/sources/libfill\?part=nav[^"]*try=1"`).MatchString(out) {
			t.Errorf("%s has no nav placeholder", path)
		}
	}
	if out := get(t, w, "/sources"); !strings.Contains(out, `id="ovLibPending"`) ||
		!strings.Contains(out, "part=overview") {
		t.Error("the Overview has no Library placeholder")
	}
	waitFor(t, func() bool { return c.loads.Load() == 1 })
	if n := c.loads.Load(); n != 1 {
		t.Errorf("loads = %d, want the one load every visit shares", n)
	}
}

// The placeholder route answers from the cache: the placeholder again, with
// the next try, while nothing has loaded, and the filled fragment once it has.
func TestLibraryFillRoute(t *testing.T) {
	gate := make(chan struct{})
	c := newCountingLibrary(t, gate)
	w := c.webWith(t, config.FeedsDoc{
		Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed", Tags: []string{"go"}}},
	})

	nav := get(t, w, "/sources/libfill?part=nav&section=manage&try=1")
	if strings.Contains(nav, navGroupMarker) || !strings.Contains(nav, "try=2") {
		t.Errorf("held library: nav = %s, want the placeholder with try=2", nav)
	}
	ov := get(t, w, "/sources/libfill?part=overview&try=1")
	if !strings.Contains(ov, `id="ovLibPending"`) || !strings.Contains(ov, "try=2") {
		t.Errorf("held library: overview = %s, want the placeholder with try=2", ov)
	}

	close(gate)
	waitFor(t, func() bool { return w.library.Snapshot().Loaded })
	nav = get(t, w, "/sources/libfill?part=nav&section=library&group=blogs&try=2")
	for _, want := range []string{navGroupMarker, `id="srcLibNav"`, `href="/sources/library/blogs"`} {
		if !strings.Contains(nav, want) {
			t.Errorf("loaded library: nav missing %q", want)
		}
	}
	if strings.Contains(nav, "hx-get") {
		t.Error("a filled nav still asks to be filled")
	}
	ov = get(t, w, "/sources/libfill?part=overview&try=2")
	for _, want := range []string{"Library suggestions", "Library coverage"} {
		if !strings.Contains(ov, want) {
			t.Errorf("loaded library: overview missing %q", want)
		}
	}
	if strings.Contains(ov, "hx-get") || strings.Contains(ov, "ovLibPending") {
		t.Error("filled sections still ask to be filled")
	}
}

// A library that never loads is asked for a bounded number of times, then the
// placeholder is dropped.
func TestLibraryFillStopsAfterBoundedTries(t *testing.T) {
	w, _ := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}}})
	down := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		http.Error(rw, "down", http.StatusInternalServerError)
	}))
	t.Cleanup(down.Close)
	w.SetLibrary(libraryLoader(down))
	for _, part := range []string{"nav", "overview"} {
		last := fmt.Sprintf("/sources/libfill?part=%s&try=%d", part, libraryFillTries-1)
		if out := get(t, w, last); !strings.Contains(out, fmt.Sprintf("try=%d", libraryFillTries)) {
			t.Errorf("%s try %d: %s, want one more ask", part, libraryFillTries-1, out)
		}
		final := fmt.Sprintf("/sources/libfill?part=%s&try=%d", part, libraryFillTries)
		if out := get(t, w, final); strings.Contains(out, "hx-get") || strings.Contains(out, "ovLibPending") {
			t.Errorf("%s try %d still asks again: %s", part, libraryFillTries, out)
		}
		// A client sending a huge try is held to the same bound.
		if out := get(t, w, "/sources/libfill?part="+part+"&try=999999"); strings.Contains(out, "hx-get") {
			t.Errorf("%s: an oversized try still asks again", part)
		}
	}
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sources/libfill?part=nope", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown part: status = %d, want 400", rec.Code)
	}
}

// Only a library that can still load draws a placeholder: one that is off, or
// has nothing to fetch from, does not.
func TestPlaceholderOnlyWhileLoadable(t *testing.T) {
	doc := config.FeedsDoc{Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}}}
	gate := make(chan struct{})
	held := newCountingLibrary(t, gate)
	t.Cleanup(func() { close(gate) })
	cases := []struct {
		name   string
		w      *Web
		expect bool
	}{
		{"not loaded yet", held.webWith(t, doc), true},
		{"no source", func() *Web { w, _ := sourcesTestWeb(t, doc); return w }(), false},
		{"off", func() *Web {
			w, _ := sourcesTestWeb(t, doc)
			w.SetLibrary(library.NewLoader(library.Options{BaseURL: "off"}))
			return w
		}(), false},
	}
	for _, tc := range cases {
		for _, path := range []string{"/sources", "/sources/manage"} {
			if got := strings.Contains(get(t, tc.w, path), "libfill"); got != tc.expect {
				t.Errorf("%s %s: placeholder = %v, want %v", tc.name, path, got, tc.expect)
			}
		}
	}
}

// A library that is off draws no placeholder anywhere.
func TestLibraryOffDrawsNoPlaceholder(t *testing.T) {
	w, _ := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}}})
	w.SetLibrary(library.NewLoader(library.Options{BaseURL: "off"}))
	for _, path := range []string{"/sources", "/sources/manage", "/sources/library"} {
		if out := get(t, w, path); strings.Contains(out, "libfill") {
			t.Errorf("%s draws a placeholder for a library that is off", path)
		}
	}
}

func TestSourcesPageFreshLibraryIsNotLoadedAgain(t *testing.T) {
	c := newCountingLibrary(t, nil)
	w := c.web(t)
	w.library.Get(t.Context())
	for _, path := range []string{"/sources", "/sources/manage"} {
		get(t, w, path)
	}
	if n := c.loads.Load(); n != 1 {
		t.Errorf("loads = %d, want the single one before the visits", n)
	}
}

func TestSourcesPageConcurrentVisitsLoadOnce(t *testing.T) {
	c := newCountingLibrary(t, nil)
	w := c.web(t)
	h := w.Routes()
	var wg sync.WaitGroup
	var bad atomic.Int32
	for i := range 8 {
		path := "/sources"
		if i%2 == 1 {
			path = "/sources/manage"
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code != http.StatusOK {
				bad.Add(1)
			}
		}()
	}
	wg.Wait()
	if n := bad.Load(); n != 0 {
		t.Errorf("%d visits failed", n)
	}
	waitFor(t, func() bool { return w.library.Snapshot().Loaded })
	if n := c.loads.Load(); n != 1 {
		t.Errorf("loads = %d, want 1", n)
	}
}

// A visitor who leaves does not cancel the load: it runs on its own context and
// the next visit lists the groups.
func TestSourcesPageCancelDoesNotCancelLoad(t *testing.T) {
	gate := make(chan struct{})
	c := newCountingLibrary(t, gate)
	w := c.web(t)

	ctx, cancel := context.WithCancel(t.Context())
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "/sources/manage", nil))
	waitFor(t, func() bool { return c.loads.Load() == 1 })
	cancel()

	close(gate)
	waitFor(t, func() bool { return w.library.Snapshot().Loaded })
	if c.canceled.Load() {
		t.Error("the load ran on the request context")
	}
	if !strings.Contains(get(t, w, "/sources/manage"), navGroupMarker) {
		t.Error("the next visit does not list the Library groups")
	}
	if n := c.loads.Load(); n != 1 {
		t.Errorf("loads = %d, want 1", n)
	}
}

// A library that cannot load leaves the page working, and one failed load is
// not retried by every visit.
func TestSourcesPageUnreachableLibraryStillRenders(t *testing.T) {
	var loads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		loads.Add(1)
		http.Error(rw, "down", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	w, _ := sourcesTestWeb(t, config.FeedsDoc{})
	w.SetLibrary(libraryLoader(srv))
	for range 3 {
		if out := get(t, w, "/sources/manage"); strings.Contains(out, navGroupMarker) {
			t.Error("nav lists groups of a library that did not load")
		}
	}
	// One load reads each of the three library files.
	waitFor(t, func() bool { return loads.Load() == 3 })
	time.Sleep(50 * time.Millisecond)
	if n := loads.Load(); n != 3 {
		t.Errorf("requests = %d, want one load of every file", n)
	}
}

// The Library page waits for a held load only as long as the shortened
// libraryWait, then shows its retry state.
func TestLibraryPageWaitIsBounded(t *testing.T) {
	gate := make(chan struct{})
	c := newCountingLibrary(t, gate)
	w := c.web(t)
	w.libraryWait = 100 * time.Millisecond
	t.Cleanup(func() { close(gate) })

	start := time.Now()
	out := get(t, w, "/sources/library")
	if d := time.Since(start); d > time.Second {
		t.Errorf("Library page took %v with the library held, want about %v", d, w.libraryWait)
	}
	if !strings.Contains(out, "Could not load the Library.") || !strings.Contains(out, "Try again") {
		t.Errorf("held library: page shows no retry state; body=%s", out)
	}
}

// Nothing is fetched until the library is asked for: building the Web and its
// routes does not preload it, serve does that itself.
func TestLibraryNotLoadedAtBoot(t *testing.T) {
	c := newCountingLibrary(t, nil)
	w := c.web(t)
	_ = w.Routes()
	time.Sleep(50 * time.Millisecond)
	if n := c.loads.Load(); n != 0 {
		t.Errorf("loads = %d, want none until the library is asked for", n)
	}
}

func TestSourcesPageLoadsLibraryFromDir(t *testing.T) {
	w, _ := sourcesTestWeb(t, config.FeedsDoc{})
	w.SetLibrary(library.NewLoader(library.Options{Dir: os.DirFS("../../library")}))
	get(t, w, "/sources")
	waitFor(t, func() bool { return w.library.Snapshot().Loaded })
	if !strings.Contains(get(t, w, "/sources"), navGroupMarker) {
		t.Error("the visit after the load does not list the Library groups")
	}
}

// waitFor polls cond until it holds or a few seconds pass.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
