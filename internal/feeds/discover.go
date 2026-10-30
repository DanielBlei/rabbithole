// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package feeds

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mmcdole/gofeed"
	"github.com/rs/zerolog"
	"golang.org/x/net/html"

	"github.com/DanielBlei/rabbithole/internal/httpclient"
)

const (
	// MaxPageBytes caps how much of a web page Discover reads to find feed links.
	MaxPageBytes = 2 << 20
	// MaxCandidates caps how many feed addresses Discover verifies and returns.
	MaxCandidates = 5
	// DiscoverTimeout bounds one whole Discover call.
	DiscoverTimeout = 20 * time.Second

	requestTimeout = 10 * time.Second
	// verifyParallel is how many candidates are verified at once.
	verifyParallel = 3
	// previewEntries is how many recent entries a candidate or preview carries.
	previewEntries = 3
)

// fallbackPaths are tried against the site root when a page declares no feed.
var fallbackPaths = []string{"/feed", "/rss", "/rss.xml", "/atom.xml", "/index.xml"}

// Kind is a feed's format.
type Kind string

// The feed formats gofeed reports.
const (
	KindRSS  Kind = "rss"
	KindAtom Kind = "atom"
	KindJSON Kind = "json"
)

var (
	// ErrNoFeed means the page declares no feed and no common feed path answered.
	ErrNoFeed = errors.New("no feed found")
	// ErrNotPage means the address served neither a web page nor a feed.
	ErrNotPage = errors.New("not a web page")
)

// Entry is one post of a feed.
type Entry struct {
	Title     string
	Published time.Time
}

// Candidate is a verified feed address. Latest holds the newest entries.
type Candidate struct {
	URL       string
	Title     string
	Kind      Kind
	ItemCount int
	Latest    []Entry
}

// PreviewResult is a read-only look at one feed.
type PreviewResult struct {
	Title string
	Items []Entry
	Count int
}

// Discover finds the feeds behind a site or feed address. An address that is
// already a feed comes back as the only candidate after a single request.
// Otherwise the page head is scanned for declared feeds, falling back to common
// paths when it declares none, and each candidate is fetched and parsed before
// it is returned. Candidates that fail to fetch or parse are dropped.
func Discover(ctx context.Context, client *http.Client, rawURL string) ([]Candidate, error) {
	if client == nil {
		client = NewClient("")
	}
	ctx, cancel := context.WithTimeout(ctx, DiscoverTimeout)
	defer cancel()

	rawURL = strings.TrimSpace(rawURL)
	pg, err := loadPage(ctx, client, rawURL)
	if err != nil {
		return nil, err
	}
	if pg.feed != nil {
		return []Candidate{candidateFrom(pg.feed, feedAddress(rawURL, pg.final), "")}, nil
	}

	refs := declaredFeeds(pg.head, pg.final)
	if len(refs) == 0 {
		refs = fallbackFeeds(pg.final)
	}
	if len(refs) > MaxCandidates {
		refs = refs[:MaxCandidates]
	}

	found := verifyAll(ctx, client, refs)
	if len(found) == 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, ErrNoFeed
	}
	return found, nil
}

// Preview fetches one feed and returns its title, size and newest entries.
func Preview(ctx context.Context, client *http.Client, rawURL string) (PreviewResult, error) {
	if client == nil {
		client = NewClient("")
	}
	feed, err := parseFeedURL(ctx, newParser(client), strings.TrimSpace(rawURL))
	if err != nil {
		return PreviewResult{}, err
	}
	return PreviewResult{
		Title: CollapseWhitespace(feed.Title),
		Items: latestEntries(feed.Items),
		Count: len(feed.Items),
	}, nil
}

// page is what the first request produced: a parsed feed when the address is
// one, otherwise the leading bytes of the page and its final address.
type page struct {
	final *url.URL
	feed  *gofeed.Feed
	head  []byte
}

func loadPage(ctx context.Context, client *http.Client, rawURL string) (page, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return page{}, err
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/rss+xml,application/atom+xml,"+
		"application/feed+json,application/xml;q=0.9,text/xml;q=0.9,*/*;q=0.5")
	resp, err := client.Do(req)
	if err != nil {
		return page{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return page{}, fmt.Errorf("http error: %s", resp.Status)
	}

	pg := page{final: resp.Request.URL}
	read, err := io.ReadAll(io.LimitReader(resp.Body, MaxPageBytes+1))
	if err != nil {
		return page{}, err
	}
	pg.head = read[:min(len(read), MaxPageBytes)]

	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if feedMediaType(mt) || sniffFeed(pg.head) {
		// The first bytes are already read; the rest streams under the feed cap.
		feed, err := parseStream(newParser(client), io.MultiReader(bytes.NewReader(read), resp.Body))
		if err == nil {
			pg.feed = feed
			return pg, nil
		}
		// A page that merely starts like XML can pass the sniff and still be a page.
		if errors.Is(err, httpclient.ErrTooLarge) && !htmlMediaType(mt) {
			return page{}, err
		}
	}
	if !pageMediaType(mt) {
		return page{}, ErrNotPage
	}
	return pg, nil
}

