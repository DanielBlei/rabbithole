// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package library

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing/fstest"
	"time"

	"github.com/rs/zerolog"

	"github.com/DanielBlei/rabbithole/internal/httpclient"
)

// DefaultBaseURL is the directory the running app fetches the library files from.
const DefaultBaseURL = "https://raw.githubusercontent.com/DanielBlei/rabbithole/main/library/"

const (
	offValue     = "off"
	maxFileBytes = 1 << 20

	defaultTTL     = time.Hour
	defaultTimeout = 5 * time.Second
	defaultBackoff = 5 * time.Minute

	// dirTTL is the default age after which files read from Options.Dir are read again.
	dirTTL = 2 * time.Second

	// emptyBackoff caps the retry gap while nothing has loaded yet.
	emptyBackoff = 30 * time.Second
	// refreshFloor is the minimum gap between attempts for an explicit Refresh.
	refreshFloor = 10 * time.Second

	// NeedsNewerVersion is the Snapshot notice when a file needs a newer app.
	NeedsNewerVersion = "Some of the Library needs a newer version of The Rabbit Hole."
)

// Options configures NewLoader.
type Options struct {
	Client  *http.Client  // nil without Dir: the library is never fetched and Snapshot reports not loaded
	Dir     fs.FS         // when set, the files are read from it instead of fetched, with a 2s TTL and backoff
	BaseURL string        // directory URL, a trailing slash is added; "off" disables; empty means DefaultBaseURL
	TTL     time.Duration // age after which a refresh starts, default 1h
	Timeout time.Duration // bound of one refresh, default 5s
	Backoff time.Duration // minimum gap after a file's attempt before its next one, default 5m (30s while nothing is loaded)
	Now     func() time.Time
	Log     zerolog.Logger
}

// Snapshot is the library as last fetched. Library is never nil and must not be modified.
type Snapshot struct {
	Library *Library
	Notice  string // set when a file needs a newer version of the app
	Loaded  bool   // at least one file was accepted
	Off     bool   // the library is turned off
}

// fileState is what the loader knows about one library file.
type fileState struct {
	data  []byte // last accepted bytes
	etag  string // validator of data
	newer bool   // the server's copy needs a newer app

	lastOK      time.Time // last time the server's answer for this file was understood
	lastAttempt time.Time // last time a fetch of this file started
}

// Loader fetches the library files from a base URL, or reads them from a directory, and caches them.
// A refresh runs on its own context, so a caller giving up never aborts it.
type Loader struct {
	client  *http.Client
	dir     fs.FS
	base    string
	off     bool
	ttl     time.Duration
	timeout time.Duration
	backoff time.Duration
	now     func() time.Time
	log     zerolog.Logger

	mu          sync.Mutex
	snap        Snapshot
	inflight    chan struct{} // non-nil while a refresh runs, closed when it ends
	done        bool          // a refresh has ended
	lastAttempt time.Time     // start of the latest refresh

	files map[string]*fileState // fetched fields owned by the running refresh
}

// NewLoader returns a Loader that fetches nothing until Get, Snapshot or Refresh is called.
func NewLoader(o Options) *Loader {
	base := strings.TrimSpace(o.BaseURL)
	l := &Loader{
		client:  o.Client,
		dir:     o.Dir,
		base:    base,
		off:     strings.EqualFold(base, offValue),
		ttl:     durationOr(o.TTL, defaultTTL),
		timeout: durationOr(o.Timeout, defaultTimeout),
		backoff: durationOr(o.Backoff, defaultBackoff),
		now:     o.Now,
		log:     o.Log,
		files:   map[string]*fileState{},
	}
	if o.Dir != nil {
		l.ttl = durationOr(o.TTL, dirTTL)
		l.backoff = durationOr(o.Backoff, dirTTL)
	}
	for _, typ := range knownTypes {
		l.files[fileName(typ)] = &fileState{}
	}
	if base == "" {
		l.base = DefaultBaseURL
	}
	if !strings.HasSuffix(l.base, "/") {
		l.base += "/"
	}
	if l.now == nil {
		l.now = time.Now
	}
	l.snap = Snapshot{Library: &Library{}, Off: l.off}
	return l
}

