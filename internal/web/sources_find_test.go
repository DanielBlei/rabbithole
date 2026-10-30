// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	nethtml "golang.org/x/net/html"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/httpclient"
)

const findFeedXML = `<?xml version="1.0"?><rss version="2.0"><channel><title>%s</title>` +
	`<item><title>First post</title><link>https://example.com/1</link><pubDate>Mon, 01 Jan 2024 10:00:00 GMT</pubDate></item>` +
	`<item><title>Second post</title><link>https://example.com/2</link><pubDate>Tue, 02 Jan 2024 10:00:00 GMT</pubDate></item>` +
	`</channel></rss>`

const findPage = `<html><head><link rel="alternate" type="application/rss+xml" href="/feed.xml"></head></html>`

// findSite serves a site whose home page declares /feed.xml, and wires the
// web handler to reach it. The feed's title is the argument.
func findSite(t *testing.T, w *Web, title string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/{$}", func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(rw, findPage)
	})
	mux.HandleFunc("/feed.xml", func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "application/rss+xml")
		_, _ = fmt.Fprintf(rw, findFeedXML, title)
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	w.SetFeedClient(srv.Client())
	return srv
}

func discover(t *testing.T, w *Web, addr string) string {
	t.Helper()
	return postForm(t, w, "/sources/discover", url.Values{"url": {addr}})
}

func TestSourceDiscoverListsANewFeed(t *testing.T) {
	w, _ := sourcesTestWeb(t, config.FeedsDoc{})
	srv := findSite(t, w, "Julia's Blog")
	out := discover(t, w, srv.URL)

	if !strings.Contains(out, "Found a feed.") {
		t.Errorf("no heading; body=%s", out)
	}
	wantAttrs(t, "radiogroup", tagWith(t, out, "role", "radiogroup"), map[string]string{"aria-label": "Feeds found"})
	radio := tagWith(t, out, "id", "ed-cand-0")
	wantAttrs(t, "radio", radio, map[string]string{
		"type": "radio", "name": "cand", "data-name": "Julia's Blog",
		"data-url": srv.URL + "/feed.xml", "data-type": "rss",
	})
	lacksAttr(t, "radio", radio, "disabled")
	wantAttrs(t, "row", tagWith(t, out, "class", "mg-cand"), map[string]string{"data-state": "new"})
	for _, want := range []string{">RSS<", "2 posts", "First post", "Second post", "Recent posts", `data-src-cand-prev=""`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q; body=%s", want, out)
		}
	}
	// The preview block waits hidden until the row is picked.
	wantAttrs(t, "preview block", tagWith(t, out, "data-src-cand-prev", ""), map[string]string{"hidden": present})
}

func TestSourceDiscoverMarksStoredFeeds(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{})
	srv := findSite(t, w, "Julia's Blog")
	feedURL := srv.URL + "/feed.xml"

	t.Run("subscribed", func(t *testing.T) {
		id, err := db.AddFeed(t.Context(), config.Feed{Name: "Mine", URL: feedURL})
		if err != nil {
			t.Fatalf("AddFeed: %v", err)
		}
		out := discover(t, w, srv.URL)
		wantAttrs(t, "row", tagWith(t, out, "class", "mg-cand"), map[string]string{"data-state": "subscribed"})
		wantAttrs(t, "radio", tagWith(t, out, "id", "ed-cand-0"), map[string]string{"disabled": present})
		wantAttrs(t, "show it", tagWith(t, out, "data-src-reveal", id), map[string]string{"type": "button"})
		if !strings.Contains(out, "Already in your sources") || !strings.Contains(out, "Found this feed.") {
			t.Errorf("no subscribed note; body=%s", out)
		}
		deleteSource(t, w, id)
	})

	t.Run("removed", func(t *testing.T) {
		out := discover(t, w, srv.URL)
		id := feedID(t, db, "Mine")
		wantAttrs(t, "row", tagWith(t, out, "class", "mg-cand"), map[string]string{"data-state": "removed"})
		wantAttrs(t, "radio", tagWith(t, out, "id", "ed-cand-0"), map[string]string{"disabled": present})
		wantAttrs(t, "restore", tagWith(t, out, "data-src-restore", id), map[string]string{
			"hx-post": "/sources/feed/" + id + "/restore", "hx-target": "#src-" + id, "hx-swap": "outerHTML",
			"hx-params": "none",
		})
		if !strings.Contains(out, "Removed earlier") {
			t.Errorf("no removed note; body=%s", out)
		}
	})
}