// feedAddress is the address a feed is stored under: the one typed, unless the
// site only upgraded it from http to https.
func feedAddress(typed string, final *url.URL) string {
	u, err := url.Parse(typed)
	if err != nil || u.Scheme != "http" || final.Scheme != "https" {
		return typed
	}
	u.Scheme, u.Fragment, u.RawFragment = "https", "", ""
	upgraded := *final
	upgraded.Fragment, upgraded.RawFragment = "", ""
	if u.String() != upgraded.String() {
		return typed
	}
	return upgraded.String()
}

// htmlMediaType reports whether a content type names a web page.
func htmlMediaType(mt string) bool {
	return mt == "text/html" || mt == "application/xhtml+xml"
}

// feedMediaType reports whether a content type names a feed or generic XML/JSON.
func feedMediaType(mt string) bool {
	switch mt {
	case "application/rss+xml", "application/atom+xml", "application/rdf+xml", "application/feed+json",
		"application/xml", "text/xml", "application/json":
		return true
	}
	return false
}

// pageMediaType reports whether a content type may be a web page. A missing
// type is allowed.
func pageMediaType(mt string) bool {
	return mt == "" || strings.HasPrefix(mt, "text/") || mt == "application/xhtml+xml"
}

// sniffFeed reports whether the leading bytes look like a feed document.
func sniffFeed(b []byte) bool {
	b = bytes.TrimLeft(bytes.TrimPrefix(bytes.TrimLeft(b, " \t\r\n"), []byte("\xef\xbb\xbf")), " \t\r\n")
	if bytes.HasPrefix(b, []byte("{")) || bytes.HasPrefix(b, []byte("<?xml")) {
		return true
	}
	head := bytes.ToLower(b[:min(len(b), 16)])
	for _, p := range []string{"<rss", "<feed", "<rdf:rdf"} {
		if bytes.HasPrefix(head, []byte(p)) {
			return true
		}
	}
	return false
}

// errTrack remembers the first non-EOF read error, so an oversize body is
// recognised however the parser wraps it.
type errTrack struct {
	r   io.Reader
	err error
}

func (t *errTrack) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if err != nil && err != io.EOF && t.err == nil {
		t.err = err
	}
	return n, err
}

// parseStream parses a feed from r, failing with httpclient.ErrTooLarge past MaxFeedBytes.
func parseStream(parser *gofeed.Parser, r io.Reader) (*gofeed.Feed, error) {
	tr := &errTrack{r: httpclient.LimitBody(r, MaxFeedBytes)}
	feed, err := parser.Parse(tr)
	if err == nil {
		// XML parsers stop at the root's closing tag; read the rest through the cap.
		_, err = io.Copy(io.Discard, tr)
	}
	if errors.Is(tr.err, httpclient.ErrTooLarge) {
		return nil, fmt.Errorf("%w: over %d bytes", httpclient.ErrTooLarge, MaxFeedBytes)
	}
	if err != nil {
		return nil, err
	}
	return feed, nil
}

// parseFeedURL fetches and parses one feed within requestTimeout.
func parseFeedURL(ctx context.Context, parser *gofeed.Parser, rawURL string) (*gofeed.Feed, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	feed, err := parser.ParseURLWithContext(rawURL, ctx)
	if errors.Is(err, gofeed.ErrResponseTooLarge) {
		return nil, fmt.Errorf("%w: over %d bytes", httpclient.ErrTooLarge, MaxFeedBytes)
	}
	return feed, err
}

// feedRef is a feed address to verify and the title the page gave it.
type feedRef struct {
	url   string
	title string
}

// declaredFeeds returns the feeds a page head advertises with
// <link rel="alternate">, resolved against <base href> or the final address,
// deduplicated, in document order with comment feeds last.
func declaredFeeds(head []byte, final *url.URL) []feedRef {
	type link struct{ href, title string }
	var links []link
	base := headLinks(bytes.NewReader(head), final, func(attrs map[string]string) {
		if !hasToken(attrs["rel"], "alternate") || !feedLinkType(attrs["type"]) {
			return
		}
		if href := strings.TrimSpace(attrs["href"]); href != "" {
			links = append(links, link{href: href, title: CollapseWhitespace(attrs["title"])})
		}
	})

	var posts, comments []feedRef
	seen := map[string]bool{}
	for _, l := range links {
		u, err := base.Parse(l.href)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			continue
		}
		u.Fragment, u.RawFragment = "", ""
		ref := feedRef{url: u.String(), title: l.title}
		key := dedupeKey(ref.url)
		if seen[key] {
			continue
		}
		seen[key] = true
		if isCommentsFeed(ref) {
			comments = append(comments, ref)
		} else {
			posts = append(posts, ref)
		}
	}
	return append(posts, comments...)
}

