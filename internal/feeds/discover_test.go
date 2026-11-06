// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package feeds_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/httpclient"
)

const (
	typeRSS  = "application/rss+xml"
	typeAtom = "application/atom+xml"
	typeJSON = "application/feed+json"
	typeHTML = "text/html; charset=utf-8"
)

func rssDoc(title string, items int) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><rss version="2.0"><channel><title>` + title + `</title>`)
	for i := range items {
		fmt.Fprintf(&b, `<item><title>Post %d</title><link>https://example.com/%d</link>`+
			`<pubDate>Mon, %02d Jan 2024 10:00:00 GMT</pubDate></item>`, i, i, i+1)
	}
	b.WriteString(`</channel></rss>`)
	return b.String()
}

const atomDoc = `<?xml version="1.0"?><feed xmlns="http://www.w3.org/2005/Atom"><title>Atom Blog</title>
<entry><title>One</title><id>1</id><updated>2024-03-01T00:00:00Z</updated></entry>
<entry><title>Two</title><id>2</id><updated>2024-03-02T00:00:00Z</updated></entry></feed>`

const jsonDoc = `{"version":"https://jsonfeed.org/version/1.1","title":"JSON Blog","items":[
{"id":"1","title":"Alpha","date_published":"2024-05-01T00:00:00Z"}]}`

// resp is what a test site serves at a path.
type resp struct {
	ctype    string
	body     string
	status   int
	redirect string
	delay    time.Duration
}

// site is a test server that counts requests per path.
type site struct {
	*httptest.Server
	mu    sync.Mutex
	paths map[string]resp
	hits  map[string]int
}

