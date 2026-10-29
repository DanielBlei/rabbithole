// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package library

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

// fakeFile is one file the test server serves.
type fakeFile struct {
	body   []byte
	etag   string
	status int           // 0 serves the body
	block  chan struct{} // when set, the handler waits for it to close
	delay  time.Duration
}

// fakeServer serves library files and records what it was asked.
type fakeServer struct {
	*httptest.Server
	mu    sync.Mutex
	files map[string]*fakeFile
	hits  map[string]int
	inm   map[string][]string
}

func newFakeServer(t *testing.T, files map[string]*fakeFile) *fakeServer {
	t.Helper()
	fs := &fakeServer{files: files, hits: map[string]int{}, inm: map[string][]string{}}
	fs.Server = httptest.NewServer(http.HandlerFunc(fs.serve))
	t.Cleanup(fs.Close)
	return fs
}

func (s *fakeServer) serve(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/library/")
	s.mu.Lock()
	f := s.files[name]
	s.hits[name]++
	s.inm[name] = append(s.inm[name], r.Header.Get("If-None-Match"))
	var cp fakeFile
	if f != nil {
		cp = *f
	}
	s.mu.Unlock()
	if f == nil {
		http.NotFound(w, r)
		return
	}
	if cp.block != nil {
		select {
		case <-cp.block:
		case <-r.Context().Done():
			return
		}
	}
	time.Sleep(cp.delay)
	if cp.status != 0 {
		w.WriteHeader(cp.status)
		return
	}
	if cp.etag != "" {
		if r.Header.Get("If-None-Match") == cp.etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", cp.etag)
	}
	_, _ = w.Write(cp.body)
}

// set changes one served file.
func (s *fakeServer) set(name string, fn func(*fakeFile)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s.files[name])
}

func (s *fakeServer) hitCount(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[name]
}

func (s *fakeServer) ifNoneMatch(name string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.inm[name]...)
}

// fakeClock is a settable time source.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// repoFile reads a checked-in library file.
func repoFile(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(libraryDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// repoFiles serves the checked-in rss and academic files, each with an ETag.
func repoFiles(t *testing.T) map[string]*fakeFile {
	t.Helper()
	return map[string]*fakeFile{
		"rss.yaml":      {body: repoFile(t, "rss.yaml"), etag: `"rss-1"`},
		"academic.yaml": {body: repoFile(t, "academic.yaml"), etag: `"academic-1"`},
	}
}

// settle waits for the refresh in flight, if any.
func settle(l *Loader) {
	l.mu.Lock()
	ch := l.inflight
	l.mu.Unlock()
	if ch != nil {
		<-ch
	}
}

// loaderFor builds a Loader against s with a fake clock and a short timeout.
func loaderFor(s *fakeServer, clk *fakeClock) *Loader {
	return NewLoader(Options{
		Client:  s.Client(),
		BaseURL: s.URL + "/library",
		Timeout: 2 * time.Second,
		Now:     clk.now,
	})
}

func newClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
}

func fileNames(lib *Library) []string {
	var out []string
	for _, f := range lib.Files {
		out = append(out, f.Name)
	}
	return out
}

func requireFiles(t *testing.T, snap Snapshot, want ...string) {
	t.Helper()
	if snap.Library == nil {
		t.Fatal("Library is nil")
	}
	got := fileNames(snap.Library)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("files = %q, want %q", got, want)
	}
	if snap.Loaded != (len(want) > 0) {
		t.Fatalf("Loaded = %v with files %q", snap.Loaded, want)
	}
}

func TestLoaderFetchesAndRevalidates(t *testing.T) {
	srv := newFakeServer(t, repoFiles(t))
	clk := newClock()
	l := loaderFor(srv, clk)

	if srv.hitCount("rss.yaml") != 0 {
		t.Fatal("constructor fetched")
	}
	snap := l.Get(context.Background())
	requireFiles(t, snap, "rss.yaml", "academic.yaml")
	if snap.Notice != "" || snap.Off {
		t.Fatalf("snapshot = %+v, want no notice and not off", snap)
	}
	if got := snap.Library.Files[0].Groups[0].Title; got != "Blogs" {
		t.Errorf("first group title = %q, want Blogs", got)
	}
	if got := srv.ifNoneMatch("rss.yaml"); len(got) != 1 || got[0] != "" {
		t.Fatalf("first request If-None-Match = %q, want none", got)
	}

	// Within the TTL nothing is requested.
	clk.advance(30 * time.Minute)
	l.Get(context.Background())
	settle(l)
	if srv.hitCount("rss.yaml") != 1 {
		t.Fatalf("rss hits = %d within the TTL, want 1", srv.hitCount("rss.yaml"))
	}

	// Past the TTL a Get returns the cache and revalidates in the background.
	clk.advance(31 * time.Minute)
	if got := l.Get(context.Background()); !got.Loaded {
		t.Fatal("stale Get must return the cache")
	}
	settle(l)
	for name, etag := range map[string]string{"rss.yaml": `"rss-1"`, "academic.yaml": `"academic-1"`} {
		got := srv.ifNoneMatch(name)
		if len(got) != 2 || got[1] != etag {
			t.Errorf("%s If-None-Match = %q, want second request to send %s", name, got, etag)
		}
	}
	requireFiles(t, l.Snapshot(), "rss.yaml", "academic.yaml")
}