// headLinks scans a page head, calling visit with the attributes of each <link>
// until </head> or <body>. It returns the address hrefs resolve against: the
// first <base href> resolved against final, else final.
func headLinks(r io.Reader, final *url.URL, visit func(attrs map[string]string)) *url.URL {
	var baseRaw string
	z := html.NewTokenizer(r)
scan:
	for {
		switch z.Next() {
		case html.ErrorToken:
			break scan
		case html.EndTagToken:
			if name, _ := z.TagName(); string(name) == "head" {
				break scan
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			switch string(name) {
			case "body":
				break scan
			case "base":
				attrs := tagAttrs(z, hasAttr)
				if href := strings.TrimSpace(attrs["href"]); baseRaw == "" && href != "" {
					baseRaw = href
				}
			case "link":
				visit(tagAttrs(z, hasAttr))
			}
		}
	}
	if baseRaw != "" {
		if u, err := final.Parse(baseRaw); err == nil {
			return u
		}
	}
	return final
}

// tagAttrs returns the current tag's attributes, keyed by lowercase name; the
// first occurrence of a repeated attribute wins.
func tagAttrs(z *html.Tokenizer, hasAttr bool) map[string]string {
	attrs := map[string]string{}
	for hasAttr {
		var k, v []byte
		k, v, hasAttr = z.TagAttr()
		if _, ok := attrs[string(k)]; !ok {
			attrs[string(k)] = string(v)
		}
	}
	return attrs
}

func hasToken(value, token string) bool {
	for _, f := range strings.Fields(strings.ToLower(value)) {
		if f == token {
			return true
		}
	}
	return false
}

func feedLinkType(t string) bool {
	mt, _, _ := mime.ParseMediaType(t)
	switch mt {
	case "application/rss+xml", "application/atom+xml", "application/feed+json":
		return true
	}
	return false
}

// isCommentsFeed spots feeds of reader comments, which are rarely what a
// subscriber wants.
func isCommentsFeed(r feedRef) bool {
	s := strings.ToLower(r.title)
	if u, err := url.Parse(r.url); err == nil {
		s += " " + strings.ToLower(u.Path)
	}
	return strings.Contains(s, "comment")
}

// fallbackFeeds are the common feed paths on the site root of final.
func fallbackFeeds(final *url.URL) []feedRef {
	refs := make([]feedRef, 0, len(fallbackPaths))
	for _, p := range fallbackPaths {
		u := url.URL{Scheme: final.Scheme, Host: final.Host, Path: p}
		refs = append(refs, feedRef{url: u.String()})
	}
	return refs
}

// verifyAll fetches and parses every ref, at most verifyParallel at a time,
// and returns the ones that are feeds in their original order.
func verifyAll(ctx context.Context, client *http.Client, refs []feedRef) []Candidate {
	logger := zerolog.Ctx(ctx)
	out := make([]*Candidate, len(refs))
	sem := make(chan struct{}, verifyParallel)
	var wg sync.WaitGroup
	for i, ref := range refs {
		wg.Add(1)
		go func(i int, ref feedRef) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			// gofeed.Parser is not goroutine-safe: one per goroutine.
			feed, err := parseFeedURL(ctx, newParser(client), ref.url)
			if err != nil {
				logger.Debug().Str("url", ref.url).Err(err).Msg("skipping feed candidate")
				return
			}
			c := candidateFrom(feed, ref.url, ref.title)
			out[i] = &c
		}(i, ref)
	}
	wg.Wait()

	var found []Candidate
	for _, c := range out {
		if c != nil {
			found = append(found, *c)
		}
	}
	return found
}

// candidateFrom builds a candidate from a parsed feed. The title falls back to
// the page's own name for the link, then to the host.
func candidateFrom(feed *gofeed.Feed, feedURL, linkTitle string) Candidate {
	title := CollapseWhitespace(feed.Title)
	if title == "" {
		title = linkTitle
	}
	if title == "" {
		if u, err := url.Parse(feedURL); err == nil {
			title = u.Host
		}
	}
	return Candidate{
		URL:       feedURL,
		Title:     title,
		Kind:      Kind(feed.FeedType),
		ItemCount: len(feed.Items),
		Latest:    latestEntries(feed.Items),
	}
}

// latestEntries returns the newest previewEntries items, newest first; undated
// items sort last.
func latestEntries(items []*gofeed.Item) []Entry {
	entries := make([]Entry, 0, len(items))
	for _, it := range items {
		var published time.Time
		if it.PublishedParsed != nil {
			published = *it.PublishedParsed
		} else if it.UpdatedParsed != nil {
			published = *it.UpdatedParsed
		}
		entries = append(entries, Entry{Title: CollapseWhitespace(it.Title), Published: published})
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Published.After(entries[j].Published) })
	return entries[:min(len(entries), previewEntries)]
}

// dedupeKey mirrors config.FeedURLKey: two addresses sharing a key point at the
// same feed. It lives here because config imports this package.
func dedupeKey(raw string) string {
	s := strings.TrimSpace(raw)
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return strings.ToLower(s)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.Fragment = ""
	u.RawFragment = ""
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = strings.TrimRight(u.RawPath, "/")
	return u.String()
}
