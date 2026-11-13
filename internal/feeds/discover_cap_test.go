// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package feeds

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/httpclient"
)

// The size-cap tests run inside the package so they can pass a small cap to
// discover, preview and fetchAll: the over-cap path is exercised with
// kilobytes and no wall-clock assumptions, instead of transferring a 16 MiB
// body over a socket within the request deadline — which fails on a busy CI
// runner and used to be misreported as ErrNotPage. The production cap keeps
// its own boundary coverage through the public API, with bodies served by an
// in-memory transport.

const (
	capTypeRSS  = "application/rss+xml"
	capTypeHTML = "text/html; charset=utf-8"
)

// capPage is one path's response in a capSite.
type capPage struct {
	ctype string
	body  string
}

// capSite serves paths with fixed responses; it is the internal twin of
// newSite in discover_test.go, without the hit counting.
func capSite(t *testing.T, paths map[string]capPage) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := paths[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if p.ctype != "" {
			w.Header().Set("Content-Type", p.ctype)
		}
		_, _ = w.Write([]byte(p.body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// bigRSS returns a valid feed of about size bytes.
func bigRSS(size int) string {
	pad := strings.Repeat("x", 1024)
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><rss version="2.0"><channel><title>Big</title>`)
	for i := 0; b.Len() < size; i++ {
		fmt.Fprintf(&b, `<item><title>Item %d</title><description>%s</description></item>`, i, pad)
	}
	b.WriteString(`</channel></rss>`)
	return b.String()
}

func TestDiscoverFeedOverCapFails(t *testing.T) {
	const maxFeed = 64 << 10
	s := capSite(t, map[string]capPage{"/feed": {ctype: capTypeRSS, body: bigRSS(maxFeed + 32<<10)}})
	_, err := discover(context.Background(), s.Client(), s.URL+"/feed", maxFeed)
	if !errors.Is(err, httpclient.ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}

// A feed just under the cap still parses, so the cap does not clip what it
// should allow through.
func TestDiscoverFeedUnderCapParses(t *testing.T) {
	const maxFeed = 1 << 20
	s := capSite(t, map[string]capPage{"/feed": {ctype: capTypeRSS, body: bigRSS(maxFeed - 256<<10)}})
	got, err := discover(context.Background(), s.Client(), s.URL+"/feed", maxFeed)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(got) != 1 || got[0].Kind != KindRSS || got[0].ItemCount == 0 {
		t.Fatalf("got %+v, want one parsed feed with items", got)
	}
}

// A page that starts like XML and is over the feed cap is still searched as a page.
func TestDiscoverOversizeHTMLThatSniffsAsAFeed(t *testing.T) {
	const maxFeed = 64 << 10
	body := `<rss version="2.0"><head><link rel="alternate" type="application/rss+xml" href="/f.xml"/></head><body>` +
		strings.Repeat("<p>filler</p>", (maxFeed+32<<10)/13)
	declared := `<rss version="2.0"><channel><title>F</title><item><title>I</title></item></rss>`
	s := capSite(t, map[string]capPage{
		"/":      {ctype: capTypeHTML, body: body},
		"/f.xml": {ctype: capTypeRSS, body: declared},
	})
	got, err := discover(context.Background(), s.Client(), s.URL, maxFeed)
	if err != nil || len(got) != 1 || got[0].URL != s.URL+"/f.xml" {
		t.Fatalf("got %+v, %v, want the declared feed", got, err)
	}
}

func TestPreviewFeedOverCapFails(t *testing.T) {
	const maxFeed = 64 << 10
	s := capSite(t, map[string]capPage{"/big": {ctype: capTypeRSS, body: bigRSS(maxFeed + 32<<10)}})
	_, err := preview(context.Background(), s.Client(), s.URL+"/big", maxFeed)
	if !errors.Is(err, httpclient.ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}

// A feed address whose body stalls past the deadline reports the timeout, not
// ErrNotPage. The deadline used to be swallowed by the not-a-page fallthrough,
// which is how a slow CI runner turned an oversized feed into "not a web page".
func TestDiscoverFeedReadTimeoutSurfaces(t *testing.T) {
	// The body sits above MaxPageBytes but under the cap, so the page read
	// completes and only the held-open tail can run out the clock.
	body := bigRSS(2<<20 + 512<<10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", capTypeRSS)
		_, _ = w.Write([]byte(body))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done() // hold the connection until the client gives up
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err := discover(ctx, srv.Client(), srv.URL+"/feed", 4<<20)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if errors.Is(err, ErrNotPage) {
		t.Fatalf("err = %v, the timeout was misclassified as ErrNotPage", err)
	}
}

// memTransport answers every request with one canned response. Production-sized
// bodies move through memory instead of a socket, so tests at the real
// MaxFeedBytes boundary carry no network timing.
type memTransport struct {
	ctype string
	body  io.Reader
}

func (m memTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{m.ctype}},
		Body:       io.NopCloser(m.body),
		Request:    req,
	}, nil
}

func memClient(ctype string, body io.Reader) *http.Client {
	return &http.Client{Transport: memTransport{ctype: ctype, body: body}}
}

// errReader yields one chunk, then fails with err.
type errReader struct {
	chunk string
	err   error
	done  bool
}

func (r *errReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, r.err
	}
	r.done = true
	return copy(p, r.chunk), nil
}

// A context error wrapped by lower layers still surfaces through the parser
// chain, so loadPage can recognise a cut-short read with errors.Is.
func TestParseStreamPreservesWrappedContextErrors(t *testing.T) {
	r := &errReader{
		chunk: `<?xml version="1.0"?><rss version="2.0"><channel><title>T</title>`,
		err:   fmt.Errorf("connection stalled: %w", context.DeadlineExceeded),
	}
	_, err := parseStream(newParser(http.DefaultClient, MaxFeedBytes), r, MaxFeedBytes)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a wrapped context.DeadlineExceeded", err)
	}
}

// feedThenCtxReader serves a feed larger than MaxPageBytes, then reports the
// context's error once the feed is exhausted — the way a transport aborts a
// body read when its request is canceled.
type feedThenCtxReader struct {
	feed *strings.Reader
	ctx  context.Context
}

func (r *feedThenCtxReader) Read(p []byte) (int, error) {
	if n, err := r.feed.Read(p); n > 0 || err != io.EOF {
		return n, err
	}
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

// A canceled feed read reports the cancellation, not ErrNotPage.
func TestDiscoverFeedReadCanceledSurfaces(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	body := &feedThenCtxReader{feed: strings.NewReader(bigRSS(2<<20 + 512<<10)), ctx: ctx}
	_, err := Discover(ctx, memClient(capTypeRSS, body), "https://example.test/feed")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if errors.Is(err, ErrNotPage) {
		t.Fatalf("err = %v, the cancellation was misclassified as ErrNotPage", err)
	}
}

// A deadline error wrapped by lower layers surfaces through the whole
// discovery chain — loadPage must recognise it with errors.Is, not ==.
func TestDiscoverFeedWrappedDeadlineSurfaces(t *testing.T) {
	feed := strings.NewReader(bigRSS(2<<20 + 512<<10))
	stalled := &errReader{err: fmt.Errorf("connection stalled: %w", context.DeadlineExceeded)}
	body := io.MultiReader(feed, stalled)
	_, err := Discover(context.Background(), memClient(capTypeRSS, body), "https://example.test/feed")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a wrapped context.DeadlineExceeded", err)
	}
	if errors.Is(err, ErrNotPage) {
		t.Fatalf("err = %v, the wrapped deadline was misclassified as ErrNotPage", err)
	}
}

// delayedBody holds its first read back until after a duration, so a context
// deadline can expire while the body is still pending.
type delayedBody struct {
	delayed bool
	after   time.Duration
	r       io.Reader
}

func (d *delayedBody) Read(p []byte) (int, error) {
	if !d.delayed {
		d.delayed = true
		time.Sleep(d.after)
	}
	return d.r.Read(p)
}

// A parse failure unrelated to the clock keeps its ErrNotPage verdict, whether
// the request deadline has separately expired or not: an expired clock must
// not rewrite a verdict about the content.
func TestDiscoverParseFailureKeepsErrNotPage(t *testing.T) {
	const garbage = "this is not a feed"
	assertNotPage := func(t *testing.T, err error) {
		t.Helper()
		if !errors.Is(err, ErrNotPage) {
			t.Fatalf("err = %v, want ErrNotPage", err)
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, an unrelated parse failure was rewritten as a context error", err)
		}
	}
	t.Run("live context", func(t *testing.T) {
		client := memClient(capTypeRSS, strings.NewReader(garbage))
		_, err := Discover(context.Background(), client, "https://example.test/feed")
		assertNotPage(t, err)
	})
	t.Run("deadline expired during the read", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		body := &delayedBody{after: 120 * time.Millisecond, r: strings.NewReader(garbage)}
		_, err := Discover(ctx, memClient(capTypeRSS, body), "https://example.test/feed")
		assertNotPage(t, err)
	})
}

// bigPadRSS returns a valid feed of about size bytes with the bulk inside one
// item, so a parser that reads the whole document trips the cap mid-parse.
func bigPadRSS(size int) string {
	pad := strings.Repeat("x", size)
	return `<?xml version="1.0"?><rss version="2.0"><channel><title>Big</title>` +
		`<item><title>Item</title><description>` + pad + `</description></item></channel></rss>`
}

// trailingJunkFeed returns a valid feed followed by about size bytes of
// trailing padding, all counted against the cap: the parser stops at the
// root's closing tag, and the drain enforces the cap over the whole body.
// The bulk bypasses XML scanning, so the fixture stays cheap under the race
// detector and far inside the request deadline.
func trailingJunkFeed(size int) string {
	doc := `<?xml version="1.0"?><rss version="2.0"><channel><title>T</title>` +
		`<item><title>Item</title></item></channel></rss>`
	return doc + strings.Repeat("x", size)
}

// The production cap keeps its own coverage through the public API: a body
// just under MaxFeedBytes parses and one just over it is rejected. The
// in-memory transport carries the multi-megabyte bodies without a socket.
func TestDiscoverFeedAtProductionCap(t *testing.T) {
	const feedURL = "https://example.test/feed"
	t.Run("just under parses", func(t *testing.T) {
		body := trailingJunkFeed(MaxFeedBytes - 2<<10)
		got, err := Discover(context.Background(), memClient(capTypeRSS, strings.NewReader(body)), feedURL)
		if err != nil {
			t.Fatalf("Discover: %v", err)
		}
		if len(got) != 1 || got[0].Kind != KindRSS || got[0].ItemCount != 1 {
			t.Fatalf("got %+v, want one parsed feed with an item", got)
		}
	})
	t.Run("just over is rejected", func(t *testing.T) {
		body := trailingJunkFeed(MaxFeedBytes + 2<<10)
		_, err := Discover(context.Background(), memClient(capTypeRSS, strings.NewReader(body)), feedURL)
		if !errors.Is(err, httpclient.ErrTooLarge) {
			t.Fatalf("err = %v, want ErrTooLarge", err)
		}
	})
}

// The fetch path trips the cap inside the document, since gofeed stops at the
// root's closing tag: the bulk must be part of the feed for the parser to
// read past MaxFeedBytes. It runs against the 30s fetch timeout, not the
// discovery request deadline.
func TestFetchAllRejectsOverProductionCap(t *testing.T) {
	body := bigPadRSS(MaxFeedBytes + 2<<10)
	src := []Source{{Name: "Big", URL: "https://example.test/feed"}}
	res := FetchAll(context.Background(), memClient(capTypeRSS, strings.NewReader(body)), src)[0]
	if !errors.Is(res.Err, httpclient.ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", res.Err)
	}
}