// A feed whose title is another source's name can still be picked, with a note
// to rename it.
func TestSourceDiscoverNameConflictStaysSelectable(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{})
	srv := findSite(t, w, "Taken")
	if _, err := db.AddFeed(t.Context(), config.Feed{Name: "Taken", URL: "https://elsewhere.test/feed"}); err != nil {
		t.Fatalf("AddFeed: %v", err)
	}
	out := discover(t, w, srv.URL)

	wantAttrs(t, "row", tagWith(t, out, "class", "mg-cand"), map[string]string{"data-state": "conflict"})
	lacksAttr(t, "radio", tagWith(t, out, "id", "ed-cand-0"), "disabled")
	want := `A source named "Taken" exists with another address. Pick it and rename it in step 2.`
	if !strings.Contains(out, want) {
		t.Errorf("missing %q; body=%s", want, out)
	}
}

// A feed whose address a source gave up by editing it cannot be added at all.
func TestSourceDiscoverIDConflictIsBlocked(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{})
	srv := findSite(t, w, "Moved")
	id, err := db.AddFeed(t.Context(), config.Feed{Name: "Old", URL: srv.URL + "/feed.xml"})
	if err != nil {
		t.Fatalf("AddFeed: %v", err)
	}
	if _, err := db.UpdateFeed(t.Context(), id, config.Feed{Name: "Old", URL: "https://moved.test/feed"}); err != nil {
		t.Fatalf("UpdateFeed: %v", err)
	}
	out := discover(t, w, srv.URL)

	wantAttrs(t, "row", tagWith(t, out, "class", "mg-cand"), map[string]string{"data-state": "blocked"})
	wantAttrs(t, "radio", tagWith(t, out, "id", "ed-cand-0"), map[string]string{"disabled": present})
}

// The id stays taken whatever the name does: a rename would not let the feed in.
func TestSourceDiscoverIDConflictWithTakenNameIsBlocked(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{})
	srv := findSite(t, w, "Moved")
	id, err := db.AddFeed(t.Context(), config.Feed{Name: "Moved", URL: srv.URL + "/feed.xml"})
	if err != nil {
		t.Fatalf("AddFeed: %v", err)
	}
	moved := config.Feed{Name: "Moved", URL: "https://moved.test/feed"}
	if _, err := db.UpdateFeed(t.Context(), id, moved); err != nil {
		t.Fatalf("UpdateFeed: %v", err)
	}
	out := discover(t, w, srv.URL)

	wantAttrs(t, "row", tagWith(t, out, "class", "mg-cand"), map[string]string{"data-state": "blocked"})
	wantAttrs(t, "radio", tagWith(t, out, "id", "ed-cand-0"), map[string]string{"disabled": present})
	if strings.Contains(out, "rename it") {
		t.Errorf("a blocked feed is offered a rename; body=%s", out)
	}
}

// A failure of the store is a fragment where the results go, not a bare 500.
func TestSourceDiscoverStoreFailureIsAFragment(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{})
	srv := findSite(t, w, "Blog")
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	out := discover(t, w, srv.URL)
	wantAttrs(t, "error", tagWith(t, out, "role", "alert"), map[string]string{
		"class": "sdet__err mg-find__err", "tabindex": "-1",
	})
	if !strings.Contains(out, findCheckText) || strings.Contains(out, "data-src-manual") {
		t.Errorf("want only %q; body=%s", findCheckText, out)
	}
}

