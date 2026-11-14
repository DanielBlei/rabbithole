// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package articlefetch

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/text/encoding/japanese"

	"github.com/DanielBlei/rabbithole/internal/articleextract"
	"github.com/DanielBlei/rabbithole/internal/httpclient"
)

// useTestFetchPolicy installs the package's test-only client factory. It still
// constructs httpclient.NewSafe with userinfo rejection, but allows loopback
// and the ephemeral httptest port. Production has no equivalent switch.
func useTestFetchPolicy(t *testing.T) {
	t.Helper()
	prev := newFetchClient
	newFetchClient = func(limits Limits) *http.Client {
		return httpclient.NewSafe(httpclient.Options{
			Timeout:        limits.Timeout,
			AnyPort:        true,
			MaxRedirects:   limits.MaxRedirects,
			UserAgent:      limits.UserAgent,
			AllowPrivate:   true,
			RejectUserinfo: true,
		})
	}
	t.Cleanup(func() { newFetchClient = prev })
}

func fetchTest(t *testing.T, rawURL string, limits Limits) (Result, error) {
	t.Helper()
	useTestFetchPolicy(t)
	return fetch(context.Background(), rawURL, limits)
}

func TestFetchRejectsSchemes(t *testing.T) {
	for _, raw := range []string{"file:///etc/passwd", "ftp://example.com/a", "data:text/html,hi"} {
		t.Run(raw, func(t *testing.T) {
			_, err := Fetch(context.Background(), raw, Limits{})
			if !errors.Is(err, ErrBlocked) {
				t.Fatalf("Fetch(%q) err = %v, want ErrBlocked", raw, err)
			}
		})
	}
}

func TestFetchRejectsNoLink(t *testing.T) {
	for _, raw := range []string{"", "   ", "not a url", "/relative"} {
		if _, err := Fetch(context.Background(), raw, Limits{}); !errors.Is(err, ErrNoLink) {
			t.Errorf("Fetch(%q) err = %v, want ErrNoLink", raw, err)
		}
	}
}

func TestFetchBlocksPrivateAndMetadataAddresses(t *testing.T) {
	vectors := []string{
		"http://127.0.0.1/",
		"http://localhost/",
		"http://[::1]/",
		"http://10.0.0.1/",
		"http://172.16.0.1/",
		"http://192.168.1.1/",
		"http://169.254.169.254/latest/meta-data/",
		"http://[fe80::1]/",
		"http://[fd12::1]/",
		"http://[::ffff:127.0.0.1]/",
		"http://[64:ff9b::127.0.0.1]/",
	}
	for _, raw := range vectors {
		t.Run(raw, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, err := Fetch(ctx, raw, Limits{Timeout: time.Second})
			if !errors.Is(err, ErrBlocked) {
				t.Fatalf("Fetch(%q) err = %v, want ErrBlocked", raw, err)
			}
		})
	}
}

func TestFetchBlocksNonStandardPort(t *testing.T) {
	_, err := Fetch(context.Background(), "http://93.184.216.34:8080/", Limits{Timeout: time.Second})
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v, want ErrBlocked", err)
	}
}

func TestFetchSuccessAndHygiene(t *testing.T) {
	var gotUA, gotAuth, gotCookie, gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotAuth = r.Header.Get("Authorization")
		gotCookie = r.Header.Get("Cookie")
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "<p>hello</p>")
	}))
	defer srv.Close()

	res, err := fetchTest(t, srv.URL+"/article", Limits{MaxArticleBytes: 1024})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "<p>hello</p>" {
		t.Errorf("body = %q", body)
	}
	if res.FinalURL == "" || res.ContentType != "text/html; charset=utf-8" {
		t.Errorf("result = %+v", res)
	}
	if gotUA != DefaultUserAgent() {
		t.Errorf("User-Agent = %q", gotUA)
	}
	if gotAuth != "" || gotCookie != "" {
		t.Errorf("credentials leaked: Authorization=%q Cookie=%q", gotAuth, gotCookie)
	}
	if !strings.Contains(gotAccept, "text/html") {
		t.Errorf("Accept = %q", gotAccept)
	}
}