func newSite(t *testing.T, paths map[string]resp) *site {
	t.Helper()
	s := &site{paths: paths, hits: map[string]int{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.hits[r.URL.Path]++
		rp, ok := s.paths[r.URL.Path]
		s.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		if rp.delay > 0 {
			select {
			case <-time.After(rp.delay):
			case <-r.Context().Done():
				return
			}
		}
		if rp.redirect != "" {
			http.Redirect(w, r, rp.redirect, http.StatusFound)
			return
		}
		if rp.ctype != "" {
			w.Header().Set("Content-Type", rp.ctype)
		}
		if rp.status != 0 {
			w.WriteHeader(rp.status)
		}
		_, _ = w.Write([]byte(rp.body))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *site) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.hits {
		n += c
	}
	return n
}

func (s *site) count(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[path]
}

func head(links string) resp {
	return resp{ctype: typeHTML, body: `<!doctype html><html><head><title>Site</title>` + links +
		`</head><body><p>hi</p></body></html>`}
}

func urls(cs []feeds.Candidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.URL
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestDiscoverDeclaredLinks(t *testing.T) {
	s := newSite(t, map[string]resp{
		"/rss.xml":  {ctype: typeRSS, body: rssDoc("Main", 4)},
		"/atom.xml": {ctype: typeAtom, body: atomDoc},
		"/f.json":   {ctype: typeJSON, body: jsonDoc},
	})
	// The JSON link is absolute, so the page is registered once the address is known.
	s.paths["/"] = head(`<link rel="alternate" type="application/rss+xml" title="Main RSS" href="/rss.xml">` +
		`<link rel="alternate" type="application/atom+xml" href="atom.xml">` +
		`<link rel="ALTERNATE" type="application/feed+json; charset=utf-8" href="` + s.URL + `/f.json">` +
		`<link rel="stylesheet" href="/s.css"><link rel="alternate" type="text/html" href="/fr">`)

	got, err := feeds.Discover(context.Background(), s.Client(), s.URL+"/")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	want := []string{s.URL + "/rss.xml", s.URL + "/atom.xml", s.URL + "/f.json"}
	if !equalStrings(urls(got), want) {
		t.Fatalf("urls = %v, want %v", urls(got), want)
	}
	kinds := []feeds.Kind{feeds.KindRSS, feeds.KindAtom, feeds.KindJSON}
	for i, c := range got {
		if c.Kind != kinds[i] {
			t.Errorf("candidate %d kind = %q, want %q", i, c.Kind, kinds[i])
		}
	}
	// Feed title wins over the link title.
	if got[0].Title != "Main" || got[1].Title != "Atom Blog" || got[2].Title != "JSON Blog" {
		t.Errorf("titles = %q %q %q", got[0].Title, got[1].Title, got[2].Title)
	}
	if got[0].ItemCount != 4 || len(got[0].Latest) != 3 {
		t.Fatalf("rss count = %d latest = %d", got[0].ItemCount, len(got[0].Latest))
	}
	if got[0].Latest[0].Title != "Post 3" || !got[0].Latest[0].Published.After(got[0].Latest[1].Published) {
		t.Errorf("latest not newest first: %+v", got[0].Latest)
	}
	// Atom entries carry the updated date.
	if got[1].Latest[0].Title != "Two" || got[1].Latest[0].Published.IsZero() {
		t.Errorf("atom latest = %+v", got[1].Latest)
	}
	if s.count("/fr") != 0 {
		t.Error("a non-feed alternate link was fetched")
	}
}

func TestDiscoverTitleFallbacks(t *testing.T) {
	untitled := `<?xml version="1.0"?><rss version="2.0"><channel><title> </title></channel></rss>`
	s := newSite(t, map[string]resp{
		"/": head(`<link rel="alternate" type="application/rss+xml" title="  From   link " href="/a.xml">` +
			`<link rel="alternate" type="application/rss+xml" href="/b.xml">`),
		"/a.xml": {ctype: typeRSS, body: untitled},
		"/b.xml": {ctype: typeRSS, body: untitled},
	})
	got, err := feeds.Discover(context.Background(), s.Client(), s.URL)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got) != 2 || got[0].Title != "From link" || got[1].Title != strings.TrimPrefix(s.URL, "http://") {
		t.Fatalf("got %+v", got)
	}
	if got[0].ItemCount != 0 || len(got[0].Latest) != 0 {
		t.Errorf("empty feed reported %d items", got[0].ItemCount)
	}
}

func TestDiscoverBaseAppliesEvenAfterLink(t *testing.T) {
	s := newSite(t, map[string]resp{
		"/": head(`<link rel="alternate" type="application/rss+xml" href="feed.xml">` +
			`<base href="/blog/"><base href="/ignored/">`),
		"/blog/feed.xml": {ctype: typeRSS, body: rssDoc("Blog", 1)},
	})
	got, err := feeds.Discover(context.Background(), s.Client(), s.URL)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if want := []string{s.URL + "/blog/feed.xml"}; !equalStrings(urls(got), want) {
		t.Fatalf("urls = %v, want %v", urls(got), want)
	}
}

func TestDiscoverResolvesAgainstRedirectTarget(t *testing.T) {
	s := newSite(t, map[string]resp{
		"/old":              {redirect: "/new/dir/"},
		"/new/dir/":         head(`<link rel="alternate" type="application/rss+xml" href="feed.xml">`),
		"/new/dir/feed.xml": {ctype: typeRSS, body: rssDoc("Moved", 1)},
	})
	got, err := feeds.Discover(context.Background(), s.Client(), s.URL+"/old")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if want := []string{s.URL + "/new/dir/feed.xml"}; !equalStrings(urls(got), want) {
		t.Fatalf("urls = %v, want %v", urls(got), want)
	}
}

func TestDiscoverIgnoresHeadlessNoise(t *testing.T) {
	// Links after <body> and non-http schemes are not candidates.
	s := newSite(t, map[string]resp{
		"/": {ctype: typeHTML, body: `<html><head>` +
			`<link rel="alternate" type="application/rss+xml" href="ftp://example.com/f">` +
			`<link rel="alternate" type="application/rss+xml" href="javascript:alert(1)">` +
			`<link rel="alternate" type="application/rss+xml" href="">` +
			`</head><body><link rel="alternate" type="application/rss+xml" href="/late.xml"></body></html>`},
		"/late.xml": {ctype: typeRSS, body: rssDoc("Late", 1)},
	})
	_, err := feeds.Discover(context.Background(), s.Client(), s.URL)
	if !errors.Is(err, feeds.ErrNoFeed) {
		t.Fatalf("err = %v, want ErrNoFeed", err)
	}
	if s.count("/late.xml") != 0 {
		t.Error("a link after <body> was fetched")
	}
}

// The scan reads the whole capped page, so big inline blocks before the link do
// not stop it, and a link inside raw text is not one.
func TestDiscoverScansPastBigHeadBlocksAndRawText(t *testing.T) {
	link := `<link rel="alternate" type="application/rss+xml" href="/f.xml">`
	big := strings.Repeat("a{color:red}", 100<<10/12)
	ldjson := `<script type="application/ld+json">{"a":"` + big + `"}</script>`
	cases := []struct {
		name, links string
		want        bool
	}{
		{"100 KiB style before the link", `<style>` + big + `</style>` + link, true},
		{"100 KiB json-ld before the link", ldjson + link, true},
		{"link inside script", `<script>document.write('` + link + `')</script>`, false},
		{"link inside noscript", `<noscript>` + link + `</noscript>`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newSite(t, map[string]resp{
				"/":      head(tc.links),
				"/f.xml": {ctype: typeRSS, body: rssDoc("F", 1)},
			})
			got, err := feeds.Discover(context.Background(), s.Client(), s.URL)
			if tc.want {
				if err != nil || len(got) != 1 || got[0].URL != s.URL+"/f.xml" {
					t.Fatalf("got %+v, %v, want the declared feed", got, err)
				}
				return
			}
			if !errors.Is(err, feeds.ErrNoFeed) {
				t.Fatalf("err = %v, want ErrNoFeed", err)
			}
			if s.count("/f.xml") != 0 {
				t.Error("a link in raw text was fetched")
			}
		})
	}
}