func TestSourceDiscoverErrorsAreFragments(t *testing.T) {
	w, _ := sourcesTestWeb(t, config.FeedsDoc{})
	empty := http.NewServeMux()
	empty.HandleFunc("/", func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			rw.Header().Set("Content-Type", "text/html")
			_, _ = fmt.Fprint(rw, `<html><head></head><body>hi</body></html>`)
			return
		}
		http.NotFound(rw, r)
	})
	srv := httptest.NewTLSServer(empty)
	defer srv.Close()
	w.SetFeedClient(srv.Client())

	cases := []struct {
		name, addr, want string
		manual           bool
	}{
		{"empty", "  ", findEmptyText, false},
		{"scheme", "ftp://example.test/feed", schemeText, false},
		{"no host", "http://", findBadAddrText, false},
		{"unparsable", "http://exa mple.test", findBadAddrText, false},
		{"no feed", srv.URL, findNoFeedText, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := discover(t, w, c.addr)
			wantAttrs(
				t,
				"error",
				tagWith(t, out, "role", "alert"),
				map[string]string{"class": "sdet__err mg-find__err"},
			)
			if !strings.Contains(out, c.want) {
				t.Errorf("missing %q; body=%s", c.want, out)
			}
			if got := strings.Contains(out, "data-src-manual"); got != c.manual {
				t.Errorf("manual link = %v, want %v; body=%s", got, c.manual, out)
			}
			if strings.Contains(out, `name="cand"`) {
				t.Errorf("an error carries candidates; body=%s", out)
			}
		})
	}
}

// The default client is the ingestion one, which refuses private addresses.
func TestSourceDiscoverUsesTheGuardedClient(t *testing.T) {
	t.Setenv(httpclient.AllowPrivateEnv, "")
	w, _ := sourcesTestWeb(t, config.FeedsDoc{})
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	out := discover(t, w, srv.URL)
	if !strings.Contains(out, "private network") {
		t.Errorf("a loopback address was not refused as private; body=%s", out)
	}
}

// Whatever a remote site names its feed, the answer prints it as text and in
// data attributes: nothing becomes markup, a link or a request value.
func TestSourceDiscoverEscapesRemoteText(t *testing.T) {
	w, _ := sourcesTestWeb(t, config.FeedsDoc{})
	evil := `"><script>alert(1)</script><img src=x onerror=alert(2)>`
	srv := findSite(t, w, html.EscapeString(evil))
	out := discover(t, w, srv.URL)

	for _, bad := range []string{"<script", "<img", `"><`, "href=\"" + srv.URL, "hx-vals", "javascript:"} {
		if strings.Contains(out, bad) {
			t.Errorf("found %q in the answer; body=%s", bad, out)
		}
	}
	radio := tagWith(t, out, "id", "ed-cand-0")
	if got := radio["data-name"]; got != evil {
		t.Errorf("data-name = %q, want the title kept as text", got)
	}
	if !strings.Contains(out, "&lt;script&gt;") {
		t.Errorf("the title is not shown escaped; body=%s", out)
	}
}

func TestSourceFindFormLimits(t *testing.T) {
	w, _ := sourcesTestWeb(t, config.FeedsDoc{})
	for _, path := range []string{"/sources/discover", "/sources/preview"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("url=%zz"))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			w.Routes().ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("malformed form = %d, want 400", rec.Code)
			}

			rec = httptest.NewRecorder()
			req = httptest.NewRequest(
				http.MethodPost,
				path,
				strings.NewReader("url="+strings.Repeat("a", findFormLimit+1)),
			)
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			w.Routes().ServeHTTP(rec, req)
			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Errorf("oversize form = %d, want 413", rec.Code)
			}
		})
	}
}