func TestFetchContentTypeGate(t *testing.T) {
	for _, ct := range []string{"application/pdf", "image/png", "application/rss+xml", ""} {
		t.Run(ct, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if ct != "" {
					w.Header().Set("Content-Type", ct)
				}
				_, _ = io.WriteString(w, "body")
			}))
			defer srv.Close()
			_, err := fetchTest(t, srv.URL, Limits{})
			if !errors.Is(err, ErrUnsupportedContentType) {
				t.Fatalf("content type %q err = %v, want ErrUnsupportedContentType", ct, err)
			}
		})
	}
}

func TestFetchAcceptsXHTMLWithCharset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=iso-8859-1")
		_, _ = io.WriteString(w, "body")
	}))
	defer srv.Close()
	res, err := fetchTest(t, srv.URL, Limits{})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	_ = res.Body.Close()
}

func TestFetchBodyLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(strings.Repeat("a", 2048)))
	}))
	defer srv.Close()
	res, err := fetchTest(t, srv.URL, Limits{MaxArticleBytes: 1024})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	_, err = io.ReadAll(res.Body)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("read err = %v, want ErrTooLarge", err)
	}
}

func TestFetchGzipBombCountsDecompressedBytes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept-Encoding") == "" {
			t.Error("transport did not request gzip; LimitBody may be counting compressed bytes")
		}
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		_, _ = gz.Write([]byte(strings.Repeat("a", 1<<20)))
		_ = gz.Close()
	}))
	defer srv.Close()
	res, err := fetchTest(t, srv.URL, Limits{MaxArticleBytes: 1024})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	_, err = io.ReadAll(res.Body)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("read err = %v, want ErrTooLarge", err)
	}
}

func TestFetchTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = io.WriteString(w, "late")
	}))
	defer srv.Close()
	_, err := fetchTest(
		t,
		srv.URL,
		Limits{Timeout: 50 * time.Millisecond},
	)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
}

func TestFetchRedirectCap(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/next", http.StatusFound)
	}))
	defer srv.Close()
	_, err := fetchTest(t, srv.URL, Limits{MaxRedirects: 5})
	if !errors.Is(err, ErrTooManyRedirects) {
		t.Fatalf("err = %v, want ErrTooManyRedirects", err)
	}
}

func TestFetchNonSuccessStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer srv.Close()
	_, err := fetchTest(t, srv.URL, Limits{})
	if !errors.Is(err, ErrHTTPStatus) {
		t.Fatalf("err = %v, want ErrHTTPStatus", err)
	}
}

// Finding #4: the validated declared charset survives the fetch so extraction
// can decode the page as the publisher specified.
func TestFetchPreservesDeclaredCharset(t *testing.T) {
	tests := []struct {
		header string
		want   string
	}{
		{"text/html; charset=iso-8859-1", "text/html; charset=iso-8859-1"},
		{"application/xhtml+xml; charset=utf-8", "application/xhtml+xml; charset=utf-8"},
	}
	for _, tt := range tests {
		t.Run(tt.header, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tt.header)
				_, _ = io.WriteString(w, "<p>body</p>")
			}))
			defer srv.Close()
			res, err := fetchTest(t, srv.URL, Limits{})
			if err != nil {
				t.Fatalf("fetch: %v", err)
			}
			_ = res.Body.Close()
			if res.ContentType != tt.want {
				t.Errorf("ContentType = %q, want %q", res.ContentType, tt.want)
			}
		})
	}
}

// Finding #4: a declared Shift_JIS charset reaches extraction, which is the
// difference between readable Japanese and windows-1252 mojibake.
// Additional D: a link that carries userinfo must never reach the network, a
// log line, a model prompt or a stored brief, and the error must not echo it.
func TestFetchRejectsCredentialedURL(t *testing.T) {
	const secret = "s3cr3t-feed-password"
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<p>unexpected</p>")
	}))
	defer srv.Close()

	target := strings.Replace(srv.URL, "http://", "http://feeduser:"+secret+"@", 1) + "/article"
	_, err := Fetch(context.Background(), target, Limits{Timeout: time.Second})
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v, want ErrBlocked", err)
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("server received %d requests; credentialed URL reached the network", got)
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "feeduser") {
		t.Fatalf("error leaked credentials: %v", err)
	}
}

