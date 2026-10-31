// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/library"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// Library entries the tests lean on, from library/*.yaml.
const (
	libK8sName = "Kubernetes blog"
	libK8sURL  = "https://kubernetes.io/feed.xml"
	libGoName  = "Go blog"
	libGoURL   = "https://go.dev/blog/feed.atom"
	libCNCFURL = "https://www.cncf.io/feed/"
	libKVName  = "arXiv: KV cache"
	libKVURL   = "https://arxiv.org/search?q=kv+cache"
	libLGName  = "arXiv cs.LG (Machine Learning)"
	libLGURL   = "https://rss.arxiv.org/rss/cs.LG"
)

// libraryServer serves the real library/ files.
func libraryServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.FileServer(http.Dir("../../library")))
	t.Cleanup(srv.Close)
	return srv
}

func libraryLoader(srv *httptest.Server) *library.Loader {
	return library.NewLoader(library.Options{Client: srv.Client(), BaseURL: srv.URL})
}

// libraryTestWeb is a Web over a store seeded with doc and a loader reading
// the real library files, already loaded the way the boot preload leaves it.
func libraryTestWeb(t *testing.T, doc config.FeedsDoc) (*Web, *store.Store) {
	t.Helper()
	w, db := sourcesTestWeb(t, doc)
	w.SetLibrary(libraryLoader(libraryServer(t)))
	if snap := w.library.Get(t.Context()); !snap.Loaded {
		t.Fatalf("test library did not load: %+v", snap)
	}
	return w, db
}

func libKey(rawURL string) string { return config.FeedURLKey(rawURL) }

// attrOf is v as html/template writes it in an attribute.
func attrOf(v string) string { return strings.ReplaceAll(html.EscapeString(v), "+", "&#43;") }

// libRow cuts the library row of a source out of the page.
func libRow(t *testing.T, body, rawURL string) string {
	t.Helper()
	open := `<li class="li-row" id="lib-` + config.FeedID(rawURL) + `"`
	_, rest, ok := strings.Cut(body, open)
	if !ok {
		t.Fatalf("no library row for %s; body=%s", rawURL, body)
	}
	row, _, _ := strings.Cut(rest, "</li>")
	return row
}

func postLibrary(t *testing.T, w *Web, path string, form url.Values, hdr map[string]string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST %s: status = %d, want 200; body=%s", path, rec.Code, rec.Body)
	}
	return rec.Body.String()
}

func libraryTotal(t *testing.T, w *Web) int {
	t.Helper()
	return len(libraryEntries(w.library.Get(t.Context())))
}