func TestSourcePreviewShowsRecentPosts(t *testing.T) {
	w, _ := sourcesTestWeb(t, config.FeedsDoc{})
	srv := findSite(t, w, "Blog")

	out := postForm(t, w, "/sources/preview", url.Values{"url": {srv.URL + "/feed.xml"}})
	for _, want := range []string{"Recent posts", "First post", "Second post", "Preview only. Nothing is saved until you add the source."} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q; body=%s", want, out)
		}
	}
	if strings.Contains(out, "sdet__err") {
		t.Errorf("a good feed shows an error; body=%s", out)
	}

	// A page is not a feed.
	out = postForm(t, w, "/sources/preview", url.Values{"url": {srv.URL}})
	wantAttrs(t, "error", tagWith(t, out, "role", "alert"), map[string]string{"class": "sdet__err"})
	if !strings.Contains(out, findWebPageText) {
		t.Errorf("a page was not explained as one; body=%s", out)
	}
	if strings.Contains(out, "Recent posts") {
		t.Errorf("an error sits under the posts heading; body=%s", out)
	}
	if out = postForm(t, w, "/sources/preview", url.Values{"url": {""}}); !strings.Contains(out, findEmptyText) {
		t.Errorf("an empty address was not refused; body=%s", out)
	}
	out = postForm(t, w, "/sources/preview", url.Values{"url": {"ftp://x.test/f"}})
	if !strings.Contains(out, schemeText) {
		t.Errorf("a scheme was not explained; body=%s", out)
	}
}

func TestSourcePreviewOfAnEmptyFeed(t *testing.T) {
	w, _ := sourcesTestWeb(t, config.FeedsDoc{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "application/rss+xml")
		_, _ = fmt.Fprint(rw, `<?xml version="1.0"?><rss version="2.0"><channel><title>Empty</title></channel></rss>`)
	}))
	defer srv.Close()
	w.SetFeedClient(srv.Client())

	out := postForm(t, w, "/sources/preview", url.Values{"url": {srv.URL}})
	if !strings.Contains(out, "This feed has no posts yet. You can still add it.") {
		t.Errorf("no empty-feed line; body=%s", out)
	}
}

func TestSourceCheckFindsStoredFeeds(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{
		{Name: "Alpha", URL: "https://alpha.test/feed"},
		{Name: "Beta", URL: "https://beta.test/feed"},
	}})
	alpha, beta := feedID(t, db, "Alpha"), feedID(t, db, "Beta")
	deleteSource(t, w, beta)

	live := get(t, w, "/sources/check?url="+url.QueryEscape("ALPHA.test/feed/"))
	if !strings.Contains(live, "Already in your sources as Alpha.") {
		t.Errorf("a live match is not reported; body=%s", live)
	}
	wantAttrs(t, "show it", tagWith(t, live, "data-src-reveal", alpha), map[string]string{"type": "button"})

	gone := get(t, w, "/sources/check?url="+url.QueryEscape("https://beta.test/feed"))
	if !strings.Contains(gone, "You removed Beta earlier.") {
		t.Errorf("a removed match is not reported; body=%s", gone)
	}
	wantAttrs(t, "restore", tagWith(t, gone, "data-src-restore", beta), map[string]string{
		"hx-post": "/sources/feed/" + beta + "/restore", "hx-target": "#src-" + beta,
	})

	for _, q := range []string{"", "?url=", "?url=" + url.QueryEscape("https://new.test/feed"), "?url=" + url.QueryEscape("%%")} {
		if out := get(t, w, "/sources/check"+q); strings.TrimSpace(out) != "" {
			t.Errorf("check%s answered %q, want an empty body", q, out)
		}
	}
}