// A credentialed redirect must be rejected before the target is contacted.
func TestFetchRejectsCredentialedRedirect(t *testing.T) {
	const secret = "s3cr3t-redirect-password"
	var finalHits atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/final" {
			finalHits.Add(1)
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, "<p>unexpected</p>")
			return
		}
		target := strings.Replace(srv.URL, "http://", "http://redirectuser:"+secret+"@", 1) + "/final"
		http.Redirect(w, r, target, http.StatusFound)
	}))
	defer srv.Close()

	_, err := fetchTest(t, srv.URL+"/start", Limits{})
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v, want ErrBlocked", err)
	}
	if got := finalHits.Load(); got != 0 {
		t.Fatalf("redirected endpoint received %d requests; credential hop was followed", got)
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "redirectuser") {
		t.Fatalf("error leaked redirect credentials: %v", err)
	}
}

// A multi-hop chain that ends in a credentialed URL is rejected before the
// final hop, even though earlier hops are legitimate.
func TestFetchRejectsCredentialedFinalHop(t *testing.T) {
	const secret = "s3cr3t-final-hop"
	var finalHits, middleHits atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/one":
			http.Redirect(w, r, srv.URL+"/two", http.StatusFound)
		case "/two":
			middleHits.Add(1)
			target := strings.Replace(srv.URL, "http://", "http://finalhop:"+secret+"@", 1) + "/final"
			http.Redirect(w, r, target, http.StatusFound)
		default:
			finalHits.Add(1)
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, "<p>unexpected</p>")
		}
	}))
	defer srv.Close()

	_, err := fetchTest(t, srv.URL+"/one", Limits{})
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v, want ErrBlocked", err)
	}
	if got := middleHits.Load(); got != 1 {
		t.Fatalf("middle hop hits = %d, want 1", got)
	}
	if got := finalHits.Load(); got != 0 {
		t.Fatalf("final endpoint received %d requests; credential hop was followed", got)
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "finalhop") {
		t.Fatalf("error leaked final-hop credentials: %v", err)
	}
}

// The exported path has no client/transport injection. A loopback test server
// is still blocked by the production policy even though the package has a
// test-only factory that could allow it.
func TestProductionFetchCannotUseUnsafeClient(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<p>unexpected</p>")
	}))
	defer srv.Close()

	_, err := Fetch(context.Background(), srv.URL, Limits{Timeout: time.Second})
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v, want ErrBlocked", err)
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("server received %d requests; production policy was bypassed", got)
	}
}

// A credential-free redirect works under the explicit test policy, and the
// final URL is reported without any userinfo component.
func TestFetchFinalURLAfterSafeRedirect(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/go" {
			http.Redirect(w, r, srv.URL+"/final", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<p>final</p>")
	}))
	defer srv.Close()

	res, err := fetchTest(t, srv.URL+"/go", Limits{})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	_ = res.Body.Close()
	u, err := url.Parse(res.FinalURL)
	if err != nil || u.User != nil || u.Path != "/final" {
		t.Fatalf("FinalURL = %q (%v), want the final path without userinfo", res.FinalURL, err)
	}
}

func TestWithoutUserinfo(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"https://example.com/a", "https://example.com/a"},
		{"https://user:pass@example.com/a?q=1#f", "https://example.com/a?q=1#f"},
		{"https://user@example.com/", "https://example.com/"},
		{"https://:pass@example.com/", "https://example.com/"},
		{"http://user:pass@example.com:8080/a", "http://example.com:8080/a"},
		{"://not a url", ""},
	}
	for _, tt := range tests {
		if got := WithoutUserinfo(tt.in); got != tt.want {
			t.Errorf("WithoutUserinfo(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestFetchShiftJISReachesExtraction(t *testing.T) {
	const jp = "こんにちは世界。これは日本語の記事です。"
	page := `<html><body><article><p>` + strings.Repeat(jp, 25) +
		`</p></article></body></html>`
	encoded, err := japanese.ShiftJIS.NewEncoder().String(page)
	if err != nil {
		t.Fatalf("encode shift_jis: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=shift_jis")
		_, _ = io.WriteString(w, encoded)
	}))
	defer srv.Close()

	res, err := fetchTest(t, srv.URL, Limits{})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	body, err := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	art, err := articleextract.Extract(bytes.NewReader(body), res.ContentType, 0)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if !strings.Contains(art.Text, "こんにちは世界") {
		t.Errorf("declared shift_jis charset was lost: %q", art.Text)
	}
}
