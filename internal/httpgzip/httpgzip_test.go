// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package httpgzip

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// serve runs one request through the middleware over a handler that writes body
// with contentType, and returns the recorded response.
func serve(t *testing.T, req *http.Request, contentType, body string, status int) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		if status != 0 {
			w.WriteHeader(status)
		}
		if body != "" {
			_, _ = io.WriteString(w, body)
		}
	})).ServeHTTP(rec, req)
	return rec
}

func gzipReq(path string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("Accept-Encoding", "gzip")
	return r
}

func TestMiddleware(t *testing.T) {
	big := strings.Repeat("the rabbit hole ", 200) // well over minSize

	tests := []struct {
		name        string
		req         *http.Request
		contentType string
		body        string
		status      int
		wantGzip    bool
	}{
		{
			name: "compresses a large text response",
			req:  gzipReq("/feed"), contentType: "text/html; charset=utf-8",
			body: big, wantGzip: true,
		},
		{
			name: "compresses json",
			req:  gzipReq("/api/items"), contentType: "application/json",
			body: big, wantGzip: true,
		},
		{
			name: "compresses svg",
			req:  gzipReq("/static/favicon.svg"), contentType: "image/svg+xml",
			body: big, wantGzip: true,
		},
		{
			name: "leaves already-compressed formats alone",
			req:  gzipReq("/static/fonts/x.woff2"), contentType: "font/woff2",
			body: big, wantGzip: false,
		},
		{
			name: "leaves a short response alone",
			req:  gzipReq("/ingest/chrome"), contentType: "text/html",
			body: "<span id=\"ingChip\"></span>", wantGzip: false,
		},
		{
			name:        "skips a client that did not ask",
			req:         httptest.NewRequest(http.MethodGet, "/feed", nil),
			contentType: "text/html", body: big, wantGzip: false,
		},
		{
			name: "honours gzip;q=0 as a refusal",
			req: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "/feed", nil)
				r.Header.Set("Accept-Encoding", "gzip;q=0")
				return r
			}(),
			contentType: "text/html", body: big, wantGzip: false,
		},
		{
			name: "skips a range request",
			req: func() *http.Request {
				r := gzipReq("/static/style.css")
				r.Header.Set("Range", "bytes=0-99")
				return r
			}(),
			contentType: "text/css", body: big, wantGzip: false,
		},
		{
			name: "leaves a 304 without an encoding",
			req:  gzipReq("/static/style.css"), contentType: "text/css",
			status: http.StatusNotModified, wantGzip: false,
		},
		{
			name: "sniffs the type when the handler sets none",
			req:  gzipReq("/feed"), contentType: "",
			body: "<!doctype html>" + big, wantGzip: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := serve(t, tc.req, tc.contentType, tc.body, tc.status)

			gotGzip := rec.Header().Get("Content-Encoding") == "gzip"
			if gotGzip != tc.wantGzip {
				t.Fatalf("Content-Encoding gzip = %v, want %v", gotGzip, tc.wantGzip)
			}
			// Whatever was decided, the client must get back what the handler
			// wrote — compression is a transport detail, not a change of body.
			got := rec.Body.String()
			if gotGzip {
				zr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
				if err != nil {
					t.Fatalf("gzip.NewReader: %v", err)
				}
				plain, err := io.ReadAll(zr)
				if err != nil {
					t.Fatalf("read gzip body: %v", err)
				}
				got = string(plain)
			}
			if got != tc.body {
				t.Errorf("body round-trip lost data: got %d bytes, want %d", len(got), len(tc.body))
			}
			// A compressed response must not keep the uncompressed length.
			if gotGzip && rec.Header().Get("Content-Length") != "" {
				t.Error("Content-Length left on a compressed response")
			}
		})
	}
}

// Vary has to be set whichever way the decision goes, or a shared cache can
// hand a gzip body to a client that never asked for one.
func TestVaryAlwaysSetForGzipCapableClients(t *testing.T) {
	for _, body := range []string{"tiny", strings.Repeat("x", 4096)} {
		rec := serve(t, gzipReq("/feed"), "text/html", body, 0)
		if got := rec.Header().Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
			t.Errorf("body %d bytes: Vary = %q, want it to include Accept-Encoding", len(body), got)
		}
	}
}

// The saving is the whole point, so assert it actually happens on a realistic
// stylesheet-shaped payload rather than only that the header is present.
func TestCompressionActuallyShrinks(t *testing.T) {
	body := strings.Repeat(".rabbit{color:var(--accent);background:var(--fill);}\n", 400)
	rec := serve(t, gzipReq("/static/style.css"), "text/css", body, 0)
	if rec.Body.Len() >= len(body)/2 {
		t.Errorf("compressed to %d bytes from %d; expected at least half off", rec.Body.Len(), len(body))
	}
}

// A handler that flushes wants bytes out now; the response must still be a
// valid, complete gzip stream at the end of it.
func TestFlushMidResponse(t *testing.T) {
	rec := httptest.NewRecorder()
	Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, strings.Repeat("a", 600))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		_, _ = io.WriteString(w, strings.Repeat("b", 600))
	})).ServeHTTP(rec, gzipReq("/feed"))

	zr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	got, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read flushed body: %v", err)
	}
	if want := strings.Repeat("a", 600) + strings.Repeat("b", 600); string(got) != want {
		t.Errorf("flushed body = %d bytes, want %d", len(got), len(want))
	}
}
