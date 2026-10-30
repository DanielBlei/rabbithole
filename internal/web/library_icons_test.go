// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/library"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// Sources of overviewLibraryYAML the baked icons are set for.
const (
	bakedPlainURL = "https://a.test/1" // an icon
	bakedDarkURL  = "https://a.test/2" // a dark icon
	bakedNoneURL  = "https://a.test/3" // known to have none
)

// bakedLibraryFS is overviewLibraryYAML with an icons file for two of its sources.
func bakedLibraryFS(t *testing.T) fstest.MapFS {
	t.Helper()
	png64 := base64.StdEncoding.EncodeToString(pngBytes(t))
	icons := fmt.Sprintf(
		`{"version": 1, "icons": {%q: {"png": %q, "dark": false}, %q: {"png": %q, "dark": true}}, "none": [%q]}`,
		config.FeedID(bakedPlainURL), png64, config.FeedID(bakedDarkURL), png64, config.FeedID(bakedNoneURL))
	return fstest.MapFS{
		"rss.yaml":        {Data: []byte(overviewLibraryYAML)},
		library.IconsFile: {Data: []byte(icons)},
	}
}

// bakedWeb is a Web over doc whose library ships bakedLibraryFS.
func bakedWeb(t *testing.T, doc config.FeedsDoc) (*Web, *store.Store) {
	t.Helper()
	w, db := sourcesTestWeb(t, doc)
	w.SetLibrary(library.NewLoader(library.Options{Dir: bakedLibraryFS(t)}))
	if snap := w.library.Get(t.Context()); !snap.Loaded || len(snap.Icons) != 2 {
		t.Fatalf("test library did not load: %+v", snap)
	}
	return w, db
}

// hasBakedImg is whether the fragment holds the library icon of id.
func hasBakedImg(body, id string) bool {
	return strings.Contains(body, `class="fav__i" src="/icons/library/`+id+`?v=`)
}

func TestLibraryIconRouteServesBakedBytes(t *testing.T) {
	w, _ := bakedWeb(t, config.FeedsDoc{})
	id := config.FeedID(bakedPlainURL)
	tag := w.library.Snapshot().Icons[id].ETag

	rec := iconReq(w.Routes(), http.MethodGet, "/icons/library/"+id+"?v="+tag)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	for k, want := range map[string]string{
		"Content-Type":            "image/png",
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": "default-src 'none'; sandbox",
		"Cache-Control":           "private, max-age=604800, immutable",
		"ETag":                    `"` + tag + `"`,
	} {
		if got := rec.Header().Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if !bytes.Equal(rec.Body.Bytes(), pngBytes(t)) {
		t.Error("body differs from the baked bytes")
	}

	rec = iconReq(w.Routes(), http.MethodGet, "/icons/library/"+id, "If-None-Match", `"`+tag+`"`)
	if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Errorf("matching tag: status = %d with %d body bytes, want an empty 304", rec.Code, rec.Body.Len())
	}
	rec = iconReq(w.Routes(), http.MethodGet, "/icons/library/"+id, "If-None-Match", `"other"`)
	if rec.Code != http.StatusOK {
		t.Errorf("a different tag: status = %d, want 200", rec.Code)
	}
}

func TestLibraryIconRouteIs404WithoutCaching(t *testing.T) {
	w, _ := bakedWeb(t, config.FeedsDoc{})
	for _, id := range []string{"unknown", config.FeedID(bakedNoneURL), "a%2Fb", "..%2F..%2Fetc"} {
		rec := iconReq(w.Routes(), http.MethodGet, "/icons/library/"+id)
		if rec.Code != http.StatusNotFound {
			t.Errorf("/icons/library/%s: status = %d, want 404", id, rec.Code)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("/icons/library/%s: Cache-Control = %q, want no-store", id, got)
		}
	}
	// A library without icons answers 404 too.
	plain, _ := overviewLibraryWeb(t, config.FeedsDoc{})
	rec := iconReq(plain.Routes(), http.MethodGet, "/icons/library/"+config.FeedID(bakedPlainURL))
	if rec.Code != http.StatusNotFound {
		t.Errorf("library without icons: status = %d, want 404", rec.Code)
	}
}