// ownedNames is what the form with this id would post: the names of its
// controls under the HTML form-owner rules, a form attribute taking priority
// over the nearest form ancestor.
func ownedNames(t *testing.T, body, formID string) []string {
	t.Helper()
	doc, err := nethtml.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var out []string
	var walk func(n *nethtml.Node, inside bool)
	attr := func(n *nethtml.Node, key string) (string, bool) {
		for _, a := range n.Attr {
			if a.Key == key {
				return a.Val, true
			}
		}
		return "", false
	}
	walk = func(n *nethtml.Node, inside bool) {
		if n.Type == nethtml.ElementNode {
			if id, _ := attr(n, "id"); n.Data == "form" && id == formID {
				inside = true
			}
			switch n.Data {
			case "input", "select", "textarea", "button":
				owner, has := attr(n, "form")
				name, _ := attr(n, "name")
				owned := inside
				if has {
					owned = owner == formID
				}
				if owned && name != "" {
					out = append(out, name)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, inside)
		}
	}
	walk(doc, false)
	return out
}

// The candidate radios sit inside the add form, yet the form posts none of
// them, and neither the find input: the radios name a form that does not
// exist, and the input has no name.
func TestSourceAddFormPostsNeitherFindInputNorCandidates(t *testing.T) {
	w, _ := sourcesTestWeb(t, config.FeedsDoc{})
	srv := findSite(t, w, "Blog")
	cands := discover(t, w, srv.URL)
	form := get(t, w, "/sources/new")
	page := strings.Replace(form, `data-src-cands=""></div>`, `data-src-cands="">`+cands+`</div>`, 1)
	if !strings.Contains(page, `name="cand"`) {
		t.Fatal("the candidates were not placed in the form")
	}

	names := ownedNames(t, page, "ed-new")
	if len(names) == 0 {
		t.Fatal("the add form owns no controls")
	}
	for _, n := range names {
		if n == "cand" {
			t.Errorf("the add form posts the candidate radios: %v", names)
		}
	}
	for _, want := range []string{"name", "url", "type", "tags", "enabled", "since", "max_items"} {
		found := false
		for _, n := range names {
			found = found || n == want
		}
		if !found {
			t.Errorf("the add form no longer posts %q: %v", want, names)
		}
	}

	radio := tagWith(t, cands, "id", "ed-cand-0")
	if id := radio["form"]; id == "" || strings.Contains(page, `id="`+id+`"`) {
		t.Errorf("the radio's form attribute %q is empty or names an element", id)
	}
	find := tagWith(t, form, "id", "ed-find-new")
	lacksAttr(t, "find input", find, "name")
}

func TestSourceAddFormStartsAtTheFindStep(t *testing.T) {
	w, _ := sourcesTestWeb(t, config.FeedsDoc{})
	out := get(t, w, "/sources/new")

	for _, want := range []string{"Add a source</h2>", "Paste a site or feed address", "A site address like jvns.ca is enough: we look for its feed.",
		"Check the details", "Advanced: since and max items"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q; body=%s", want, out)
		}
	}
	wantAttrs(t, "library link", tagWith(t, out, "href", "/sources/library"), map[string]string{"class": "mg-liblink"})
	wantAttrs(t, "form", tagWith(t, out, "id", "ed-new"), map[string]string{"data-find-failed": findErrorFallback})
	wantAttrs(t, "find input", tagWith(t, out, "id", "ed-find-new"), map[string]string{
		"placeholder": "jvns.ca or https://example.com/feed.xml", "data-src-find": present,
	})
	wantAttrs(t, "find", tagWith(t, out, "data-src-find-go", ""), map[string]string{
		"type": "button", "hx-post": "/sources/discover", "hx-params": "none", "hx-target": "#ed-cands-new",
		"data-src-rss-only": present,
	})
	wantAttrs(t, "preview", tagWith(t, out, "data-src-preview", ""), map[string]string{
		"type": "button", "hx-post": "/sources/preview", "hx-params": "none", "hx-target": "#ed-prev-new",
		"data-src-rss-only": present,
	})
	wantAttrs(t, "address", tagWith(t, out, "id", "ed-url-new"), map[string]string{
		"name": "url", "placeholder": "https://example.com/feed.xml", "hx-get": "/sources/check",
		"hx-trigger": "input changed delay:400ms, change", "hx-target": "#ed-dup-new",
	})
	wantAttrs(t, "name", tagWith(t, out, "id", "ed-name-new"), map[string]string{"placeholder": "e.g. Julia Evans"})
	wantAttrs(t, "step 2", tagWith(t, out, "data-src-step2", ""), map[string]string{"hidden": present})
	wantAttrs(t, "dup note", tagWith(t, out, "id", "ed-dup-new"), map[string]string{"aria-live": "polite"})
	wantAttrs(t, "manual", tagWith(t, out, "data-src-manual", ""), map[string]string{"type": "button"})

	// Since and max items fold away on the add form, closed.
	details := out[strings.Index(out, `<details class="mg-adv"`):]
	details = details[:strings.Index(details, "</details>")]
	for _, want := range []string{`name="since"`, `name="max_items"`} {
		if !strings.Contains(details, want) {
			t.Errorf("Advanced lacks %s; details=%s", want, details)
		}
	}
	if strings.HasPrefix(details, `<details class="mg-adv" open`) {
		t.Errorf("Advanced starts open; details=%s", details)
	}
}