func fileName(typ Type) string { return string(typ) + ".yaml" }

func durationOr(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// Get returns the library. While nothing has loaded it waits for a running or due refresh, bounded
// by ctx; once something has loaded it returns the cache and refreshes in the background when stale.
func (l *Loader) Get(ctx context.Context) Snapshot {
	l.mu.Lock()
	if !l.enabled() || l.snap.Loaded || (l.inflight == nil && !l.staleLocked()) {
		return l.unlockedSnapshot()
	}
	ch := l.startLocked(false)
	l.mu.Unlock()
	select {
	case <-ch:
	case <-ctx.Done():
	}
	return l.read()
}

// Snapshot returns the cache without waiting, and starts a background refresh when it is stale.
func (l *Loader) Snapshot() Snapshot {
	l.mu.Lock()
	return l.unlockedSnapshot()
}

// Refresh refetches every file unless one runs, waits for it bounded by ctx and returns the cache.
// An attempt less than 10 seconds old, or one in flight, is reused instead of starting another.
func (l *Loader) Refresh(ctx context.Context) Snapshot {
	l.mu.Lock()
	if !l.enabled() {
		return l.unlockedSnapshot()
	}
	if l.inflight == nil && l.now().Sub(l.lastAttempt) < refreshFloor {
		s := l.snap
		l.mu.Unlock()
		return s
	}
	ch := l.startLocked(true)
	l.mu.Unlock()
	select {
	case <-ch:
	case <-ctx.Done():
	}
	return l.read()
}

func (l *Loader) read() Snapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.snap
}

// unlockedSnapshot kicks a refresh when stale, releases mu and returns the cache.
// The caller holds mu.
func (l *Loader) unlockedSnapshot() Snapshot {
	if l.enabled() && l.inflight == nil && l.staleLocked() {
		l.startLocked(false)
	}
	s := l.snap
	l.mu.Unlock()
	return s
}

func (l *Loader) enabled() bool {
	return !l.off && (l.client != nil || l.dir != nil)
}

// dueLocked reports whether a file needs a fetch: its last understood answer is older than the TTL
// and its last attempt is older than the backoff. A file never fetched is due. The backoff is short
// while nothing has loaded so a page load recovers soon after the network returns.
func (l *Loader) dueLocked(st *fileState, now time.Time) bool {
	backoff := l.backoff
	if !l.snap.Loaded {
		backoff = min(backoff, emptyBackoff)
	}
	return now.Sub(st.lastOK) > l.ttl && now.Sub(st.lastAttempt) > backoff
}

// staleLocked reports whether any file is due.
func (l *Loader) staleLocked() bool {
	now := l.now()
	for _, st := range l.files {
		if l.dueLocked(st, now) {
			return true
		}
	}
	return false
}

// startLocked returns the running refresh, starting one when none runs. It fetches every file when
// all is set, otherwise the files that are due. The caller holds mu.
func (l *Loader) startLocked(all bool) chan struct{} {
	if l.inflight != nil {
		return l.inflight
	}
	ch := make(chan struct{})
	l.inflight = ch
	now := l.now()
	l.lastAttempt = now
	var names []string
	for _, typ := range knownTypes {
		name := fileName(typ)
		st := l.files[name]
		if all || l.dueLocked(st, now) {
			st.lastAttempt = now
			names = append(names, name)
		}
	}
	go l.refresh(ch, names)
	return ch
}

