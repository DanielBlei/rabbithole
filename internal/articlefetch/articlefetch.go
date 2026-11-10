// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

// Package articlefetch retrieves one article page on demand for a Decision
// Brief. It is deliberately stricter than the feed client: article fetches
// never reach private, loopback, link-local or metadata addresses, use only
// ports 80 and 443, and never consult a proxy from the environment.
package articlefetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"time"

	"github.com/DanielBlei/rabbithole/internal/httpclient"
)

const (
	// DefaultMaxArticleBytes bounds one fetched page body.
	DefaultMaxArticleBytes int64 = 5 << 20
	// DefaultTimeout bounds one fetch end to end, including the body.
	DefaultTimeout = 20 * time.Second
	// DefaultMaxRedirects is the redirect chain an article fetch follows.
	DefaultMaxRedirects = 5

	acceptHTML = "text/html,application/xhtml+xml;q=0.9,*/*;q=0.5"
)

var (
	// ErrNoLink marks an item whose link is missing or not a usable URL.
	ErrNoLink = errors.New("article has no usable link")
	// ErrBlocked marks a destination refused by the scheme, address or port policy.
	ErrBlocked = errors.New("article destination blocked")
	// ErrTooManyRedirects marks a redirect chain past the configured cap.
	ErrTooManyRedirects = errors.New("too many article redirects")
	// ErrTimeout marks a fetch that ran out of its time budget.
	ErrTimeout = errors.New("article fetch timed out")
	// ErrTooLarge marks a page body over the configured cap. It is the same
	// sentinel LimitBody returns, so overflow is recognizable while reading.
	ErrTooLarge = httpclient.ErrTooLarge
	// ErrUnsupportedContentType marks a response that is not HTML.
	ErrUnsupportedContentType = errors.New("unsupported article content type")
	// ErrHTTPStatus marks a non-2xx article response.
	ErrHTTPStatus = errors.New("article returned a non-success status")
)

// Limits sizes one fetch. Zero values take the documented defaults.
type Limits struct {
	MaxArticleBytes int64
	Timeout         time.Duration
	MaxRedirects    int
	UserAgent       string
}

func (l Limits) withDefaults() Limits {
	if l.MaxArticleBytes <= 0 {
		l.MaxArticleBytes = DefaultMaxArticleBytes
	}
	if l.Timeout <= 0 {
		l.Timeout = DefaultTimeout
	}
	if l.MaxRedirects <= 0 {
		l.MaxRedirects = DefaultMaxRedirects
	}
	if l.UserAgent == "" {
		l.UserAgent = DefaultUserAgent()
	}
	return l
}

// DefaultUserAgent identifies this build to publishers.
func DefaultUserAgent() string {
	version := "dev"
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			version = v
		}
	}
	return "rabbithole/" + version + " (+https://github.com/DanielBlei/rabbithole)"
}

// Result is one fetched page. Body is already bounded by Limits.MaxArticleBytes;
// callers must close it.
type Result struct {
	Body        io.ReadCloser
	ContentType string
	FinalURL    string
	Elapsed     time.Duration
}

// Fetch retrieves rawURL under the article fetch policy.
func Fetch(ctx context.Context, rawURL string, limits Limits) (Result, error) {
	return fetch(ctx, rawURL, limits, nil)
}

// fetch is Fetch with an optional client override for tests. Production always
// passes nil and gets the hardened client built here.
func fetch(ctx context.Context, rawURL string, limits Limits, client *http.Client) (Result, error) {
	limits = limits.withDefaults()
	target, err := validateURL(rawURL)
	if err != nil {
		return Result{}, err
	}
	if client == nil {
		client = httpclient.NewSafe(httpclient.Options{
			Timeout:      limits.Timeout,
			AnyPort:      false,
			MaxRedirects: limits.MaxRedirects,
			UserAgent:    limits.UserAgent,
			AllowPrivate: false,
			Proxy:        nil,
		})
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrNoLink, err)
	}
	req.Header.Set("Accept", acceptHTML)

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return Result{}, classify(err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, httpclient.LimitBody(resp.Body, 1<<20))
		_ = resp.Body.Close()
		return Result{}, fmt.Errorf("%w: HTTP %d", ErrHTTPStatus, resp.StatusCode)
	}
	contentType, err := htmlContentType(resp.Header.Get("Content-Type"))
	if err != nil {
		_ = resp.Body.Close()
		return Result{}, err
	}
	finalURL := target.String()
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}
	return Result{
		Body:        &limitedBody{Reader: httpclient.LimitBody(resp.Body, limits.MaxArticleBytes), Closer: resp.Body},
		ContentType: contentType,
		FinalURL:    finalURL,
		Elapsed:     time.Since(start),
	}, nil
}

// validateURL accepts only absolute http(s) URLs with a host.
func validateURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, ErrNoLink
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %q", ErrNoLink, raw)
	}
	if u.Scheme == "" {
		return nil, fmt.Errorf("%w: %q", ErrNoLink, raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("%w: %s", ErrBlocked, u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("%w: %q", ErrNoLink, raw)
	}
	return u, nil
}

func htmlContentType(header string) (string, error) {
	mediaType, _, err := mime.ParseMediaType(header)
	if err != nil {
		return "", fmt.Errorf("%w: %q", ErrUnsupportedContentType, header)
	}
	switch strings.ToLower(mediaType) {
	case "text/html", "application/xhtml+xml":
		return mediaType, nil
	default:
		return "", fmt.Errorf("%w: %s", ErrUnsupportedContentType, mediaType)
	}
}

// classify maps transport failures onto the stable product failure classes.
func classify(err error) error {
	switch {
	case errors.Is(err, httpclient.ErrTooManyRedirects):
		return fmt.Errorf("%w: %v", ErrTooManyRedirects, err)
	case errors.Is(err, httpclient.ErrUnsupportedScheme),
		errors.Is(err, httpclient.ErrBlockedAddress):
		return fmt.Errorf("%w: %v", ErrBlocked, err)
	case errors.Is(err, httpclient.ErrTooLarge):
		return fmt.Errorf("%w: %v", ErrTooLarge, err)
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return fmt.Errorf("%w: %v", ErrTimeout, err)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return fmt.Errorf("%w: %v", ErrTimeout, err)
	}
	// http.Client.Timeout surfaces as a *url.Error whose Timeout is true, but a
	// wrapped error may only expose the text; keep the common case readable.
	if strings.Contains(err.Error(), "Client.Timeout") ||
		strings.Contains(err.Error(), "context deadline exceeded") {
		return fmt.Errorf("%w: %v", ErrTimeout, err)
	}
	return err
}

// limitedBody pairs a limited reader with the response body so closing the
// result releases the connection.
type limitedBody struct {
	io.Reader
	io.Closer
}