func TestLoaderNewEtagReplacesBytes(t *testing.T) {
	srv := newFakeServer(t, repoFiles(t))
	clk := newClock()
	l := loaderFor(srv, clk)
	l.Get(context.Background())

	srv.set("rss.yaml", func(f *fakeFile) {
		f.body = []byte(goodRSS)
		f.etag = `"rss-2"`
	})
	clk.advance(2 * time.Hour)
	l.Snapshot()
	settle(l)
	snap := l.Snapshot()
	if n := len(snap.Library.Files[0].Groups); n != 1 {
		t.Fatalf("rss groups = %d, want the 1 group of the new body", n)
	}
}

func TestLoaderNewerSchemaKeepsTheRest(t *testing.T) {
	newer := strings.Replace(string(repoFile(t, "academic.yaml")), "schema_version: 1", "schema_version: 2", 1)

	t.Run("never loaded", func(t *testing.T) {
		files := repoFiles(t)
		files["academic.yaml"] = &fakeFile{body: []byte(newer)}
		l := loaderFor(newFakeServer(t, files), newClock())
		snap := l.Get(context.Background())
		requireFiles(t, snap, "rss.yaml")
		if snap.Notice != NeedsNewerVersion {
			t.Fatalf("Notice = %q, want %q", snap.Notice, NeedsNewerVersion)
		}
	})

	t.Run("last good bytes kept, notice clears when fixed", func(t *testing.T) {
		srv := newFakeServer(t, repoFiles(t))
		clk := newClock()
		l := loaderFor(srv, clk)
		requireFiles(t, l.Get(context.Background()), "rss.yaml", "academic.yaml")

		srv.set("academic.yaml", func(f *fakeFile) { f.body, f.etag = []byte(newer), `"academic-2"` })
		clk.advance(2 * time.Hour)
		l.Snapshot()
		settle(l)
		snap := l.Snapshot()
		requireFiles(t, snap, "rss.yaml", "academic.yaml")
		if snap.Notice != NeedsNewerVersion {
			t.Fatalf("Notice = %q, want %q", snap.Notice, NeedsNewerVersion)
		}

		srv.set("academic.yaml", func(f *fakeFile) { f.body, f.etag = repoFile(t, "academic.yaml"), `"academic-1"` })
		clk.advance(2 * time.Hour)
		l.Snapshot()
		settle(l)
		if snap = l.Snapshot(); snap.Notice != "" {
			t.Fatalf("Notice = %q after the file was fixed, want none", snap.Notice)
		}
	})
}

func TestLoaderAcceptsUnknownAdditiveKey(t *testing.T) {
	body := strings.Replace(string(repoFile(t, "rss.yaml")), "    sources:\n", "    icon: book\n    sources:\n", 1)
	files := repoFiles(t)
	files["rss.yaml"] = &fakeFile{body: []byte(body)}
	snap := loaderFor(newFakeServer(t, files), newClock()).Get(context.Background())
	requireFiles(t, snap, "rss.yaml", "academic.yaml")
}