// refresh fetches the named files once, rebuilds the library and publishes it.
func (l *Loader) refresh(ch chan struct{}, names []string) {
	ctx, cancel := context.WithTimeout(context.Background(), l.timeout)
	defer cancel()

	var reached []string
	changed := false
	for _, name := range names {
		r, c := l.fetchFile(ctx, name)
		if r {
			reached = append(reached, name)
		}
		changed = changed || c
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for _, name := range reached {
		l.files[name].lastOK = now
	}
	if changed || !l.done {
		l.snap.Library = l.assemble()
	}
	l.snap.Notice = ""
	l.snap.Loaded = false
	for _, st := range l.files {
		if st.newer {
			l.snap.Notice = NeedsNewerVersion
		}
		if st.data != nil {
			l.snap.Loaded = true
		}
	}
	l.done = true
	l.inflight = nil
	close(ch)
}

// fetchFile loads one file from the source. reached is true when the answer was understood
// (not modified, accepted, or needing a newer app); changed is true when new bytes were accepted.
// A failure keeps the last good bytes.
func (l *Loader) fetchFile(ctx context.Context, name string) (reached, changed bool) {
	st := l.files[name]
	log := l.log.With().Str("file", name).Logger()
	var data []byte
	var etag string
	var ok bool
	if l.dir != nil {
		data, ok = l.readDir(name, log)
		if ok && st.data != nil && bytes.Equal(data, st.data) {
			st.newer = false
			return true, false
		}
	} else {
		data, etag, ok = l.httpGet(ctx, st, name, log)
		if ok && data == nil {
			st.newer = false
			return true, false
		}
	}
	if !ok {
		return false, false
	}

	lib, diags := ParseFS(fstest.MapFS{name: {Data: data}})
	if len(lib.Files) == 0 {
		st.newer = slices.ContainsFunc(diags, func(d Diagnostic) bool {
			return d.Code == CodeNewerSchema
		})
		log.Warn().Str("findings", diags.Err().Error()).Msg("library file rejected")
		return st.newer, false
	}
	if len(diags) > 0 {
		log.Debug().Str("findings", diags.Err().Error()).Msg("library file findings")
	}
	st.data, st.etag, st.newer = data, etag, false
	return true, true
}

// readDir reads one file of the directory source, bounded by maxFileBytes.
func (l *Loader) readDir(name string, log zerolog.Logger) ([]byte, bool) {
	f, err := l.dir.Open(name)
	if err != nil {
		log.Warn().Err(err).Msg("library read")
		return nil, false
	}
	defer f.Close() //nolint:errcheck // nothing to do on a failed close
	data, err := io.ReadAll(httpclient.LimitBody(f, maxFileBytes))
	if err != nil {
		log.Warn().Err(err).Msg("library read")
		return nil, false
	}
	return data, true
}

// httpGet requests one file with the last ETag. ok with nil data means not modified.
func (l *Loader) httpGet(
	ctx context.Context,
	st *fileState,
	name string,
	log zerolog.Logger,
) (data []byte, etag string, ok bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.base+name, nil)
	if err != nil {
		log.Warn().Err(err).Msg("library request")
		return nil, "", false
	}
	if st.data != nil && st.etag != "" {
		req.Header.Set("If-None-Match", st.etag)
	}
	resp, err := l.client.Do(req)
	if err != nil {
		log.Warn().Err(err).Msg("library fetch")
		return nil, "", false
	}
	defer resp.Body.Close() //nolint:errcheck // nothing to do on a failed close
	switch {
	case resp.StatusCode == http.StatusNotModified && st.data != nil:
		return nil, "", true
	case resp.StatusCode != http.StatusOK:
		log.Warn().Int("status", resp.StatusCode).Msg("library fetch")
		return nil, "", false
	}
	data, err = io.ReadAll(httpclient.LimitBody(resp.Body, maxFileBytes))
	if err != nil {
		log.Warn().Err(err).Msg("library read")
		return nil, "", false
	}
	return data, resp.Header.Get("ETag"), true
}

// assemble parses every accepted file together so the cross-file checks run. The caller holds mu.
func (l *Loader) assemble() *Library {
	fsys := fstest.MapFS{}
	for name, st := range l.files {
		if st.data != nil {
			fsys[name] = &fstest.MapFile{Data: st.data}
		}
	}
	if len(fsys) == 0 {
		return &Library{}
	}
	lib, diags := ParseFS(fsys)
	if len(diags) > 0 {
		l.log.Debug().Str("findings", diags.Err().Error()).Msg("library findings")
	}
	return lib
}