func TestDiscoverDemotesCommentsAndDedupes(t *testing.T) {
	s := newSite(t, map[string]resp{
		"/": head(`<link rel="alternate" type="application/rss+xml" title="Comments on Blog" href="/c.xml">` +
			`<link rel="alternate" type="application/rss+xml" href="/comments/feed">` +
			`<link rel="alternate" type="application/rss+xml" href="/posts.xml">` +
			`<link rel="alternate" type="application/atom+xml" href="/posts.xml#frag">` +
			`<link rel="alternate" type="application/atom+xml" href="/posts.xml/">` +
			`<link rel="alternate" type="application/atom+xml" href="/other.xml">`),
		"/c.xml":         {ctype: typeRSS, body: rssDoc("C", 1)},
		"/comments/feed": {ctype: typeRSS, body: rssDoc("C2", 1)},
		"/posts.xml":     {ctype: typeRSS, body: rssDoc("Posts", 1)},
		"/other.xml":     {ctype: typeAtom, body: atomDoc},
	})
	got, err := feeds.Discover(context.Background(), s.Client(), s.URL)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	want := []string{s.URL + "/posts.xml", s.URL + "/other.xml", s.URL + "/c.xml", s.URL + "/comments/feed"}
	if !equalStrings(urls(got), want) {
		t.Fatalf("urls = %v, want %v", urls(got), want)
	}
	if n := s.count("/posts.xml"); n != 1 {
		t.Errorf("posts.xml fetched %d times", n)
	}
}

func TestDiscoverCapsCandidates(t *testing.T) {
	paths := map[string]resp{}
	var links strings.Builder
	for i := range 9 {
		p := fmt.Sprintf("/f%d.xml", i)
		paths[p] = resp{ctype: typeRSS, body: rssDoc(fmt.Sprintf("F%d", i), 1)}
		fmt.Fprintf(&links, `<link rel="alternate" type="application/rss+xml" href="%s">`, p)
	}
	paths["/"] = head(links.String())
	s := newSite(t, paths)

	got, err := feeds.Discover(context.Background(), s.Client(), s.URL)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got) != feeds.MaxCandidates {
		t.Fatalf("got %d candidates, want %d", len(got), feeds.MaxCandidates)
	}
	for i, c := range got {
		if want := fmt.Sprintf("%s/f%d.xml", s.URL, i); c.URL != want {
			t.Errorf("candidate %d = %s, want %s", i, c.URL, want)
		}
	}
	if n := s.total(); n > 1+feeds.MaxCandidates {
		t.Errorf("%d requests, want at most %d", n, 1+feeds.MaxCandidates)
	}
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