func TestLoaderFailures(t *testing.T) {
	oversize := func(f *fakeFile) {
		f.body = append(append([]byte(nil), f.body...), "\n#"+strings.Repeat("x", 1<<20)...)
		f.etag = ""
	}
	tests := []struct {
		name    string
		breakIt func(*fakeFile)
	}{
		{"not found", func(f *fakeFile) { f.status = http.StatusNotFound }},
		{"server error", func(f *fakeFile) { f.status = http.StatusInternalServerError }},
		{"timeout", func(f *fakeFile) { f.block = make(chan struct{}); f.etag = "" }},
		{"oversize", oversize},
		{"invalid yaml", func(f *fakeFile) { f.body, f.etag = []byte("\t: [garbage: :\n"), "" }},
	}
	for _, tc := range tests {
		t.Run(tc.name+" with nothing cached", func(t *testing.T) {
			files := repoFiles(t)
			tc.breakIt(files["rss.yaml"])
			tc.breakIt(files["academic.yaml"])
			srv := newFakeServer(t, files)
			l := NewLoader(
				Options{Client: srv.Client(), BaseURL: srv.URL + "/library/", Timeout: 100 * time.Millisecond},
			)
			snap := l.Get(context.Background())
			requireFiles(t, snap)
			if snap.Notice != "" {
				t.Fatalf("snapshot = %+v, want no notice", snap)
			}
		})

		t.Run(tc.name+" keeps last good bytes", func(t *testing.T) {
			srv := newFakeServer(t, repoFiles(t))
			clk := newClock()
			l := NewLoader(Options{
				Client: srv.Client(), BaseURL: srv.URL + "/library", Timeout: 100 * time.Millisecond, Now: clk.now,
			})
			good := l.Get(context.Background())
			requireFiles(t, good, "rss.yaml", "academic.yaml")

			srv.set("rss.yaml", tc.breakIt)
			clk.advance(2 * time.Hour)
			l.Snapshot()
			settle(l)
			snap := l.Snapshot()
			requireFiles(t, snap, "rss.yaml", "academic.yaml")
			if snap.Library != good.Library {
				t.Error("Library was rebuilt although no file changed")
			}
		})
	}
}

func TestLoaderTTLAndBackoff(t *testing.T) {
	files := repoFiles(t)
	files["rss.yaml"].status = http.StatusInternalServerError
	files["academic.yaml"].status = http.StatusInternalServerError
	srv := newFakeServer(t, files)
	clk := newClock()
	l := loaderFor(srv, clk)

	requireFiles(t, l.Get(context.Background()))
	if srv.hitCount("rss.yaml") != 1 {
		t.Fatalf("hits = %d, want 1", srv.hitCount("rss.yaml"))
	}

	// Inside the short backoff (nothing loaded) neither Get nor Snapshot retries.
	clk.advance(20 * time.Second)
	l.Get(context.Background())
	l.Snapshot()
	settle(l)
	if srv.hitCount("rss.yaml") != 1 {
		t.Fatalf("hits = %d inside the backoff, want 1", srv.hitCount("rss.yaml"))
	}

	// After it Snapshot retries in the background and does not wait.
	clk.advance(11 * time.Second)
	srv.set("rss.yaml", func(f *fakeFile) { f.status = 0 })
	srv.set("academic.yaml", func(f *fakeFile) { f.status = 0 })
	if snap := l.Snapshot(); snap.Loaded && srv.hitCount("rss.yaml") == 1 {
		t.Fatal("Snapshot reported loaded before the refresh ran")
	}
	settle(l)
	requireFiles(t, l.Snapshot(), "rss.yaml", "academic.yaml")
	hits := srv.hitCount("rss.yaml")
	if hits != 2 {
		t.Fatalf("hits = %d after the backoff, want 2", hits)
	}

	// A good refresh restarts the TTL.
	clk.advance(59 * time.Minute)
	l.Snapshot()
	settle(l)
	if srv.hitCount("rss.yaml") != hits {
		t.Fatal("refreshed before the TTL")
	}
	clk.advance(2 * time.Minute)
	l.Snapshot()
	settle(l)
	if srv.hitCount("rss.yaml") != hits+1 {
		t.Fatalf("hits = %d, want %d after the TTL", srv.hitCount("rss.yaml"), hits+1)
	}
}

func TestLoaderRefreshIgnoresBackoff(t *testing.T) {
	files := repoFiles(t)
	files["rss.yaml"].status = http.StatusInternalServerError
	files["academic.yaml"].status = http.StatusInternalServerError
	srv := newFakeServer(t, files)
	clk := newClock()
	l := loaderFor(srv, clk)
	requireFiles(t, l.Get(context.Background()))

	srv.set("rss.yaml", func(f *fakeFile) { f.status = 0 })
	clk.advance(11 * time.Second)
	snap := l.Refresh(context.Background())
	requireFiles(t, snap, "rss.yaml")
}

func TestLoaderRefreshFloor(t *testing.T) {
	srv := newFakeServer(t, repoFiles(t))
	clk := newClock()
	l := loaderFor(srv, clk)
	requireFiles(t, l.Get(context.Background()), "rss.yaml", "academic.yaml")

	clk.advance(5 * time.Second)
	requireFiles(t, l.Refresh(context.Background()), "rss.yaml", "academic.yaml")
	if got := srv.hitCount("rss.yaml"); got != 1 {
		t.Fatalf("rss hits = %d within the floor, want 1", got)
	}

	clk.advance(6 * time.Second)
	l.Refresh(context.Background())
	if got := srv.hitCount("rss.yaml"); got != 2 {
		t.Fatalf("rss hits = %d past the floor, want 2", got)
	}
}