func TestLibraryPageListsEveryGroup(t *testing.T) {
	w, _ := libraryTestWeb(t, config.FeedsDoc{})
	out := get(t, w, "/sources/library")
	total := libraryTotal(t, w)

	for _, want := range []string{
		"<title>Library</title>", `<h1 class="li-title">Library</h1>`, "All sources in the Library",
		"Blogs", "Releases", "Community", "Papers", "LLM systems", "Agents, RAG and evals", "Journals",
		`placeholder="Search name, address or tag"`, `aria-label="Filter the Library"`,
		`data-lib-state="all"`, `data-lib-state="subscribed"`, `data-lib-state="removed"`, `data-lib-state="add"`,
		"Filter Library by status", `data-lib-tags-toggle`, `aria-expanded="false"`, `id="libTagFilters"`,
		"Filter by tag", `id="libBody"`, "data-lib-bar",
		`aria-label="Select ` + libK8sName + `"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(out, "@subscribed") || strings.Contains(out, "@removed") || strings.Contains(out, "@add") {
		t.Error("Library page still exposes @status filters")
	}
	for _, want := range []string{
		`class="li-sec__t"><h3>Agents, RAG and evals</h3>`,
		`class="li-sec__t"><h3>Papers</h3>`,
		`class="li-sec__t"><h3>Blogs</h3>`,
		`class="li-sec__t"><h3>Community</h3>`,
		`class="li-sec__t"><h3>Journals</h3>`,
		`class="li-sec__t"><h3>LLM systems</h3>`,
		`class="li-sec__t"><h3>Releases</h3>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing sorted group heading %q", want)
		}
	}
	order := []string{
		`class="li-sec__t"><h3>Agents, RAG and evals</h3>`,
		`class="li-sec__t"><h3>Blogs</h3>`,
		`class="li-sec__t"><h3>Community</h3>`,
		`class="li-sec__t"><h3>Journals</h3>`,
		`class="li-sec__t"><h3>LLM systems</h3>`,
		`class="li-sec__t"><h3>Papers</h3>`,
		`class="li-sec__t"><h3>Releases</h3>`,
	}
	last := -1
	for _, marker := range order {
		at := strings.Index(out, marker)
		if at <= last {
			t.Errorf("group headings are not alphabetized at %q", marker)
		}
		last = at
	}
	blogAt := strings.Index(out, `data-title="Blogs"`)
	if blogAt < 0 {
		t.Fatal("Blogs section not found")
	}
	blog, _, _ := strings.Cut(out[blogAt:], `</section>`)
	var blogNames []string
	for rest := blog; ; {
		_, rowRest, ok := strings.Cut(rest, `<li class="li-row"`)
		if !ok {
			break
		}
		row, tail, _ := strings.Cut(rowRest, `</li>`)
		_, nameRest, _ := strings.Cut(row, `class="li-row__nm">`)
		name, _, _ := strings.Cut(nameRest, `</span>`)
		blogNames = append(blogNames, name)
		rest = tail
	}
	for i := 1; i < len(blogNames); i++ {
		if strings.ToLower(blogNames[i-1]) > strings.ToLower(blogNames[i]) {
			t.Errorf("source names are not alphabetized: %q before %q", blogNames[i-1], blogNames[i])
		}
	}
	if n := strings.Count(out, `<li class="li-row"`); n != total {
		t.Errorf("rows = %d, want %d", n, total)
	}
	if n := strings.Count(out, `data-state="add"`); n != total {
		t.Errorf("add rows = %d, want %d", n, total)
	}
	if want := plural(total, "source", "sources") + ", 0 added"; !strings.Contains(out, want) {
		t.Errorf("missing meta %q", want)
	}
	if strings.Contains(out, "/sources/live") {
		t.Error("the library page polls the live route")
	}
	// The nav lists the groups once the library has loaded.
	for _, want := range []string{
		`<details class="sh-nav__grp" open>`, `href="/sources/library/journals"`,
		`href="/sources/library" aria-current="page"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("nav missing %q", want)
		}
	}
}

func TestLibraryRowCarriesTheEntry(t *testing.T) {
	w, _ := libraryTestWeb(t, config.FeedsDoc{})
	out := get(t, w, "/sources/library")

	kv := libRow(t, out, libKVURL)
	for _, want := range []string{
		`data-key="` + attrOf(libKey(libKVURL)) + `"`, `class="stp-chip li-kind">academic<`,
		`value="` + attrOf(libKey(libKVURL)) + `"`,
		"Newest arXiv papers on KV cache", `data-lib-chip="Inference"`, `aria-label="Add ` + libKVName + `"`,
		`hx-post="/sources/library/apply"`,
	} {
		if !strings.Contains(kv, want) {
			t.Errorf("academic row missing %q; row=%s", want, kv)
		}
	}
	lg := libRow(t, out, libLGURL)
	if !strings.Contains(lg, "max 25 per run") || strings.Contains(lg, "li-kind") {
		t.Errorf("rss row with a cap = %s", lg)
	}
}

func TestLibraryGroupPage(t *testing.T) {
	w, _ := libraryTestWeb(t, config.FeedsDoc{})
	out := get(t, w, "/sources/library/arxiv-categories")

	for _, want := range []string{
		"<title>Papers</title>", `<h1 class="li-title">Papers</h1>`, "arXiv cs.AI (Artificial Intelligence)", `data-scope="this group"`,
		`href="/sources/library/arxiv-categories" aria-current="page"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, not := range []string{libK8sName, "<h3>Releases</h3>", `<h3>Journals</h3>`} {
		if strings.Contains(out, not) {
			t.Errorf("group page holds %q", not)
		}
	}

	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sources/library/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown group status = %d, want 404", rec.Code)
	}
}