func TestDiscoverAddressIsAlreadyAFeed(t *testing.T) {
	cases := []struct {
		name  string
		ctype string
		body  string
		kind  feeds.Kind
	}{
		{"rss", typeRSS, rssDoc("Direct", 5), feeds.KindRSS},
		{"atom", typeAtom, atomDoc, feeds.KindAtom},
		{"json", typeJSON, jsonDoc, feeds.KindJSON},
		{"generic xml type", "text/xml", rssDoc("Direct", 1), feeds.KindRSS},
		{"mislabelled html type", typeHTML, "\n" + rssDoc("Direct", 1), feeds.KindRSS},
		{"no type", "", rssDoc("Direct", 1), feeds.KindRSS},
		{"3 MiB", typeRSS, bigRSS(3 << 20), feeds.KindRSS},
		{"15 MiB", typeRSS, bigRSS(15 << 20), feeds.KindRSS},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newSite(t, map[string]resp{"/feed": {ctype: tc.ctype, body: tc.body}})
			got, err := feeds.Discover(context.Background(), s.Client(), s.URL+"/feed")
			if err != nil {
				t.Fatalf("Discover: %v", err)
			}
			if len(got) != 1 || got[0].URL != s.URL+"/feed" || got[0].Kind != tc.kind {
				t.Fatalf("got %+v", got)
			}
			if n := s.total(); n != 1 {
				t.Errorf("%d requests, want exactly 1", n)
			}
			if got[0].Title == "" || got[0].ItemCount == 0 {
				t.Errorf("candidate lacks title or items: %+v", got[0])
			}
		})
	}
}

func TestFeedAddress(t *testing.T) {
	parse := func(raw string) *url.URL {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	cases := []struct {
		name, typed, final, want string
	}{
		{"same", "https://a.test/feed", "https://a.test/feed", "https://a.test/feed"},
		{"typed keeps its form", "https://A.test/feed/", "https://a.test/feed", "https://A.test/feed/"},
		{"scheme upgrade", "http://a.test/feed", "https://a.test/feed", "https://a.test/feed"},
		{"upgrade with query", "http://a.test/f?x=1", "https://a.test/f?x=1", "https://a.test/f?x=1"},
		{"upgrade and another path", "http://a.test/feed", "https://a.test/rss", "http://a.test/feed"},
		{"upgrade and another host", "http://a.test/feed", "https://www.a.test/feed", "http://a.test/feed"},
		{"redirect elsewhere", "http://a.test/feed", "http://b.test/feed", "http://a.test/feed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := feeds.FeedAddress(tc.typed, parse(tc.final)); got != tc.want {
				t.Errorf("FeedAddress = %q, want %q", got, tc.want)
			}
		})
	}
}

// A feed the site redirects keeps the address that was typed.
func TestDiscoverFeedKeepsTypedAddressAcrossRedirect(t *testing.T) {
	s := newSite(t, map[string]resp{
		"/old": {redirect: "/new"},
		"/new": {ctype: typeRSS, body: rssDoc("Moved", 1)},
	})
	got, err := feeds.Discover(context.Background(), s.Client(), s.URL+"/old")
	if err != nil || len(got) != 1 || got[0].URL != s.URL+"/old" {
		t.Fatalf("got %+v, %v, want the typed address", got, err)
	}
}

// A page that starts like XML and is over the feed cap is still searched as a page.
func TestDiscoverOversizeHTMLThatSniffsAsAFeed(t *testing.T) {
	body := `<rss version="2.0"><head><link rel="alternate" type="application/rss+xml" href="/f.xml"/></head><body>` +
		strings.Repeat("<p>filler</p>", (feeds.MaxFeedBytes+1<<20)/13)
	s := newSite(t, map[string]resp{
		"/":      {ctype: typeHTML, body: body},
		"/f.xml": {ctype: typeRSS, body: rssDoc("F", 1)},
	})
	got, err := feeds.Discover(context.Background(), s.Client(), s.URL)
	if err != nil || len(got) != 1 || got[0].URL != s.URL+"/f.xml" {
		t.Fatalf("got %+v, %v, want the declared feed", got, err)
	}
}

