// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package articlefetch

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/httpclient"
)

func testClient(t *testing.T, maxRedirects int) *http.Client {
	t.Helper()
	return testClientTimeout(t, maxRedirects, 2*time.Second)
}

func testClientTimeout(t *testing.T, maxRedirects int, timeout time.Duration) *http.Client {
	t.Helper()
	return httpclient.NewSafe(httpclient.Options{
		Timeout:      timeout,
		AnyPort:      true,
		MaxRedirects: maxRedirects,
		UserAgent:    "rabbithole/test",
		AllowPrivate: true,
	})
}

func fetchTest(t *testing.T, rawURL string, limits Limits, client *http.Client) (Result, error) {
	t.Helper()
	return fetch(context.Background(), rawURL, limits, client)
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

	res, err := fetchTest(t, srv.URL+"/article", Limits{MaxArticleBytes: 1024}, testClient(t, 5))
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
	if res.FinalURL == "" || res.ContentType != "text/html" {
		t.Errorf("result = %+v", res)
	}
	if gotUA != "rabbithole/test" {
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
			_, err := fetchTest(t, srv.URL, Limits{}, testClient(t, 5))
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
	res, err := fetchTest(t, srv.URL, Limits{}, testClient(t, 5))
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
	res, err := fetchTest(t, srv.URL, Limits{MaxArticleBytes: 1024}, testClient(t, 5))
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
	res, err := fetchTest(t, srv.URL, Limits{MaxArticleBytes: 1024}, testClient(t, 5))
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
		testClientTimeout(t, 5, 50*time.Millisecond),
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
	_, err := fetchTest(t, srv.URL, Limits{MaxRedirects: 5}, testClient(t, 5))
	if !errors.Is(err, ErrTooManyRedirects) {
		t.Fatalf("err = %v, want ErrTooManyRedirects", err)
	}
}

func TestFetchNonSuccessStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer srv.Close()
	_, err := fetchTest(t, srv.URL, Limits{}, testClient(t, 5))
	if !errors.Is(err, ErrHTTPStatus) {
		t.Fatalf("err = %v, want ErrHTTPStatus", err)
	}
}