// A rejected add comes back at step 2 with what was typed, and opens Advanced
// only when the rejection is about since or max items.
func TestSourceAddRejectionRevealsDetailsAndOpensAdvanced(t *testing.T) {
	w, _ := sourcesTestWeb(t, config.FeedsDoc{
		Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}},
	})
	cases := []struct {
		name string
		form url.Values
		open bool
	}{
		{"bad since", url.Values{"name": {"New"}, "url": {"https://new.test/feed"}, "since": {"soon"}}, true},
		{"negative since", url.Values{"name": {"New"}, "url": {"https://new.test/feed"}, "since": {"-3d"}}, true},
		{"bad max", url.Values{"name": {"New"}, "url": {"https://new.test/feed"}, "max_items": {"lots"}}, true},
		{
			"name taken",
			url.Values{"name": {"Alpha"}, "url": {"https://new.test/feed"}, "since": {"3d"}, "max_items": {"5"}},
			false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := postForm(t, w, "/sources", c.form)
			wantAttrs(t, "step 2", tagWith(t, out, "data-src-step2", ""), map[string]string{"class": "mg-step2"})
			lacksAttr(t, "step 2", tagWith(t, out, "data-src-step2", ""), "hidden")
			open := strings.Contains(out, `<details class="mg-adv" open>`)
			if open != c.open {
				t.Errorf("Advanced open = %v, want %v; body=%s", open, c.open, out)
			}
			if !strings.Contains(out, `class="sdet__err mg-ed__msg"`) {
				t.Errorf("no error; body=%s", out)
			}
		})
	}
}

// A success starts the next add at the find step, with the notice above it.
func TestSourceAddSuccessReturnsToTheFindStep(t *testing.T) {
	w, _ := sourcesTestWeb(t, config.FeedsDoc{})
	out := postForm(t, w, "/sources", url.Values{"name": {"Fresh"}, "url": {"https://fresh.test/feed"}})
	wantAttrs(t, "step 2", tagWith(t, out, "data-src-step2", ""), map[string]string{"hidden": present})
	if strings.Index(out, "Added Fresh.") > strings.Index(out, `id="ed-find-new"`) {
		t.Errorf("the notice is below the find step; body=%s", out)
	}
}

// The edit form is the old one: no find step, and since and max items in line.
func TestSourceEditFormKeepsTuningInLine(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}}})
	out := get(t, w, "/sources/feed/"+feedID(t, db, "Alpha"))
	for _, bad := range []string{"mg-adv", "data-src-find", "data-src-step2", "ed-dup-new", "ed-prev-new", "data-src-preview", "mg-add__head", "data-find-failed"} {
		if strings.Contains(out, bad) {
			t.Errorf("the edit form carries %q; body=%s", bad, out)
		}
	}
	for _, want := range []string{`name="since"`, `name="max_items"`, `placeholder="example.com/feed.xml"`} {
		if !strings.Contains(out, want) {
			t.Errorf("the edit form lost %s; body=%s", want, out)
		}
	}
}