func TestLibraryRowStates(t *testing.T) {
	w, db := libraryTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{
		{Name: libK8sName, URL: libK8sURL},
		{Name: libGoName, URL: libGoURL},
		{Name: "CNCF blog", URL: "https://other.test/feed"},
	}})
	if err := db.SoftDeleteFeed(t.Context(), feedID(t, db, libGoName)); err != nil {
		t.Fatalf("SoftDeleteFeed: %v", err)
	}
	out := get(t, w, "/sources/library")
	for _, want := range []string{
		`value="all" data-lib-state="all" checked><span>All <i class="mg-n" data-lib-state-count="all">`,
		`data-lib-state="subscribed"><span>Subscribed <i class="mg-n" data-lib-state-count="subscribed">1</i></span>`,
		`data-lib-state="removed"><span>Removed <i class="mg-n" data-lib-state-count="removed">1</i></span>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing status filter/count %q", want)
		}
	}

	subscribed := libRow(t, out, libK8sURL)
	if !strings.Contains(subscribed, `data-state="subscribed"`) || !strings.Contains(subscribed, "Subscribed") ||
		strings.Contains(subscribed, `type="checkbox"`) {
		t.Errorf("subscribed row = %s", subscribed)
	}
	removed := libRow(t, out, libGoURL)
	for _, want := range []string{
		`data-state="removed"`, "Removed", `hx-post="/sources/library/restore"`, `name="key"`,
		`aria-label="Restore ` + libGoName + `"`,
	} {
		if !strings.Contains(removed, want) {
			t.Errorf("removed row missing %q; row=%s", want, removed)
		}
	}
	if strings.Contains(removed, `type="checkbox"`) {
		t.Errorf("removed row can be ticked; row=%s", removed)
	}
	conflict := libRow(t, out, libCNCFURL)
	for _, want := range []string{
		`data-state="conflict"`, "Name taken", "Name taken by a source with a different URL",
		`class="libact__why" title="`,
	} {
		if !strings.Contains(conflict, want) {
			t.Errorf("conflict row missing %q; row=%s", want, conflict)
		}
	}
	if !strings.Contains(out, ", 1 added") {
		t.Errorf("meta does not count the subscribed source")
	}
}

func TestLibraryPlanPreviewsPickedKeys(t *testing.T) {
	w, db := libraryTestWeb(t, config.FeedsDoc{})
	out := postLibrary(t, w, "/sources/library/plan", url.Values{
		"pick": {libKey(libK8sURL), libKey(libKVURL), "https://nowhere.test/feed"},
		// A document from the client is never read.
		"yaml": {"feeds:\n  - name: Evil\n    url: https://evil.test/feed\n"},
	}, nil)

	if n := strings.Count(out, `data-status="new"`); n != 2 {
		t.Errorf("new rows = %d, want 2; body=%s", n, out)
	}
	if n := strings.Count(out, ` data-plan-pick checked`); n != 2 {
		t.Errorf("ticked rows = %d, want 2", n)
	}
	for _, want := range []string{
		">Add from the library<", ">Add 2<", `hx-post="/sources/library/apply"`, `hx-target="#libBody"`,
		"data-plan-cancel", "Back to the Library", `data-lib-list hidden`, `id="libMeta" hx-swap-oob="true"`,
		libK8sName, libKVName,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(out, "Evil") || strings.Contains(out, "hx-get") {
		t.Error("plan took the client document or a server cancel")
	}
	doc, err := db.FeedsDoc(t.Context())
	if err != nil || len(doc.Feeds) != 0 {
		t.Errorf("plan wrote feeds = %v, %v", doc.Feeds, err)
	}

	empty := postLibrary(t, w, "/sources/library/plan", url.Values{"pick": {"https://nowhere.test/feed"}}, nil)
	if !strings.Contains(empty, "Nothing selected.") || strings.Contains(empty, "data-plan") {
		t.Errorf("plan of an unknown key = %s", empty)
	}
}

func TestLibraryApplyAddsPickedSources(t *testing.T) {
	w, db := libraryTestWeb(t, config.FeedsDoc{})
	out := postLibrary(t, w, "/sources/library/apply", url.Values{
		"pick": {libKey(libKVURL), libKey(libLGURL)},
	}, nil)

	for _, want := range []string{"Added 2 sources", `id="libBody"`, ", 2 added"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q; body=%s", want, out)
		}
	}
	if strings.Contains(out, "data-plan") {
		t.Error("a finished apply still shows the plan")
	}
	if !strings.Contains(libRow(t, out, libKVURL), "Subscribed") {
		t.Error("added entry is not marked Subscribed")
	}

	doc, err := db.FeedsDoc(t.Context())
	if err != nil {
		t.Fatalf("FeedsDoc: %v", err)
	}
	byName := make(map[string]config.Feed)
	for _, f := range doc.Feeds {
		byName[f.Name] = f
	}
	kv, ok := byName[libKVName]
	if !ok || kv.Type != config.FeedTypeAcademic || kv.MaxItems != nil {
		t.Errorf("academic entry stored as %+v", kv)
	}
	if strings.Join(kv.Tags, ",") != "AI,Inference,Papers" {
		t.Errorf("academic tags = %v, want the display form", kv.Tags)
	}
	lg, ok := byName[libLGName]
	if !ok || lg.Type != config.FeedTypeRSS && lg.Type != "" || lg.MaxItems == nil || *lg.MaxItems != 25 {
		t.Errorf("capped entry stored as %+v", lg)
	}
}