func TestDiscoverFeedOverCapFails(t *testing.T) {
	s := newSite(t, map[string]resp{"/feed": {ctype: typeRSS, body: bigRSS(feeds.MaxFeedBytes + 1<<20)}})
	_, err := feeds.Discover(context.Background(), s.Client(), s.URL+"/feed")
	if !errors.Is(err, httpclient.ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}

func TestDiscoverFallbackPaths(t *testing.T) {
	s := newSite(t, map[string]resp{
		"/":         head(""),
		"/atom.xml": {ctype: typeAtom, body: atomDoc},
		"/a/b":      head(""),
	})
	// From a deep page the fallbacks are tried at the site root.
	got, err := feeds.Discover(context.Background(), s.Client(), s.URL+"/a/b")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if want := []string{s.URL + "/atom.xml"}; !equalStrings(urls(got), want) {
		t.Fatalf("urls = %v, want %v", urls(got), want)
	}
	if got[0].Title != "Atom Blog" {
		t.Errorf("title = %q", got[0].Title)
	}
}

func TestDiscoverFallbacksSkippedWhenDeclared(t *testing.T) {
	s := newSite(t, map[string]resp{
		"/":             head(`<link rel="alternate" type="application/rss+xml" href="/declared.xml">`),
		"/feed":         {ctype: typeRSS, body: rssDoc("Fallback", 1)},
		"/declared.xml": {ctype: typeRSS, body: rssDoc("Declared", 1)},
	})
	got, err := feeds.Discover(context.Background(), s.Client(), s.URL)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got) != 1 || got[0].Title != "Declared" {
		t.Fatalf("got %+v", got)
	}
	if s.count("/feed") != 0 {
		t.Error("a fallback path was tried although the page declares a feed")
	}
}

func TestDiscoverNoFeedAnywhere(t *testing.T) {
	t.Run("404s", func(t *testing.T) {
		s := newSite(t, map[string]resp{"/": head("")})
		_, err := feeds.Discover(context.Background(), s.Client(), s.URL)
		if !errors.Is(err, feeds.ErrNoFeed) {
			t.Fatalf("err = %v, want ErrNoFeed", err)
		}
	})
	t.Run("every path serves html", func(t *testing.T) {
		page := head("")
		paths := map[string]resp{"/": page}
		for _, p := range []string{"/feed", "/rss", "/rss.xml", "/atom.xml", "/index.xml"} {
			paths[p] = page
		}
		s := newSite(t, paths)
		_, err := feeds.Discover(context.Background(), s.Client(), s.URL)
		if !errors.Is(err, feeds.ErrNoFeed) {
			t.Fatalf("err = %v, want ErrNoFeed", err)
		}
	})
}

func TestDiscoverSkipsBadCandidates(t *testing.T) {
	s := newSite(t, map[string]resp{
		"/": head(`<link rel="alternate" type="application/rss+xml" href="/err.xml">` +
			`<link rel="alternate" type="application/rss+xml" href="/html.xml">` +
			`<link rel="alternate" type="application/rss+xml" href="/broken.xml">` +
			`<link rel="alternate" type="application/rss+xml" href="/missing.xml">` +
			`<link rel="alternate" type="application/rss+xml" href="/good.xml">`),
		"/err.xml":    {ctype: typeRSS, status: 500, body: "boom"},
		"/html.xml":   head(""),
		"/broken.xml": {ctype: typeRSS, body: `<rss version="2.0"><channel><title>x`},
		"/good.xml":   {ctype: typeRSS, body: rssDoc("Good", 2)},
	})
	got, err := feeds.Discover(context.Background(), s.Client(), s.URL)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if want := []string{s.URL + "/good.xml"}; !equalStrings(urls(got), want) {
		t.Fatalf("urls = %v, want %v", urls(got), want)
	}
}

