// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
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
	w, _ := sourcesTestWeb(t, config.FeedsDoc{})
	w.SetLibrary(libraryLoader(c.srv))
	return w
}

func TestSourcesPageLoadsLibraryOnFirstVisit(t *testing.T) {
	for _, path := range []string{"/sources", "/sources/manage"} {
		t.Run(path, func(t *testing.T) {
			c := newCountingLibrary(t, nil)
			w := c.web(t)
			if n := c.loads.Load(); n != 0 {
				t.Fatalf("loads before a visit = %d, want 0", n)
			}
			if !strings.Contains(get(t, w, path), navGroupMarker) {
				t.Error("first visit does not list the Library groups")
			}
			get(t, w, path)
			if n := c.loads.Load(); n != 1 {
				t.Errorf("loads after two visits = %d, want 1", n)
			}
		})
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
	var missing atomic.Int32
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
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), navGroupMarker) {
				missing.Add(1)
			}
		}()
	}
	wg.Wait()
	if n := missing.Load(); n != 0 {
		t.Errorf("%d visits did not list the Library groups", n)
	}
	if n := c.loads.Load(); n != 1 {
		t.Errorf("loads = %d, want 1", n)
	}
}

// A visitor who leaves does not cancel the load: the next visit finds it done.
func TestSourcesPageCancelDoesNotCancelLoad(t *testing.T) {
	gate := make(chan struct{})
	c := newCountingLibrary(t, gate)
	w := c.web(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		rec := httptest.NewRecorder()
		w.Routes().ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "/sources/manage", nil))
	}()
	waitFor(t, func() bool { return c.loads.Load() == 1 })
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the page waits on the load after the request ended")
	}

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
	if n := loads.Load(); n != 3 {
		t.Errorf("requests = %d, want one load of every file", n)
	}
}

func TestLibraryNotLoadedAtBoot(t *testing.T) {
	c := newCountingLibrary(t, nil)
	w := c.web(t)
	_ = w.Routes()
	if n := c.loads.Load(); n != 0 {
		t.Errorf("loads = %d, want none until a Sources page is visited", n)
	}
}

func TestSourcesPageLoadsLibraryFromDir(t *testing.T) {
	w, _ := sourcesTestWeb(t, config.FeedsDoc{})
	w.SetLibrary(library.NewLoader(library.Options{Dir: os.DirFS("../../library")}))
	if !strings.Contains(get(t, w, "/sources"), navGroupMarker) {
		t.Error("first visit does not list the Library groups")
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