func TestLibrarySingleAdd(t *testing.T) {
	w, db := libraryTestWeb(t, config.FeedsDoc{})
	out := postLibrary(t, w, "/sources/library/apply", url.Values{"pick": {libKey(libK8sURL)}}, nil)
	if !strings.Contains(out, "Added 1 source") || strings.Contains(out, "Added 1 sources") {
		t.Errorf("banner; body=%s", out)
	}
	doc, _ := db.FeedsDoc(t.Context())
	if len(doc.Feeds) != 1 || doc.Feeds[0].Name != libK8sName || doc.Feeds[0].URL != libK8sURL {
		t.Errorf("stored feeds = %+v", doc.Feeds)
	}
}

// A pick that no longer plans as new is reviewed again, not applied blind.
func TestLibraryApplyStalePick(t *testing.T) {
	w, db := libraryTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{{Name: libK8sName, URL: libK8sURL}}})

	out := postLibrary(t, w, "/sources/library/apply", url.Values{"pick": {libKey(libK8sURL)}}, nil)
	for _, want := range []string{
		"Everything you picked changed since the preview.", `data-status="subscribed"`, "data-plan",
		`data-lib-list hidden`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stale apply missing %q; body=%s", want, out)
		}
	}
	if strings.Contains(out, "data-plan-pick") {
		t.Error("a subscribed row can be ticked")
	}

	mixed := postLibrary(t, w, "/sources/library/apply", url.Values{
		"pick": {libKey(libK8sURL), libKey(libGoURL)},
	}, nil)
	if !strings.Contains(mixed, "Added 1 source, skipped 1 that changed since the preview") {
		t.Errorf("mixed banner; body=%s", mixed)
	}
	doc, _ := db.FeedsDoc(t.Context())
	if len(doc.Feeds) != 2 {
		t.Errorf("feeds = %d, want 2", len(doc.Feeds))
	}

	none := postLibrary(t, w, "/sources/library/apply", url.Values{}, nil)
	if !strings.Contains(none, "Nothing selected.") {
		t.Errorf("empty apply; body=%s", none)
	}
}

// A reply repaints the group the request came from.
func TestLibraryReplyKeepsTheGroup(t *testing.T) {
	w, _ := libraryTestWeb(t, config.FeedsDoc{})
	hdr := map[string]string{"HX-Current-URL": "http://localhost/sources/library/journals"}
	out := postLibrary(t, w, "/sources/library/apply", url.Values{"pick": {libKey(libK8sURL)}}, hdr)
	if !strings.Contains(out, "Added 1 source") || !strings.Contains(out, "Communications of the ACM") {
		t.Errorf("reply lost the group; body=%s", out)
	}
	if strings.Contains(out, `id="lib-`+config.FeedID(libK8sURL)+`"`) {
		t.Errorf("reply holds a row of another group")
	}
}

func TestLibraryRestoreIsAPlainUndelete(t *testing.T) {
	w, db := libraryTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{{Name: "My Go notes", URL: libGoURL}}})
	id := feedID(t, db, "My Go notes")
	if err := db.SoftDeleteFeed(t.Context(), id); err != nil {
		t.Fatalf("SoftDeleteFeed: %v", err)
	}

	out := postLibrary(t, w, "/sources/library/restore", url.Values{"key": {libKey(libGoURL)}}, nil)
	if !strings.Contains(out, "Restored My Go notes") {
		t.Errorf("banner; body=%s", out)
	}
	if !strings.Contains(libRow(t, out, libGoURL), "Subscribed") {
		t.Errorf("restored entry is not Subscribed")
	}
	f, err := db.FeedByID(t.Context(), id)
	if err != nil || f.Name != "My Go notes" {
		t.Errorf("restored feed = %+v, %v; the edited name must survive", f, err)
	}
	if deleted, _ := db.DeletedFeeds(t.Context()); len(deleted) != 0 {
		t.Errorf("deleted = %d, want 0", len(deleted))
	}
}