func TestLibraryIconRouteWithTheLibraryOffIs404(t *testing.T) {
	w, _ := sourcesTestWeb(t, config.FeedsDoc{})
	w.SetLibrary(library.NewLoader(library.Options{BaseURL: "off"}))
	rec := iconReq(w.Routes(), http.MethodGet, "/icons/library/"+config.FeedID(bakedPlainURL))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

// With the library unreachable and never loaded, the icon route waits for the loader's bound at
// most, then answers 404.
func TestLibraryIconRouteReturnsWithinTheLoaderTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	const timeout = 150 * time.Millisecond
	w, _ := sourcesTestWeb(t, config.FeedsDoc{})
	w.SetLibrary(library.NewLoader(library.Options{Client: srv.Client(), BaseURL: srv.URL, Timeout: timeout}))

	start := time.Now()
	rec := iconReq(w.Routes(), http.MethodGet, "/icons/library/"+config.FeedID(bakedPlainURL))
	if elapsed := time.Since(start); elapsed > 10*timeout {
		t.Errorf("route took %s, want about the loader timeout %s", elapsed, timeout)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestLibraryIconRouteSitsBehindTheLogin(t *testing.T) {
	w := gatedWeb(t)
	w.SetLibrary(library.NewLoader(library.Options{Dir: bakedLibraryFS(t)}))
	id := config.FeedID(bakedPlainURL)
	b := newBrowser(t, w)

	wantRedirect(t, b.get("/icons/library/"+id), "/login?next=%2Ficons%2Flibrary%2F"+id)
	wantRedirect(t, b.login("alice", "correct horse"), "/")
	rec := b.get("/icons/library/" + id)
	wantStatus(t, rec, http.StatusOK)
	if got := rec.Header().Get("Cache-Control"); got != iconCacheControl {
		t.Errorf("Cache-Control = %q, want %q (the gate's no-store must be replaced)", got, iconCacheControl)
	}
}

func TestIconRefSrcPointsLibraryIconsAtTheirRoute(t *testing.T) {
	if got := (iconRef{ID: "f1", ETag: "t1", Lib: true}).Src(); got != "/icons/library/f1?v=t1" {
		t.Errorf("library Src = %q", got)
	}
	if got := (iconRef{ID: "f1", ETag: "t1"}).Src(); got != "/icons/f1?v=t1" {
		t.Errorf("feed Src = %q", got)
	}
}

// An unsubscribed Library row shows the baked icon; a feed's own icon wins once subscribed;
// a source with none keeps the letter tile; dark adds the plate.
func TestLibraryRowsPreferTheFeedIconThenTheBakedOne(t *testing.T) {
	sub := config.Feed{Name: "Alpha One", URL: bakedPlainURL}
	w, db := bakedWeb(t, config.FeedsDoc{Feeds: []config.Feed{sub}})
	plainID, darkID := config.FeedID(bakedPlainURL), config.FeedID(bakedDarkURL)

	out := get(t, w, "/sources/library")
	if row := libRow(t, out, bakedDarkURL); !hasBakedImg(row, darkID) || !strings.Contains(row, "fav--dark") {
		t.Errorf("unsubscribed row with a dark baked icon: %s", row)
	}
	if row := libRow(t, out, bakedPlainURL); !hasBakedImg(row, plainID) {
		t.Errorf("subscribed row without a fetched icon should show the baked one: %s", row)
	}
	if row := libRow(t, out, bakedNoneURL); strings.Contains(row, "<img") || !strings.Contains(row, "fav--mono") {
		t.Errorf("source with no icon should be a monogram: %s", row)
	}
	if row := libRow(t, out, "https://a.test/4"); strings.Contains(row, "<img") || !strings.Contains(row, "fav--mono") {
		t.Errorf("source the file does not know should be a monogram: %s", row)
	}

	putIcon(t, db, bakedPlainURL, "own1", false)
	out = get(t, w, "/sources/library")
	row := libRow(t, out, bakedPlainURL)
	if !hasIconImg(row, plainID) || hasBakedImg(row, plainID) {
		t.Errorf("the feed's own icon should win over the baked one: %s", row)
	}
}

func TestSubscribedRowsAndSuggestionsUseTheBakedIcon(t *testing.T) {
	feeds := append(goFeeds(), config.Feed{Name: "Alpha Two", URL: bakedDarkURL})
	w, _ := bakedWeb(t, config.FeedsDoc{Feeds: feeds})
	darkID, plainID := config.FeedID(bakedDarkURL), config.FeedID(bakedPlainURL)

	manage := get(t, w, "/sources/manage")
	row := manage[strings.Index(manage, `id="src-`+darkID+`"`):]
	row, _, _ = strings.Cut(row, `class="mg-item"`)
	if !hasBakedImg(row, darkID) || !strings.Contains(row, "fav--dark") {
		t.Errorf("subscribed Manage row lacks the baked icon: %s", row)
	}

	// Alpha One shares the Go label with the subscribed sources and is offered.
	out := get(t, w, "/sources")
	sug := out[strings.Index(out, `id="ov-sug-`+plainID+`"`):]
	sug, _, _ = strings.Cut(sug, "</li>")
	if !hasBakedImg(sug, plainID) {
		t.Errorf("suggestion row lacks the baked icon: %s", sug)
	}
}