func TestDiscoverOversizeHTMLIsTruncatedNotFailed(t *testing.T) {
	body := `<html><head><link rel="alternate" type="application/rss+xml" href="/f.xml"></head><body>` +
		strings.Repeat("<p>filler</p>", (feeds.MaxPageBytes+1<<20)/13) + `</body></html>`
	if len(body) <= feeds.MaxPageBytes {
		t.Fatal("test page is not oversize")
	}
	s := newSite(t, map[string]resp{
		"/":      {ctype: typeHTML, body: body},
		"/f.xml": {ctype: typeRSS, body: rssDoc("F", 1)},
	})
	got, err := feeds.Discover(context.Background(), s.Client(), s.URL)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
}

func TestDiscoverNotAPage(t *testing.T) {
	for _, ctype := range []string{"image/png", "application/pdf", "application/octet-stream", "application/zip"} {
		t.Run(ctype, func(t *testing.T) {
			s := newSite(t, map[string]resp{"/x": {ctype: ctype, body: "\x89PNG binary"}})
			_, err := feeds.Discover(context.Background(), s.Client(), s.URL+"/x")
			if !errors.Is(err, feeds.ErrNotPage) {
				t.Fatalf("err = %v, want ErrNotPage", err)
			}
			if n := s.total(); n != 1 {
				t.Errorf("%d requests, want 1", n)
			}
		})
	}
}

func TestDiscoverPageHTTPError(t *testing.T) {
	s := newSite(t, map[string]resp{})
	_, err := feeds.Discover(context.Background(), s.Client(), s.URL+"/nope")
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("err = %v, want a 404 error", err)
	}
	if errors.Is(err, feeds.ErrNoFeed) || errors.Is(err, feeds.ErrNotPage) {
		t.Errorf("a failed page must not read as %v", err)
	}
}

func TestDiscoverOverallDeadline(t *testing.T) {
	s := newSite(t, map[string]resp{
		"/":         head(`<link rel="alternate" type="application/rss+xml" href="/slow.xml">`),
		"/slow.xml": {ctype: typeRSS, body: rssDoc("Slow", 1), delay: 5 * time.Second},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := feeds.Discover(ctx, s.Client(), s.URL)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("took %v, the deadline did not cut the fetch", time.Since(start))
	}
}

func TestDiscoverGuardedClientBlocksPrivateAddress(t *testing.T) {
	t.Setenv(httpclient.AllowPrivateEnv, "")
	s := newSite(t, map[string]resp{"/feed": {ctype: typeRSS, body: rssDoc("Local", 1)}})
	_, err := feeds.Discover(context.Background(), feeds.NewClient(""), s.URL+"/feed")
	if !errors.Is(err, httpclient.ErrBlockedAddress) {
		t.Fatalf("err = %v, want ErrBlockedAddress", err)
	}
	if n := s.total(); n != 0 {
		t.Errorf("the blocked address received %d requests", n)
	}
	_, err = feeds.Preview(context.Background(), feeds.NewClient(""), s.URL+"/feed")
	if !errors.Is(err, httpclient.ErrBlockedAddress) {
		t.Fatalf("Preview err = %v, want ErrBlockedAddress", err)
	}
}

// refuseHost fails requests to one host the way the guarded client refuses a
// private address, and lets everything else through.
type refuseHost struct {
	host string
	next http.RoundTripper
}

func (r refuseHost) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Hostname() == r.host {
		return nil, fmt.Errorf("%w: %s", httpclient.ErrBlockedAddress, req.URL.Host)
	}
	return r.next.RoundTrip(req)
}