func TestLibraryRestoreChecksTheKey(t *testing.T) {
	w, db := libraryTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{
		{Name: libK8sName, URL: libK8sURL},
		{Name: libGoName, URL: libGoURL},
	}})
	goID := feedID(t, db, libGoName)
	if err := db.SoftDeleteFeed(t.Context(), goID); err != nil {
		t.Fatalf("SoftDeleteFeed: %v", err)
	}

	// A live source and a source not yet added are not restorable, and a posted
	// id is not believed.
	for name, form := range map[string]url.Values{
		"live": {"key": {libKey(libK8sURL)}},
		"new":  {"key": {libKey(libKVURL)}, "id": {goID}},
	} {
		out := postLibrary(t, w, "/sources/library/restore", form, nil)
		if !strings.Contains(out, "is not among your removed sources any more.") ||
			!strings.Contains(out, "sdet__err") {
			t.Errorf("%s: body=%s", name, out)
		}
	}
	if code := postFormCode(w, "/sources/library/restore", url.Values{"id": {goID}}); code != http.StatusBadRequest {
		t.Errorf("restore without a key: status = %d, want 400", code)
	}
	deleted, _ := db.DeletedFeeds(t.Context())
	if len(deleted) != 1 || deleted[0].Name != libGoName {
		t.Errorf("deleted = %+v, want the removed source left alone", deleted)
	}
}

func TestLibraryNotLoadedOffersTryAgain(t *testing.T) {
	var healthy atomic.Bool
	files := libraryServer(t)
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if !healthy.Load() {
			http.Error(rw, "down", http.StatusInternalServerError)
			return
		}
		files.Config.Handler.ServeHTTP(rw, r)
	}))
	t.Cleanup(srv.Close)
	w, _ := sourcesTestWeb(t, config.FeedsDoc{})
	now := time.Now()
	var skew atomic.Int64
	w.SetLibrary(library.NewLoader(library.Options{
		Client: srv.Client(), BaseURL: srv.URL,
		Now: func() time.Time { return now.Add(time.Duration(skew.Load())) },
	}))

	out := get(t, w, "/sources/library")
	for _, want := range []string{
		"Could not load the Library. Check your connection and try again.", ">Try again<",
		`hx-post="/sources/library/refresh"`, `hx-target="#lib"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, not := range []string{"<details", "data-lib-filter", "data-lib-bar"} {
		if strings.Contains(out, not) {
			t.Errorf("not loaded page holds %q", not)
		}
	}
	if !strings.Contains(out, `<a class="sh-ni is-on" href="/sources/library" aria-current="page">Library</a>`) {
		t.Errorf("nav is not the single Library link")
	}

	healthy.Store(true)
	skew.Store(int64(time.Minute))
	retry := postLibrary(t, w, "/sources/library/refresh", url.Values{}, nil)
	for _, want := range []string{
		`id="lib"`, `<li class="li-row"`, `id="srcLibNav" hx-swap-oob="true"`, `<details class="sh-nav__grp" open>`,
	} {
		if !strings.Contains(retry, want) {
			t.Errorf("refresh missing %q", want)
		}
	}
	if strings.Contains(retry, "Could not load") {
		t.Error("refresh still says the library could not load")
	}
	if !strings.Contains(get(t, w, "/sources/library"), `<li class="li-row"`) {
		t.Error("the page does not list the library after a refresh")
	}
}

func TestLibraryOff(t *testing.T) {
	w, _ := sourcesTestWeb(t, config.FeedsDoc{})
	w.SetLibrary(library.NewLoader(library.Options{Client: http.DefaultClient, BaseURL: "off"}))
	out := get(t, w, "/sources/library")
	if !strings.Contains(out, "The Library is turned off on this server.") {
		t.Errorf("missing off copy; body=%s", out)
	}
	for _, not := range []string{"Try again", "data-lib-filter", `<li class="li-row"`, "<details"} {
		if strings.Contains(out, not) {
			t.Errorf("off page holds %q", not)
		}
	}
}

// The default Web holds a loader without a client: it fetches nothing and the
// page reports it could not load.
func TestLibraryDefaultLoaderIsOffline(t *testing.T) {
	w, _ := sourcesTestWeb(t, config.FeedsDoc{})
	out := get(t, w, "/sources/library")
	if !strings.Contains(out, "Could not load the Library.") {
		t.Errorf("body=%s", out)
	}
}

// The nav lists the Library groups on every section once the library is loaded.
func TestSourcesNavListsLibraryGroupsOnceLoaded(t *testing.T) {
	w, _ := libraryTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}}})
	after := get(t, w, "/sources/manage")
	for _, want := range []string{
		`<details class="sh-nav__grp" open>`, `<summary class="sh-nav__h">Library</summary>`,
		`href="/sources/library/blogs"`, `<span class="sh-ni__t">All</span>`,
	} {
		if !strings.Contains(after, want) {
			t.Errorf("nav missing %q", want)
		}
	}
	if regexp.MustCompile(`href="/sources/library[^"]*"[^>]*aria-current`).MatchString(after) {
		t.Errorf("a Library item is current on Manage")
	}
}

