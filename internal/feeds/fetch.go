// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package feeds

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/mmcdole/gofeed"
	"github.com/rs/zerolog"

	"github.com/DanielBlei/rabbithole/internal/httpclient"
)

// FetchTimeout bounds how long one source may take to fetch.
const FetchTimeout = 30 * time.Second

const (
	fetchTimeout = FetchTimeout
	maxParallel  = 8
	// MaxFeedBytes caps one feed response body.
	MaxFeedBytes = 16 << 20
	// maxFeedRedirects is the redirect chain a feed fetch follows.
	maxFeedRedirects = 5
	// summaryLimit caps the summary text sent to the scoring model.
	// Most RSS feeds only provide a short excerpt (300-600 chars) in
	// <description>, not the full article; the limit guards against
	// the rare feed that inlines entire posts via <content:encoded>.
	summaryLimit = 1500
)

// Source identifies a feed to fetch. Tags are the feed's configured labels,
type Source struct {
	Name string
	URL  string
	Tags []string
}

// Result is one source's fetch outcome. A failed fetch is a Result with Err
// set and no Items — failures are reported, not swallowed, so the caller can
// record feed health and still carry on with the sources that did work.
type Result struct {
	Source Source
	Items  []Item
	Err    error
	// Site is the feed's own site address from its link, empty when the feed
	// declares none or the source is not a feed.
	Site    string
	Elapsed time.Duration
	At      time.Time // when the fetch finished
}

// NewClient returns the client feed ingestion fetches through: it refuses
// private and metadata addresses (the private ranges open with
// RABBITHOLE_ALLOW_PRIVATE_FETCH), follows a few redirects, honours the proxy
// environment and identifies itself as rabbithole/version.
func NewClient(version string) *http.Client {
	if version == "" {
		version = "dev"
	}
	return httpclient.NewSafe(httpclient.Options{
		Timeout:      fetchTimeout,
		AnyPort:      true,
		MaxRedirects: maxFeedRedirects,
		UserAgent:    "rabbithole/" + version + " (+https://github.com/DanielBlei/rabbithole)",
		AllowPrivate: httpclient.AllowPrivateFromEnv(),
		Proxy:        http.ProxyFromEnvironment,
	})
}

// FetchAll fetches every source concurrently through client and returns one
// Result per source, in the order the sources were given. A nil client means
// NewClient. Individual feed failures are logged and carried on the Result
// rather than failing the run.
func FetchAll(ctx context.Context, client *http.Client, sources []Source) []Result {
	if client == nil {
		client = NewClient("")
	}
	logger := zerolog.Ctx(ctx)
	sem := make(chan struct{}, maxParallel)
	// Indexed writes rather than an append under a mutex, so results come back
	// in source order and a run's logs/health rows read predictably.
	results := make([]Result, len(sources))
	var wg sync.WaitGroup
	for i, src := range sources {
		wg.Add(1)
		go func(i int, src Source) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			logger.Debug().Str("feed", src.Name).Msg("fetching feed")
			start := time.Now()
			// gofeed.Parser lazily initializes internal state on first use and
			// is not goroutine-safe, so each fetch gets its own (cheap) parser.
			items, site, err := fetchOne(ctx, newParser(client), src)
			res := Result{Source: src, Items: items, Site: site, Err: err, Elapsed: time.Since(start), At: time.Now()}
			if err != nil {
				logger.Warn().Str("feed", src.Name).Err(err).Msg("skipping feed")
			} else {
				logger.Debug().Str("feed", src.Name).Int("items", len(items)).
					Str("elapsed", res.Elapsed.Round(100*time.Millisecond).String()).Msg("feed fetched")
			}
			results[i] = res
		}(i, src)
	}
	wg.Wait()
	return results
}

// newParser returns a parser bound to client. The User-Agent is left empty so
// the client's own applies, and a body past MaxFeedBytes fails the parse.
func newParser(client *http.Client) *gofeed.Parser {
	p := gofeed.NewParser()
	p.Client = client
	p.UserAgent = ""
	p.MaxByteSize = MaxFeedBytes
	return p
}

// fetchOne returns a feed's items and its site address (http or https only).
func fetchOne(ctx context.Context, parser *gofeed.Parser, src Source) ([]Item, string, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	feed, err := parser.ParseURLWithContext(src.URL, ctx)
	if errors.Is(err, gofeed.ErrResponseTooLarge) {
		return nil, "", fmt.Errorf("%w: over %d bytes", httpclient.ErrTooLarge, MaxFeedBytes)
	}
	if err != nil {
		return nil, "", err
	}
	items := make([]Item, 0, len(feed.Items))
	for _, it := range feed.Items {
		var published time.Time
		if it.PublishedParsed != nil {
			published = *it.PublishedParsed
		} else if it.UpdatedParsed != nil {
			published = *it.UpdatedParsed
		}
		summary := it.Description
		if summary == "" {
			summary = it.Content
		}
		items = append(items, Item{
			ID:        makeID(it.GUID, it.Link),
			Source:    src.Name,
			Title:     CollapseWhitespace(it.Title),
			Link:      it.Link,
			Summary:   cleanSummary(summary),
			Published: published,
			Tags:      src.Tags,
		})
	}
	return items, siteLink(src.URL, feed.Link), nil
}

// siteLink resolves a channel link against the feed's address and keeps it only
// when it is an http(s) address.
func siteLink(feedURL, link string) string {
	link = strings.TrimSpace(link)
	base, err := url.Parse(feedURL)
	if link == "" || err != nil {
		return ""
	}
	ref, err := url.Parse(link)
	if err != nil {
		return ""
	}
	site := base.ResolveReference(ref).String()
	if SiteOrigin(site) == "" {
		return ""
	}
	return site
}

var tagRE = regexp.MustCompile(`<[^>]*>`)

// CollapseWhitespace turns any run of whitespace, including newlines, into a single space, so
// a title or summary can't sneak in an extra line that looks like part of the prompt's structure.
func CollapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// cleanSummary strips HTML tags, unescapes entities, collapses whitespace,
// and truncates to summaryLimit so scoring prompts stay small.
func cleanSummary(s string) string {
	s = tagRE.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	s = CollapseWhitespace(s)
	if len(s) > summaryLimit {
		s = s[:summaryLimit] + "…"
	}
	return s
}