func TestLoaderRefreshJoinsInflight(t *testing.T) {
	files := repoFiles(t)
	release := make(chan struct{})
	files["rss.yaml"].block = release
	srv := newFakeServer(t, files)
	l := loaderFor(srv, newClock())

	done := make(chan Snapshot, 2)
	go func() { done <- l.Get(context.Background()) }()
	for srv.hitCount("rss.yaml") == 0 {
		time.Sleep(time.Millisecond)
	}
	go func() { done <- l.Refresh(context.Background()) }()
	time.Sleep(20 * time.Millisecond)
	close(release)
	for range 2 {
		if snap := <-done; !snap.Loaded {
			t.Fatal("caller returned before the shared refresh finished")
		}
	}
	if got := srv.hitCount("rss.yaml"); got != 1 {
		t.Fatalf("rss hits = %d, want the one shared refresh", got)
	}
}

func TestLoaderRetriesOnlyTheFailedFile(t *testing.T) {
	srv := newFakeServer(t, repoFiles(t))
	clk := newClock()
	l := loaderFor(srv, clk)
	requireFiles(t, l.Get(context.Background()), "rss.yaml", "academic.yaml")

	// Both files go stale; academic fails while rss refreshes.
	srv.set("academic.yaml", func(f *fakeFile) { f.status = http.StatusInternalServerError })
	clk.advance(2 * time.Hour)
	l.Snapshot()
	settle(l)
	rssHits, acHits := srv.hitCount("rss.yaml"), srv.hitCount("academic.yaml")
	if rssHits != 2 || acHits != 2 {
		t.Fatalf("hits rss=%d academic=%d, want 2 and 2", rssHits, acHits)
	}

	// Inside the backoff nothing is fetched.
	clk.advance(4 * time.Minute)
	l.Snapshot()
	settle(l)
	if srv.hitCount("academic.yaml") != acHits {
		t.Fatal("failed file retried inside the backoff")
	}

	// After the backoff only the failed file is retried; rss keeps its TTL.
	srv.set("academic.yaml", func(f *fakeFile) { f.status = 0 })
	clk.advance(2 * time.Minute)
	l.Snapshot()
	settle(l)
	if got := srv.hitCount("academic.yaml"); got != acHits+1 {
		t.Fatalf("academic hits = %d, want %d", got, acHits+1)
	}
	if got := srv.hitCount("rss.yaml"); got != rssHits {
		t.Fatalf("rss hits = %d, want %d: a good file waits for its TTL", got, rssHits)
	}
	requireFiles(t, l.Snapshot(), "rss.yaml", "academic.yaml")
}

func TestLoaderGetRecoversWhenNothingLoaded(t *testing.T) {
	files := repoFiles(t)
	files["rss.yaml"].status = http.StatusInternalServerError
	files["academic.yaml"].status = http.StatusInternalServerError
	srv := newFakeServer(t, files)
	clk := newClock()
	l := loaderFor(srv, clk)
	requireFiles(t, l.Get(context.Background()))

	srv.set("rss.yaml", func(f *fakeFile) { f.status = 0 })
	srv.set("academic.yaml", func(f *fakeFile) { f.status = 0 })
	clk.advance(31 * time.Second)
	requireFiles(t, l.Get(context.Background()), "rss.yaml", "academic.yaml")
}

func TestLoaderEmptyBackoffIsShort(t *testing.T) {
	files := repoFiles(t)
	files["rss.yaml"].status = http.StatusInternalServerError
	files["academic.yaml"].status = http.StatusInternalServerError
	srv := newFakeServer(t, files)
	clk := newClock()
	l := loaderFor(srv, clk)
	requireFiles(t, l.Get(context.Background()))

	srv.set("rss.yaml", func(f *fakeFile) { f.status = 0 })
	clk.advance(31 * time.Second)
	l.Get(context.Background())
	settle(l)
	requireFiles(t, l.Snapshot(), "rss.yaml")

	// With something loaded the failed file waits the full backoff.
	acHits := srv.hitCount("academic.yaml")
	srv.set("academic.yaml", func(f *fakeFile) { f.status = 0 })
	clk.advance(31 * time.Second)
	l.Snapshot()
	settle(l)
	if srv.hitCount("academic.yaml") != acHits {
		t.Fatal("academic retried before the full backoff")
	}
	clk.advance(5 * time.Minute)
	l.Snapshot()
	settle(l)
	requireFiles(t, l.Snapshot(), "rss.yaml", "academic.yaml")
}