func TestLibraryStatesKeyEveryEntry(t *testing.T) {
	w, _ := libraryTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{{Name: libK8sName, URL: libK8sURL}}})
	snap := w.library.Get(t.Context())
	states, err := w.libraryStates(t.Context(), snap)
	if err != nil {
		t.Fatalf("libraryStates: %v", err)
	}
	if len(states) != len(libraryEntries(snap)) {
		t.Errorf("states = %d, want one per entry (%d)", len(states), len(libraryEntries(snap)))
	}
	if st := states[libKey(libK8sURL)].Status; st != store.PlanSubscribed {
		t.Errorf("status of a stored entry = %q", st)
	}
	if st := states[libKey(libKVURL)].Status; st != store.PlanNew {
		t.Errorf("status of a new entry = %q", st)
	}
}

// Manage with no sources points at the Library as well.
func TestManageFirstRunLinksTheLibrary(t *testing.T) {
	w, _ := sourcesTestWeb(t, config.FeedsDoc{})
	out := get(t, w, "/sources/manage")
	for _, want := range []string{
		"import a sources file, or start from the Library.",
		`<a class="clk-btn" href="/sources/library">Browse the Library</a>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
}

// A removed entry picked by hand is skipped: the Library never restores over
// the user's edits.
func TestLibraryApplySkipsRemovedPicks(t *testing.T) {
	w, db := libraryTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{{Name: "My Go notes", URL: libGoURL}}})
	if err := db.SoftDeleteFeed(t.Context(), feedID(t, db, "My Go notes")); err != nil {
		t.Fatalf("SoftDeleteFeed: %v", err)
	}

	only := postLibrary(t, w, "/sources/library/apply", url.Values{"pick": {libKey(libGoURL)}}, nil)
	if !strings.Contains(only, "Everything you picked changed since the preview.") {
		t.Errorf("removed-only apply; body=%s", only)
	}
	mixed := postLibrary(t, w, "/sources/library/apply", url.Values{
		"pick": {libKey(libGoURL), libKey(libK8sURL)},
	}, nil)
	if !strings.Contains(mixed, "Added 1 source, skipped 1 that changed since the preview") {
		t.Errorf("mixed apply; body=%s", mixed)
	}
	if deleted, _ := db.DeletedFeeds(t.Context()); len(deleted) != 1 || deleted[0].Name != "My Go notes" {
		t.Errorf("deleted = %+v, want the removed source left alone", deleted)
	}
}

func TestLibraryPlanCannotSelectRemoved(t *testing.T) {
	w, db := libraryTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{{Name: libGoName, URL: libGoURL}}})
	if err := db.SoftDeleteFeed(t.Context(), feedID(t, db, libGoName)); err != nil {
		t.Fatalf("SoftDeleteFeed: %v", err)
	}
	out := postLibrary(t, w, "/sources/library/plan", url.Values{
		"pick": {libKey(libGoURL), libKey(libK8sURL)},
	}, nil)
	if !strings.Contains(out, `data-status="removed"`) {
		t.Fatalf("plan lacks the removed row; body=%s", out)
	}
	if n := strings.Count(out, "data-plan-pick"); n != 1 {
		t.Errorf("selectable rows = %d, want only the new one", n)
	}
}

func TestLibraryRefreshUnknownGroupIsNotFound(t *testing.T) {
	w, _ := libraryTestWeb(t, config.FeedsDoc{})
	req := httptest.NewRequest(http.MethodPost, "/sources/library/refresh", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Current-URL", "http://localhost/sources/library/nope")
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}
