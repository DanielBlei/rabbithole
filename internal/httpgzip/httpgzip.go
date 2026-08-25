// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

// Package httpgzip is response compression: one middleware that gzips the
// text-shaped responses, for clients that asked for it.
//
// It sits inside the access log, so the "bytes" a request logs are the bytes
// that went on the wire rather than the ones the handler wrote.
package httpgzip

import (
	"compress/gzip"
	"net/http"
	"strings"
	"sync"
)

// minSize is the response size below which compressing is not worth doing: the
// gzip header and checksum cost about twenty bytes, and a short htmx fragment
// can come back out larger than it went in. Writes are held until this much has
// arrived and the decision is then made once, so a small response pays nothing.
const minSize = 512

// One pool for the whole server: a gzip.Writer carries a 32KB+ window, and
// allocating one per response would undo most of what is saved.
var writerPool = sync.Pool{New: func() any { return gzip.NewWriter(nil) }}

// Middleware compresses responses whose content type is text-shaped — HTML,
// CSS, JavaScript, JSON, SVG — when the client advertises gzip.
//
// Already-compressed formats are left alone: woff2, .ico and PNG are containers
// with their own compression, and running them through gzip spends CPU to make
// them very slightly bigger.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A range request asks for bytes at offsets in the identity encoding;
		// compressing it would answer with offsets into a different stream.
		if !acceptsGzip(r) || r.Header.Get("Range") != "" {
			next.ServeHTTP(w, r)
			return
		}
		// Announced whether or not this particular response ends up compressed:
		// it says the URL has more than one representation, so a shared cache
		// must key on the encoding rather than serving gzip to a client that
		// cannot read it.
		w.Header().Add("Vary", "Accept-Encoding")

		cw := &compressWriter{ResponseWriter: w, status: http.StatusOK}
		defer cw.finish()
		next.ServeHTTP(cw, r)
	})
}

func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(name), "gzip") {
			continue
		}
		// "gzip;q=0" is a client saying explicitly that it does not want it.
		return !strings.EqualFold(strings.ReplaceAll(params, " ", ""), "q=0")
	}
	return false
}

// compressible reports whether a content type is worth gzipping. Text shapes
// compress by 70-90%; everything else is either already compressed or too small
// a win to be worth the CPU.
func compressible(contentType string) bool {
	mediaType, _, _ := strings.Cut(contentType, ";")
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	if strings.HasPrefix(mediaType, "text/") {
		return true
	}
	for _, shape := range []string{"json", "javascript", "xml", "svg", "wasm"} {
		if strings.Contains(mediaType, shape) {
			return true
		}
	}
	return false
}

// bodyless reports the statuses that carry no body, where an encoding header
// would describe something that is not there. 304 matters most: it is the
// answer to a conditional request for a static asset.
func bodyless(status int) bool {
	return status == http.StatusNoContent ||
		status == http.StatusNotModified ||
		(status >= 100 && status < 200)
}

// compressWriter defers the compress-or-not decision until it has seen enough
// of the body to judge, because the content type is often not known until the
// first Write — handlers that do not set it leave net/http to sniff.
//
// Until it settles, writes accumulate in held. Settling writes the status line,
// so nothing reaches the client before the encoding is decided.
type compressWriter struct {
	http.ResponseWriter
	gz      *gzip.Writer
	held    []byte
	status  int
	settled bool
	gzipped bool
}

// Unwrap keeps http.ResponseController working through this wrapper, the way
// the access log's recorder does — Flush and the deadline setters travel down
// to the real writer.
func (cw *compressWriter) Unwrap() http.ResponseWriter { return cw.ResponseWriter }

func (cw *compressWriter) WriteHeader(status int) {
	if cw.settled {
		return
	}
	cw.status = status
	// Nothing more is coming for these, so there is nothing to judge.
	if bodyless(status) {
		cw.settle(false)
	}
}

func (cw *compressWriter) Write(b []byte) (int, error) {
	if cw.settled {
		return cw.write(b)
	}
	cw.held = append(cw.held, b...)
	if len(cw.held) < minSize {
		// Reported as written: it is buffered, and finish always drains it.
		return len(b), nil
	}
	cw.settle(cw.worthIt())
	held := cw.held
	cw.held = nil
	if _, err := cw.write(held); err != nil {
		return 0, err
	}
	return len(b), nil
}

// Flush settles on whatever is known so far. A handler that flushes is asking
// for bytes to reach the client now, which outranks waiting for a better
// decision.
func (cw *compressWriter) Flush() {
	if !cw.settled {
		cw.settle(cw.worthIt())
		held := cw.held
		cw.held = nil
		_, _ = cw.write(held)
	}
	if cw.gz != nil {
		_ = cw.gz.Flush()
	}
	//nolint:bodyclose // not a body; this is the response writer's flusher.
	if f, ok := cw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// worthIt judges the held bytes: the declared content type if the handler set
// one, and net/http's own sniffing if it did not.
func (cw *compressWriter) worthIt() bool {
	if cw.ResponseWriter.Header().Get("Content-Encoding") != "" {
		return false // already encoded by the handler; do not stack another
	}
	contentType := cw.ResponseWriter.Header().Get("Content-Type")
	if contentType == "" {
		contentType = http.DetectContentType(cw.held)
	}
	return compressible(contentType)
}

func (cw *compressWriter) settle(useGzip bool) {
	if cw.settled {
		return
	}
	cw.settled, cw.gzipped = true, useGzip
	if useGzip {
		header := cw.ResponseWriter.Header()
		header.Set("Content-Encoding", "gzip")
		// The handler's length describes the uncompressed body, and the
		// compressed one is not known until the stream closes.
		header.Del("Content-Length")
		cw.gz = writerPool.Get().(*gzip.Writer)
		cw.gz.Reset(cw.ResponseWriter)
	}
	cw.ResponseWriter.WriteHeader(cw.status)
}

func (cw *compressWriter) write(b []byte) (int, error) {
	if cw.gzipped {
		return cw.gz.Write(b)
	}
	return cw.ResponseWriter.Write(b)
}

// finish drains anything still held and closes the stream. Deferred by the
// middleware, so it runs even if the handler panics past its own writes.
func (cw *compressWriter) finish() {
	if !cw.settled {
		// Under minSize, so never worth compressing whatever it is.
		cw.settle(false)
		if len(cw.held) > 0 {
			_, _ = cw.write(cw.held)
			cw.held = nil
		}
	}
	if cw.gz != nil {
		_ = cw.gz.Close()
		writerPool.Put(cw.gz)
		cw.gz = nil
	}
}