func TestLoaderSingleFlight(t *testing.T) {
	files := repoFiles(t)
	files["rss.yaml"].delay = 50 * time.Millisecond
	srv := newFakeServer(t, files)
	l := loaderFor(srv, newClock())

	const n = 20
	snaps := make([]Snapshot, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snaps[i] = l.Get(context.Background())
		}()
	}
	wg.Wait()
	for i, s := range snaps {
		if !s.Loaded {
			t.Fatalf("caller %d got a snapshot that is not loaded", i)
		}
	}
	for _, name := range []string{"rss.yaml", "academic.yaml"} {
		if srv.hitCount(name) != 1 {
			t.Errorf("%s hits = %d, want 1", name, srv.hitCount(name))
		}
	}
}

func TestLoaderCancelledCallerDoesNotAbortRefresh(t *testing.T) {
	files := repoFiles(t)
	release := make(chan struct{})
	files["rss.yaml"].block = release
	srv := newFakeServer(t, files)
	l := loaderFor(srv, newClock())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	snap := l.Get(ctx)
	requireFiles(t, snap)
	if snap.Library == nil {
		t.Fatal("Library must not be nil")
	}

	close(release)
	requireFiles(t, l.Get(context.Background()), "rss.yaml", "academic.yaml")
	if srv.hitCount("rss.yaml") != 1 {
		t.Fatalf("rss hits = %d, want the one refresh", srv.hitCount("rss.yaml"))
	}
}

func TestLoaderOff(t *testing.T) {
	for _, base := range []string{"off", " OFF "} {
		srv := newFakeServer(t, repoFiles(t))
		l := NewLoader(Options{Client: srv.Client(), BaseURL: base})
		snap := l.Get(context.Background())
		if !snap.Off || snap.Loaded || snap.Library == nil {
			t.Fatalf("%q: snapshot = %+v, want off, not loaded, non-nil Library", base, snap)
		}
		l.Snapshot()
		l.Refresh(context.Background())
		if srv.hitCount("rss.yaml") != 0 {
			t.Fatalf("%q: fetched while off", base)
		}
	}
}

func TestLoaderBaseURL(t *testing.T) {
	srv := newFakeServer(t, repoFiles(t))
	for _, base := range []string{srv.URL + "/library", srv.URL + "/library/"} {
		l := NewLoader(Options{Client: srv.Client(), BaseURL: base})
		requireFiles(t, l.Get(context.Background()), "rss.yaml", "academic.yaml")
	}
	if got := NewLoader(Options{}).base; got != DefaultBaseURL {
		t.Fatalf("default base = %q, want %q", got, DefaultBaseURL)
	}
}

func TestLoaderNilClient(t *testing.T) {
	l := NewLoader(Options{BaseURL: "https://example.invalid/library/"})
	for _, snap := range []Snapshot{l.Get(context.Background()), l.Snapshot(), l.Refresh(context.Background())} {
		if snap.Loaded || snap.Off || snap.Library == nil {
			t.Fatalf("snapshot = %+v, want not loaded, not off, non-nil Library", snap)
		}
	}
}

// A directory source loads like a fetched one, flags a newer schema and shows an edit once its TTL passes.
func TestLoaderReadsADirectory(t *testing.T) {
	rss, academic := repoFile(t, "rss.yaml"), repoFile(t, "academic.yaml")
	newer := strings.Replace(string(academic), "schema_version: 1", "schema_version: 2", 1)
	fsys := fstest.MapFS{"rss.yaml": {Data: rss}, "academic.yaml": {Data: []byte(newer)}}
	clk := newClock()
	l := NewLoader(Options{Dir: fsys, Now: clk.now})

	snap := l.Get(t.Context())
	requireFiles(t, snap, "rss.yaml")
	if snap.Notice != NeedsNewerVersion {
		t.Fatalf("Notice = %q, want %q", snap.Notice, NeedsNewerVersion)
	}

	fsys["academic.yaml"] = &fstest.MapFile{Data: academic}
	l.Snapshot()
	settle(l)
	if got := l.Snapshot(); len(got.Library.Files) != 1 {
		t.Fatalf("files = %v, want the cache before the TTL", fileNames(got.Library))
	}

	clk.advance(3 * time.Second)
	l.Snapshot()
	settle(l)
	snap = l.Snapshot()
	requireFiles(t, snap, "rss.yaml", "academic.yaml")
	if snap.Notice != "" {
		t.Errorf("Notice = %q, want none after the edit", snap.Notice)
	}
}