func TestDiscoverOneBlockedCandidateDoesNotFailTheSearch(t *testing.T) {
	s := newSite(t, map[string]resp{
		"/": head(`<link rel="alternate" type="application/rss+xml" href="http://internal.test/feed.xml">` +
			`<link rel="alternate" type="application/rss+xml" href="/good.xml">`),
		"/good.xml": {ctype: typeRSS, body: rssDoc("Good", 1)},
	})
	hc := s.Client()
	client := &http.Client{Transport: refuseHost{host: "internal.test", next: hc.Transport}}

	got, err := feeds.Discover(context.Background(), client, s.URL)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if want := []string{s.URL + "/good.xml"}; !equalStrings(urls(got), want) {
		t.Fatalf("urls = %v, want %v", urls(got), want)
	}

	// With only the blocked candidate there is nothing to offer.
	only := newSite(t, map[string]resp{
		"/": head(`<link rel="alternate" type="application/rss+xml" href="http://internal.test/feed.xml">`),
	})
	client = &http.Client{Transport: refuseHost{host: "internal.test", next: only.Client().Transport}}
	if _, err := feeds.Discover(context.Background(), client, only.URL); !errors.Is(err, feeds.ErrNoFeed) {
		t.Fatalf("err = %v, want ErrNoFeed", err)
	}
}

func TestPreview(t *testing.T) {
	s := newSite(t, map[string]resp{
		"/feed":  {ctype: typeRSS, body: rssDoc("  Spaced \n Title ", 6)},
		"/empty": {ctype: typeRSS, body: rssDoc("Empty", 0)},
		"/json":  {ctype: typeJSON, body: jsonDoc},
		"/big":   {ctype: typeRSS, body: bigRSS(feeds.MaxFeedBytes + 1<<20)},
		"/page":  head(""),
		"/err":   {ctype: typeRSS, status: 500, body: "boom"},
	})
	ctx := context.Background()

	got, err := feeds.Preview(ctx, s.Client(), s.URL+"/feed")
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if got.Title != "Spaced Title" || got.Count != 6 || len(got.Items) != 3 || got.Items[0].Title != "Post 5" {
		t.Errorf("got %+v", got)
	}

	got, err = feeds.Preview(ctx, s.Client(), s.URL+"/empty")
	if err != nil || got.Count != 0 || len(got.Items) != 0 {
		t.Errorf("empty feed: %+v, %v", got, err)
	}

	got, err = feeds.Preview(ctx, s.Client(), s.URL+"/json")
	if err != nil || got.Title != "JSON Blog" || got.Count != 1 {
		t.Errorf("json feed: %+v, %v", got, err)
	}

	if _, err = feeds.Preview(ctx, s.Client(), s.URL+"/big"); !errors.Is(err, httpclient.ErrTooLarge) {
		t.Errorf("oversize: err = %v, want ErrTooLarge", err)
	}
	if _, err = feeds.Preview(ctx, s.Client(), s.URL+"/page"); err == nil {
		t.Error("a web page previewed as a feed")
	}
	if _, err = feeds.Preview(ctx, s.Client(), s.URL+"/missing"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("missing: err = %v, want a 404 error", err)
	}
	if _, err = feeds.Preview(ctx, s.Client(), s.URL+"/err"); err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("server error: err = %v, want a 500 error", err)
	}

	slow := newSite(t, map[string]resp{"/feed": {ctype: typeRSS, body: rssDoc("S", 1), delay: 5 * time.Second}})
	dctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	if _, err = feeds.Preview(dctx, slow.Client(), slow.URL+"/feed"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("deadline: err = %v, want DeadlineExceeded", err)
	}
}

// TestDedupeKeyMatchesConfig keeps feeds' private key in step with the key the
// store uses to recognise a subscribed feed.
func TestDedupeKeyMatchesConfig(t *testing.T) {
	for _, raw := range []string{
		"https://Example.COM/Feed.xml",
		"HTTP://example.com/feed/",
		"https://example.com/feed///",
		"https://example.com/feed#frag",
		"https://example.com/Feed?Q=1&b=2",
		"https://example.com/a%2Fb/",
		"https://example.com:8080/feed/",
		"https://user:pw@example.com/feed",
		"  https://example.com/feed  ",
		"example.com/feed",
		"//example.com/feed/",
		"/relative/Path/",
		"://bad url",
		"",
		"https://",
		"mailto:Someone@Example.com",
	} {
		if got, want := feeds.DedupeKey(raw), config.FeedURLKey(raw); got != want {
			t.Errorf("DedupeKey(%q) = %q, config.FeedURLKey = %q", raw, got, want)
		}
	}
}
