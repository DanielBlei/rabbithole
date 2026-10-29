// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"bytes"
	"context"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// sourcesTestWeb builds a Web over a store seeded with doc, so the page is
// tested against feeds that went through the real write path and the real
// defaults cascade rather than a hand-built view model.
func sourcesTestWeb(t *testing.T, doc config.FeedsDoc) (*Web, *store.Store) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.SeedFeeds(t.Context(), doc); err != nil {
		t.Fatalf("SeedFeeds: %v", err)
	}
	w := New(db, config.Static{}, testIngestManager(t, db))
	return w, db
}

// summaryOf returns the text of the Manage summary line.
func summaryOf(t *testing.T, body string) string {
	t.Helper()
	const open = `<p class="mg-sub" id="srcSummary">`
	_, rest, ok := strings.Cut(body, open)
	if !ok {
		t.Fatalf("no summary line; body=%s", body)
	}
	text, _, _ := strings.Cut(rest, "</p>")
	return text
}

// feedID looks up the stored ID for a feed by name, which is how the editor
// addresses it.
func feedID(t *testing.T, db *store.Store, name string) string {
	t.Helper()
	for _, list := range [][]config.Feed{mustFeeds(t, db), mustDeleted(t, db)} {
		for _, f := range list {
			if f.Name == name {
				return f.ID
			}
		}
	}
	t.Fatalf("no feed named %q", name)
	return ""
}

func mustFeeds(t *testing.T, db *store.Store) []config.Feed {
	t.Helper()
	feeds, err := db.Feeds(t.Context())
	if err != nil {
		t.Fatalf("Feeds: %v", err)
	}
	return feeds
}

func mustDeleted(t *testing.T, db *store.Store) []config.Feed {
	t.Helper()
	feeds, err := db.DeletedFeeds(t.Context())
	if err != nil {
		t.Fatalf("DeletedFeeds: %v", err)
	}
	return feeds
}

// seedItem records one item filed under source, so a rename has something to
// carry with it.
func seedItem(t *testing.T, db *store.Store, source string) {
	t.Helper()
	if err := db.Record(t.Context(),
		[]feeds.Item{{ID: source + "-1", Source: source, Title: "An item", Link: "https://x.test/" + source}},
		nil, time.Now()); err != nil {
		t.Fatalf("Record: %v", err)
	}
}

// The editor's job is showing resolved values and where they came from.
func TestSourcesRendersResolvedValues(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{
		Defaults: config.FeedDefaults{MaxItems: intPtr(5), Tags: []string{"all"}},
		Feeds: []config.Feed{
			{Name: "Alpha", URL: "https://alpha.test/feed", Since: durPtr(48 * time.Hour), Tags: []string{"ai"}},
			{Name: "Beta", URL: "https://beta.test/feed"},
			{Name: "Parked", URL: "https://parked.test/feed", Enabled: boolPtr(false)},
		},
	})
	list := get(t, w, "/sources/manage")
	detail := get(t, w, "/sources/feed/"+feedID(t, db, "Beta"))

	cases := []struct {
		name string
		in   string
		want string
		why  string
	}{
		{
			name: "summary", in: list, want: "3 sources · 2 enabled · none failing",
			why: "parked feeds count, one of the three is parked",
		},
		{name: "parked feed listed", in: list, want: "Parked", why: "disabled feeds are shown, not hidden"},
		{name: "parked row", in: list, want: `data-state="off"`, why: "parked renders as its own state"},
		{name: "never fetched", in: list, want: "Waiting for first run", why: "no history recorded yet"},
		{
			name: "search data",
			in:   list,
			want: `data-url="https://alpha.test/feed"`,
			why:  "the client filter matches on the url",
		},
		{
			name: "built-in since as the placeholder",
			in:   detail,
			want: `placeholder="7d"`,
			why:  "Beta sets no window, so the empty box shows what it will use",
		},
		{
			name: "defaults cap as the placeholder",
			in:   detail,
			want: `placeholder="5"`,
			why:  "Beta's cap comes from the defaults",
		},
		{name: "inherited tag shown", in: detail, want: "from the defaults", why: "default tags union onto every feed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !strings.Contains(c.in, c.want) {
				t.Errorf("missing %q (%s); body=%s", c.want, c.why, c.in)
			}
		})
	}
}

// The summary line, the attention block and the filter pills are each rendered
// twice: inline on the page and again out of band by every mutation. The page
// must carry exactly one of each; two would mean a duplicate id and a stray
// visible copy.
func TestSourcesCountsRenderOnce(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{
		Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}},
	})
	page := get(t, w, "/sources/manage")
	body := postForm(t, w, "/sources/feed/"+feedID(t, db, "Alpha")+"/enabled", url.Values{"enabled": {"off"}})
	for _, id := range []string{"srcSummary", "srcAttn", "srcFilter", "srcNavBadge"} {
		if n := strings.Count(page, `id="`+id+`"`); n != 1 {
			t.Errorf("#%s appears %d times on the page, want 1", id, n)
		}
		// A mutation response carries each as an out-of-band update to that element.
		if !strings.Contains(body, `id="`+id+`"`) || strings.Count(body, `hx-swap-oob="true"`) < 4 {
			t.Errorf("a mutation did not carry #%s out of band; body=%s", id, body)
		}
	}
	if strings.Contains(page, `hx-swap-oob`) {
		t.Errorf("the page itself carries an out-of-band element; body=%s", page)
	}
}

// A blank tuning field means "take the default" and has to post back empty, so
// the form must not pre-fill the default as a value — it belongs in the
// placeholder, where it reads as what you'd get rather than what you set.
func TestSourcesDetailLeavesDefaultedFieldsBlank(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{
		Defaults: config.FeedDefaults{Since: durPtr(72 * time.Hour)},
		Feeds:    []config.Feed{{Name: "Beta", URL: "https://beta.test/feed"}},
	})
	out := get(t, w, "/sources/feed/"+feedID(t, db, "Beta"))

	if !strings.Contains(out, `name="since" value=""`) {
		t.Errorf("the default was pre-filled into the field; body=%s", out)
	}
	if !strings.Contains(out, `placeholder="3d"`) {
		t.Errorf("the field does not show the default it will take; body=%s", out)
	}
	// Uncapped is prose, not the em dash the row list uses: in a placeholder a
	// dash reads as "no value" where the meaning is "no limit".
	if !strings.Contains(out, `placeholder="uncapped"`) {
		t.Errorf("an uncapped default should say so; body=%s", out)
	}
}

// The add form has no feed to resolve, so it used to show no defaults at all.
func TestSourcesAddFormShowsDefaults(t *testing.T) {
	w, _ := sourcesTestWeb(t, config.FeedsDoc{
		Defaults: config.FeedDefaults{Since: durPtr(72 * time.Hour), MaxItems: intPtr(9)},
		Feeds:    []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}},
	})
	out := get(t, w, "/sources/new")

	if !strings.Contains(out, `placeholder="3d"`) || !strings.Contains(out, `placeholder="9"`) {
		t.Errorf("the add form does not show what a new feed will default to; body=%s", out)
	}
}

// Health is joined onto the feed by ID and drives the row's state.
func TestSourcesShowsHealth(t *testing.T) {
	good := config.Feed{Name: "Good", URL: "https://good.test/feed"}
	broken := config.Feed{Name: "Broken", URL: "https://broken.test/feed"}
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{good, broken}})

	now := time.Now()
	if err := db.RecordFeedFetches(t.Context(), []store.FeedFetch{
		{FeedID: config.FeedID(good.URL), Name: good.Name, Items: 7, At: now},
		{FeedID: config.FeedID(broken.URL), Name: broken.Name, Error: "dial tcp: no such host", At: now},
	}); err != nil {
		t.Fatalf("RecordFeedFetches: %v", err)
	}
	list := get(t, w, "/sources/manage")

	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "ok row", in: list, want: `data-state="ok"`},
		{name: "flaky row", in: list, want: `data-state="flaky"`},
		{name: "flaky count", in: list, want: "none failing · 1 flaky"},
		{name: "flaky note", in: list, want: "Two more misses in a row and it counts as failing."},
		{name: "history strip", in: list, want: "Last 12 fetches, newest on the right: 1 ok, 0 failed"},
		{name: "failed tick", in: list, want: "Last 12 fetches, newest on the right: 0 ok, 1 failed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !strings.Contains(c.in, c.want) {
				t.Errorf("missing %q; body=%s", c.want, c.in)
			}
		})
	}
}

// A disabled feed reports as parked even with history on file — old errors from
// before it was switched off must not read as a live problem.
func TestSourcesDisabledOverridesStaleHealth(t *testing.T) {
	parked := config.Feed{Name: "Parked", URL: "https://parked.test/feed", Enabled: boolPtr(false)}
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{parked}})
	if err := db.RecordFeedFetches(t.Context(), []store.FeedFetch{
		{FeedID: config.FeedID(parked.URL), Name: parked.Name, Error: "410 gone", At: time.Now()},
	}); err != nil {
		t.Fatalf("RecordFeedFetches: %v", err)
	}
	out := get(t, w, "/sources/manage")

	if !strings.Contains(out, `data-state="off"`) {
		t.Errorf("expected the parked state; body=%s", out)
	}
	if !strings.Contains(out, "none failing") || strings.Contains(out, "mg-attn__toggle") {
		t.Errorf("a disabled feed must not count as failing; body=%s", out)
	}
	// Its history is still there to look back at, dimmed.
	if !strings.Contains(
		out,
		`hs hs--dim" role="img" aria-label="Last 12 fetches, newest on the right: 0 ok, 1 failed"`,
	) {
		t.Errorf("parked feed lost its history strip; body=%s", out)
	}
}

// A feed is flaky until its fail streak reaches failingStreak, then failing. Each
// case seeds one feed and checks its state, the summary line, the Failing pill
// and the Manage badge, which must show the failing count only.
func TestSourcesFailingNeedsAStreak(t *testing.T) {
	cases := []struct {
		name    string
		streak  int
		enabled bool
		state   string
		failing int
		flaky   int
	}{
		{name: "healthy", streak: 0, enabled: true, state: "ok"},
		{name: "one failure is flaky", streak: 1, enabled: true, state: "flaky", flaky: 1},
		{name: "two failures are flaky", streak: 2, enabled: true, state: "flaky", flaky: 1},
		{name: "three failures are failing", streak: 3, enabled: true, state: "error", failing: 1},
		{name: "four failures are failing", streak: 4, enabled: true, state: "error", failing: 1},
		{name: "parked feed counts as neither", streak: 3, enabled: false, state: "off"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			feed := config.Feed{Name: "Feed", URL: "https://feed.test/rss", Enabled: boolPtr(c.enabled)}
			w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{feed}})

			id, now := config.FeedID(feed.URL), time.Now()
			var attempts []store.FeedFetch
			if c.streak == 0 {
				attempts = append(attempts, store.FeedFetch{FeedID: id, Name: feed.Name, Items: 3, At: now})
			}
			for i := range c.streak {
				attempts = append(attempts, store.FeedFetch{
					FeedID: id, Name: feed.Name, Error: "503 unavailable", At: now.Add(time.Duration(i) * time.Second),
				})
			}
			if err := db.RecordFeedFetches(t.Context(), attempts); err != nil {
				t.Fatalf("RecordFeedFetches: %v", err)
			}

			list := get(t, w, "/sources/manage")
			if want := fmt.Sprintf(`data-state="%s"`, c.state); !strings.Contains(list, want) {
				t.Errorf("missing %q; body=%s", want, list)
			}
			summary := summaryOf(t, list)
			for _, flag := range []struct {
				name string
				want int
			}{{"failing", c.failing}, {"flaky", c.flaky}} {
				phrase := fmt.Sprintf("%d %s", flag.want, flag.name)
				switch {
				case flag.want == 0 && flag.name == "flaky" && strings.Contains(summary, "flaky"):
					t.Errorf("flaky printed for a feed that does not count as flaky: %q", summary)
				case flag.want == 0 && flag.name == "failing" && !strings.Contains(summary, "none failing"):
					t.Errorf("summary does not say none failing: %q", summary)
				case flag.want > 0 && !strings.Contains(summary, phrase):
					t.Errorf("summary missing %q: %q", phrase, summary)
				}
			}
			pill := fmt.Sprintf(`Failing <i class="mg-n">%d</i>`, c.failing)
			if !strings.Contains(list, pill) {
				t.Errorf("missing the Failing pill count %q; body=%s", pill, list)
			}

			overview := get(t, w, "/sources")
			if c.failing == 0 {
				if strings.Contains(overview, "sh-ni__n--bad") {
					t.Errorf("Manage badge shown with nothing failing; body=%s", overview)
				}
			} else if !strings.Contains(overview, fmt.Sprintf(`title="%d failing"`, c.failing)) {
				t.Errorf("Manage badge missing the failing count %d; body=%s", c.failing, overview)
			}
		})
	}
}

func TestSourcesToggleEnabled(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{
		Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}},
	})
	id := feedID(t, db, "Alpha")

	postForm(t, w, "/sources/feed/"+id+"/enabled", url.Values{"enabled": {"off"}})
	feeds := mustFeeds(t, db)
	if feeds[0].Enabled == nil || *feeds[0].Enabled {
		t.Fatalf("feed was not disabled: %+v", feeds[0].Enabled)
	}

	out := postForm(t, w, "/sources/feed/"+id+"/enabled", url.Values{"enabled": {"on"}})
	feeds = mustFeeds(t, db)
	if feeds[0].Enabled == nil || !*feeds[0].Enabled {
		t.Fatalf("feed was not re-enabled: %+v", feeds[0].Enabled)
	}
	if !strings.Contains(out, "1 source · 1 enabled · none failing") {
		t.Errorf("the summary did not follow the toggle; body=%s", out)
	}
}

// Adding is where a typo is most likely, so every rejection has to come back as
// an inline message with the form still filled in — never an error page.
func TestSourcesAddRejectionsRenderInline(t *testing.T) {
	w, _ := sourcesTestWeb(t, config.FeedsDoc{
		Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}},
	})
	cases := []struct {
		name string
		form url.Values
		want string
		why  string
	}{
		{
			name: "duplicate name",
			form: url.Values{"name": {"Alpha"}, "url": {"https://other.test/feed"}},
			want: "name already exists",
			why:  "one feed per name",
		},
		{
			name: "duplicate url",
			form: url.Values{"name": {"Another"}, "url": {"https://alpha.test/feed"}},
			want: "url already exists",
			why:  "one feed per link",
		},
		{
			name: "wrong scheme",
			form: url.Values{"name": {"Another"}, "url": {"ftp://example.test/feed"}},
			want: "must start with http",
			why:  "a form field can hold anything, unlike a hand-written file",
		},
		{
			name: "unparseable since",
			form: url.Values{"name": {"Another"}, "url": {"https://other.test/feed"}, "since": {"soon"}},
			want: "invalid duration",
			why:  "the window is free text",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := postForm(t, w, "/sources", c.form)
			if !strings.Contains(out, c.want) {
				t.Errorf("missing %q (%s); body=%s", c.want, c.why, out)
			}
			if !strings.Contains(out, `value="`+c.form.Get("name")+`"`) {
				t.Errorf("the rejected form lost what was typed; body=%s", out)
			}
		})
	}
}

func TestSourcesAddThenDeleteThenRestore(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{
		Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}},
	})
	postForm(t, w, "/sources", url.Values{
		"name": {"Added"}, "url": {"https://added.test/feed"}, "tags": {"ai,news"},
	})
	if len(mustFeeds(t, db)) != 2 {
		t.Fatalf("feed was not added: %+v", mustFeeds(t, db))
	}
	id := feedID(t, db, "Added")

	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/sources/feed/"+id, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE: status = %d; body=%s", rec.Code, rec.Body)
	}
	if len(mustFeeds(t, db)) != 1 {
		t.Fatalf("feed was not deleted: %+v", mustFeeds(t, db))
	}
	// Soft: still there to bring back, and still holding its history.
	if len(mustDeleted(t, db)) != 1 {
		t.Fatal("delete was not soft")
	}
	if !strings.Contains(get(t, w, "/sources/manage"), "Restore") {
		t.Error("a deleted feed offers no way back")
	}

	postForm(t, w, "/sources/feed/"+id+"/restore", nil)
	if len(mustFeeds(t, db)) != 2 {
		t.Fatalf("feed was not restored: %+v", mustFeeds(t, db))
	}
}

// Feeds arrive in batches, so a successful add leaves the form open and empty
// for the next one instead of opening the feed it just made — landing in the
// editor means reaching for "add feed" again after every single one.
func TestSourcesAddLeavesTheFormReadyForAnother(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{
		Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}},
	})
	body := postForm(t, w, "/sources", url.Values{
		"name": {"Added"}, "url": {"https://added.test/feed"},
	})

	wantAttrs(t, "add unit", tagWith(t, body, "id", "src-new"), map[string]string{"class": "mg-item mg-item--add"})
	wantAttrs(t, "add form", tagWith(t, body, "id", "ed-new"), map[string]string{"hx-post": "/sources"})
	if !strings.Contains(body, "Added Added. It is fetched on the next ingestion run.") {
		t.Errorf("no confirmation naming the feed created; body=%s", body)
	}
	// The empty form must not be carrying the feed just made, or the next add
	// would resubmit it and collide on the name.
	if strings.Contains(body, `value="https://added.test/feed"`) {
		t.Errorf("the form kept the values it just saved; body=%s", body)
	}
	// It still landed, and shows in the list beside the form.
	if len(mustFeeds(t, db)) != 2 {
		t.Fatalf("feed was not added: %+v", mustFeeds(t, db))
	}
	if !strings.Contains(body, `data-name="Added"`) {
		t.Errorf("the new feed is not in the list; body=%s", body)
	}
}

// Saving says so. The pane keeps the feed open underneath — the confirmation
// clears itself, so it can't be in the way of the next edit.
func TestSourcesSaveAnswersWithTheUnit(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{
		Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}},
	})
	id := feedID(t, db, "Alpha")
	body := postForm(t, w, "/sources/feed/"+id, url.Values{
		"name": {"Alpha"}, "url": {"https://alpha.test/feed"},
	})

	for _, want := range []string{
		`<div class="mg-item" id="src-` + id + `"`,
		`<form class="mg-ed" id="ed-` + id + `"`,
		`<p class="sdet__ok mg-ed__msg" role="status" data-src-notice="">Saved</p>`,
		`id="srcSummary" hx-swap-oob="true"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q; body=%s", want, body)
		}
	}
	if strings.Contains(body, `id="srcBody"`) {
		t.Errorf("a save repainted the whole list; body=%s", body)
	}
}

// Deleting asks first, in a dialog stacked over Sources rather than a second
// button under the pointer that just clicked the first one.
func TestSourcesDeleteAsksBeforeItActs(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{
		Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}},
	})
	id := feedID(t, db, "Alpha")

	ask := get(t, w, "/sources/feed/"+id+"/confirm-delete")
	if !strings.Contains(ask, `class="modal"`) || !strings.Contains(ask, "Alpha") {
		t.Errorf("no dialog naming the feed; body=%s", ask)
	}
	if !strings.Contains(ask, `hx-delete="/sources/feed/`+id+`"`) {
		t.Errorf("the dialog does not carry the delete; body=%s", ask)
	}
	// Asking must not act.
	if len(mustFeeds(t, db)) != 1 {
		t.Fatal("opening the confirmation deleted the feed")
	}

	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/sources/feed/"+id, nil))
	body := rec.Body.String()

	if !strings.Contains(body, `hx-swap-oob="innerHTML:#modalTop"`) {
		t.Errorf("the confirmation dialog was left open; body=%s", body)
	}
	wantAttrs(t, "delete button", tagWith(t, ask, "hx-delete", "/sources/feed/"+id), map[string]string{
		"hx-target": "#src-" + id, "hx-swap": "outerHTML",
	})
	// The answer is that source's unit in its removed state, not the whole list.
	if !strings.Contains(body, `<div class="mg-item" id="src-`+id+`"`) || !strings.Contains(body, `data-removed="1"`) {
		t.Errorf("no removed unit; body=%s", body)
	}
	if strings.Contains(body, `id="srcBody"`) || strings.Contains(body, `class="mg-ed"`) {
		t.Errorf("the delete repainted the list or kept an editor; body=%s", body)
	}
	if !strings.Contains(body, `Removed <i class="mg-n">1</i>`) {
		t.Errorf("the pill counts did not follow the delete; body=%s", body)
	}
}

// Renaming has to carry the feed's existing items with it: items.source records
// the feed's name, so otherwise they'd be filed under a label nothing claims.
func TestSourcesRenameRefilesItems(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{
		Feeds: []config.Feed{{Name: "Old Name", URL: "https://alpha.test/feed"}},
	})
	seedItem(t, db, "Old Name")

	out := postForm(t, w, "/sources/feed/"+feedID(t, db, "Old Name"), url.Values{
		"name": {"New Name"}, "url": {"https://alpha.test/feed"},
	})
	if !strings.Contains(out, "re-filed 1 item") {
		t.Errorf("the rename did not report re-filing; body=%s", out)
	}

	sources, err := db.Sources(t.Context())
	if err != nil {
		t.Fatalf("Sources: %v", err)
	}
	if len(sources) != 1 || sources[0].Source != "New Name" {
		t.Errorf("items were not re-filed: %+v", sources)
	}
}

// A live run files new items under the name it resolved at start, so a rename
// waits for it; any other edit goes through.
func TestSourcesRenameRefusedDuringRun(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	w, db := sourcesTestWeb(t, config.FeedsDoc{
		Feeds: []config.Feed{{Name: "Old Name", URL: srv.URL + "/feed"}},
	})
	w.ing.SetClient(srv.Client())
	if err := w.ing.Start(t.Context(), store.IngestTriggerManual); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { w.ing.Shutdown(context.Background()) })
	if !w.ing.Status().Running {
		t.Fatal("run is not live")
	}

	id := feedID(t, db, "Old Name")
	out := postForm(t, w, "/sources/feed/"+id, url.Values{"name": {"New Name"}, "url": {srv.URL + "/feed"}})
	if !strings.Contains(out, "finish or cancel the ingest run before renaming") {
		t.Errorf("rename not refused; body=%s", out)
	}
	if got := mustFeeds(t, db)[0].Name; got != "Old Name" {
		t.Errorf("name = %q after a refused rename", got)
	}

	out = postForm(
		t,
		w,
		"/sources/feed/"+id,
		url.Values{"name": {"Old Name"}, "url": {srv.URL + "/feed"}, "tags": {"x"}},
	)
	if strings.Contains(out, "before renaming") {
		t.Errorf("a save without a rename was refused; body=%s", out)
	}
}

// The ID is minted once and frozen, so editing a feed's URL keeps its history
// instead of silently starting a new feed.
func TestSourcesURLEditKeepsHistory(t *testing.T) {
	const oldURL = "https://alpha.test/feed"
	w, db := sourcesTestWeb(t, config.FeedsDoc{
		Feeds: []config.Feed{{Name: "Alpha", URL: oldURL}},
	})
	id := feedID(t, db, "Alpha")
	if err := db.RecordFeedFetches(t.Context(), []store.FeedFetch{
		{FeedID: id, Name: "Alpha", Items: 4, At: time.Now()},
	}); err != nil {
		t.Fatalf("RecordFeedFetches: %v", err)
	}

	postForm(t, w, "/sources/feed/"+id, url.Values{
		"name": {"Alpha"}, "url": {"https://alpha.test/feed.xml"},
	})
	if got := feedID(t, db, "Alpha"); got != id {
		t.Errorf("the feed's ID changed with its URL: %q -> %q", id, got)
	}
	if out := get(t, w, "/sources/manage"); !strings.Contains(out, "newest on the right: 1 ok, 0 failed") {
		t.Errorf("the feed lost its history when its URL changed; body=%s", out)
	}
}

func TestSourcesDefaultsEditMovesInheritedFeeds(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{
		Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}},
	})
	postForm(t, w, "/sources/defaults", url.Values{"since": {"3d"}, "max_items": {"12"}})

	defaults, err := db.FeedDefaults(t.Context())
	if err != nil {
		t.Fatalf("FeedDefaults: %v", err)
	}
	if defaults.Since == nil || defaults.Since.Short() != "3d" {
		t.Errorf("defaults.Since = %+v, want 3d", defaults.Since)
	}
	// The feed sets neither knob, so both now come from the defaults.
	out := get(t, w, "/sources/feed/"+feedID(t, db, "Alpha"))
	if !strings.Contains(out, `placeholder="3d"`) || !strings.Contains(out, `placeholder="12"`) {
		t.Errorf("the feed did not pick up the new defaults; body=%s", out)
	}
}

// The defaults form has no tags field, so a save posts none — which must not be
// read as an instruction to clear tags a seed file set. This is the one failure
// mode here that would lose data.
func TestSourcesSaveDefaultsKeepsSeededTags(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{
		Defaults: config.FeedDefaults{Tags: []string{"seeded"}, MaxItems: intPtr(25)},
		Feeds:    []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}},
	})
	if !strings.Contains(get(t, w, "/sources/defaults"), "max items") {
		t.Fatal("the defaults form did not render")
	}
	if strings.Contains(get(t, w, "/sources/defaults"), `name="tags"`) {
		t.Error("the defaults form still offers a tags field")
	}

	postForm(t, w, "/sources/defaults", url.Values{"since": {"3d"}, "max_items": {"12"}})

	defaults, err := db.FeedDefaults(t.Context())
	if err != nil {
		t.Fatalf("FeedDefaults: %v", err)
	}
	if strings.Join(defaults.Tags, ",") != "seeded" {
		t.Errorf("Tags = %v, want the seeded tag preserved through the save", defaults.Tags)
	}
	if defaults.MaxItems == nil || *defaults.MaxItems != 12 {
		t.Errorf("MaxItems = %v, want the edited 12", defaults.MaxItems)
	}
}

// The summary's optional parts only appear when they have something to say, so
// a healthy page isn't padded with zeroes, and a removal moves the Removed pill.
func TestSourcesSummaryOmitsEmptyParts(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{
		Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}},
	})
	if got := summaryOf(t, get(t, w, "/sources/manage")); got != "1 source · 1 enabled · none failing" {
		t.Errorf("summary = %q", got)
	}

	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/sources/feed/"+feedID(t, db, "Alpha"), nil))

	out := get(t, w, "/sources/manage")
	if !strings.Contains(out, `Removed <i class="mg-n">1</i>`) {
		t.Error("the Removed pill did not count the removal")
	}
	if got := summaryOf(t, out); got != "No sources yet" {
		t.Errorf("summary after removing the only source = %q", got)
	}
}

// A delete has to land in the response it was made from. The row stays in the list
// marked deleted, so the response to the delete already carries it and the
// filter has something to reveal — no reload, no second request.
func TestSourcesDeleteShowsUpInTheSameResponse(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{
		Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}},
	})

	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/sources/feed/"+feedID(t, db, "Alpha"), nil))
	body := rec.Body.String()

	if !strings.Contains(body, `data-removed="1"`) {
		t.Errorf("the deleted feed left the list instead of being marked; body=%s", body)
	}
	if !strings.Contains(body, "Restore") {
		t.Errorf("the delete response offers no way back; body=%s", body)
	}
	if !strings.Contains(body, `Removed <i class="mg-n">1</i>`) {
		t.Errorf("the pill counts did not follow the delete; body=%s", body)
	}
}

// The filter is a radio group of pills, and every filter it offers is one the
// client-side script can serve, including Removed, whose rows are in the list
// like the rest.
func TestSourcesFilterIsSelfContained(t *testing.T) {
	w, _ := sourcesTestWeb(t, config.FeedsDoc{
		Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}},
	})
	out := get(t, w, "/sources/manage")

	if !strings.Contains(out, `role="radiogroup"`) {
		t.Errorf("the filter is not a radio group; body=%s", out)
	}
	for _, want := range []string{
		`value="all"`, `value="enabled"`, `value="disabled"`, `value="failing"`, `value="flaky"`, `value="removed"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the filter is missing %s; body=%s", want, out)
		}
	}
	// A filter that navigated would drop whatever the search box was holding.
	if strings.Contains(out, "?deleted=1") {
		t.Errorf("a filter still navigates; body=%s", out)
	}
	// Zero pills are dimmed, All never.
	zero := `<label class="stp-seg__o is-zero"><input type="radio" name="srcfilter" value="`
	if !strings.Contains(out, zero+`failing"`) {
		t.Errorf("an empty pill is not marked; body=%s", out)
	}
	if strings.Contains(out, zero+`all"`) {
		t.Errorf("the All pill is dimmed; body=%s", out)
	}
	if !strings.Contains(out, "data-src-matches") {
		t.Errorf("no place for the shown-of-total count; body=%s", out)
	}
}

// What a typed address becomes on the way into the store. The scheme is filled
// in because a bare address is what people paste; http survives because a feed
// that only serves it would otherwise never be fetched at all.
func TestSourcesAddNormalizesURL(t *testing.T) {
	cases := []struct {
		name  string
		typed string
		want  string
		why   string
	}{
		{
			name: "bare host", typed: "example.test/feed.xml",
			want: "https://example.test/feed.xml",
			why:  "the common paste, and https is the safe assumption",
		},
		{
			name: "protocol relative", typed: "//example.test/feed.xml",
			want: "https://example.test/feed.xml",
			why:  "copied out of a page's markup",
		},
		{
			name: "http kept", typed: "http://example.test/feed.xml",
			want: "http://example.test/feed.xml",
			why:  "upgrading it silently would break a feed that only serves http",
		},
		{
			name: "https untouched", typed: "https://example.test/feed.xml",
			want: "https://example.test/feed.xml",
			why:  "a complete url passes through as written",
		},
		{
			name: "surrounding space", typed: "  example.test/feed.xml  ",
			want: "https://example.test/feed.xml",
			why:  "a paste carries whitespace",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, db := sourcesTestWeb(t, config.FeedsDoc{})
			postForm(t, w, "/sources", url.Values{"name": {"Feed"}, "url": {c.typed}})

			feeds := mustFeeds(t, db)
			if len(feeds) != 1 {
				t.Fatalf("feed was not added (%s): %+v", c.why, feeds)
			}
			if feeds[0].URL != c.want {
				t.Errorf("URL = %q, want %q (%s)", feeds[0].URL, c.want, c.why)
			}
		})
	}
}

// The same feed typed with and without its scheme has to be one feed: the ID is
// the URL's digest, so normalising late would mint a second row for it.
func TestSourcesSchemeIsNotASecondFeed(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{
		Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}},
	})
	out := postForm(t, w, "/sources", url.Values{
		"name": {"Alpha Again"}, "url": {"alpha.test/feed"},
	})

	if len(mustFeeds(t, db)) != 1 {
		t.Errorf("the same feed was stored twice: %+v", mustFeeds(t, db))
	}
	if !strings.Contains(out, "url already exists") {
		t.Errorf("the duplicate was not reported; body=%s", out)
	}
}

// http is accepted, so the form has to say what it is rather than let it pass
// unremarked.
func TestSourcesFlagsInsecureURL(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{
		Feeds: []config.Feed{{Name: "Plain", URL: "http://plain.test/feed"}},
	})
	if !strings.Contains(get(t, w, "/sources/feed/"+feedID(t, db, "Plain")), "data-src-insecure") {
		t.Error("no place for the insecure-connection warning")
	}
}

// The export has to be a seed file, not just a readable dump: its whole purpose
// is reproducing the set somewhere else.
func TestSourcesExportRoundTrips(t *testing.T) {
	w, _ := sourcesTestWeb(t, config.FeedsDoc{
		Defaults: config.FeedDefaults{MaxItems: intPtr(5)},
		Feeds: []config.Feed{
			{Name: "Alpha", URL: "https://alpha.test/feed", Since: durPtr(48 * time.Hour), Tags: []string{"ai"}},
			{Name: "Parked", URL: "https://parked.test/feed", Enabled: boolPtr(false)},
		},
	})
	out := get(t, w, "/sources/export")

	// The window must come back as it was written, not as a nanosecond count.
	if !strings.Contains(out, "since: 2d") {
		t.Errorf("since was not exported in its short form; body=%s", out)
	}
	if !strings.Contains(out, "enabled: false") {
		t.Errorf("a parked feed did not export its state; body=%s", out)
	}
	if strings.Contains(out, "172800000000000") {
		t.Errorf("a duration leaked as nanoseconds; body=%s", out)
	}
}

// Manage is a whole page: it carries the document and the layout. The editor
// lives on it; defaults and export open as dialogs over it, so their targets
// are #modalTop.
func TestSourcesManageIsAPage(t *testing.T) {
	w, _ := sourcesTestWeb(t, config.FeedsDoc{
		Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}},
	})
	out := get(t, w, "/sources/manage")

	if !strings.Contains(strings.ToLower(out), "<!doctype") || !strings.Contains(out, "<html") {
		t.Errorf("manage is not a whole page; body=%.300s", out)
	}
	if strings.Contains(out, "modal__frame--sources") {
		t.Errorf("a modal frame is rendered around the editor; body=%s", out)
	}
	for _, want := range []string{
		`hx-get="/sources/defaults" hx-target="#modalTop"`,
		`hx-get="/sources/export" hx-target="#modalTop"`,
		`<h1 class="mg-title">Manage sources</h1>`,
		"src-page__foot",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q; body=%s", want, out)
		}
	}
}

// The section nav is on both pages. Overview has no summary line, so its id
// stays unique to Manage; the failing badge is shown only when there is
// something to report.
func TestSourcesPagesShowTheNav(t *testing.T) {
	w, _ := sourcesTestWeb(t, config.FeedsDoc{
		Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}},
	})
	overview := get(t, w, "/sources")
	manage := get(t, w, "/sources/manage")

	if !strings.Contains(overview, `<nav class="sh-nav"`) {
		t.Errorf("overview has no section nav; body=%s", overview)
	}
	if !strings.Contains(overview, `href="/sources" aria-current="page"`) {
		t.Errorf("overview link is not marked current; body=%s", overview)
	}
	if n := strings.Count(overview, `id="srcSummary"`); n != 0 {
		t.Errorf("overview renders the summary %d times, want 0", n)
	}
	if !strings.Contains(manage, `href="/sources/manage" aria-current="page"`) {
		t.Errorf("manage link is not marked current; body=%s", manage)
	}
	if n := strings.Count(manage, `id="srcSummary"`); n != 1 {
		t.Errorf("manage renders the summary %d times, want 1", n)
	}

	t.Run("no badge when nothing fails", func(t *testing.T) {
		if strings.Contains(manage, "sh-ni__n--bad") {
			t.Errorf("badge shown with no failing feeds; body=%s", manage)
		}
	})

	t.Run("badge counts failing feeds", func(t *testing.T) {
		broken := config.Feed{Name: "Broken", URL: "https://broken.test/feed"}
		w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{broken}})
		if err := db.RecordFeedFetches(t.Context(), []store.FeedFetch{
			{FeedID: config.FeedID(broken.URL), Name: broken.Name, Error: "dial tcp: no such host", At: time.Now()},
			{
				FeedID: config.FeedID(broken.URL),
				Name:   broken.Name,
				Error:  "dial tcp: no such host",
				At:     time.Now().Add(time.Second),
			},
			{
				FeedID: config.FeedID(broken.URL),
				Name:   broken.Name,
				Error:  "dial tcp: no such host",
				At:     time.Now().Add(2 * time.Second),
			},
		}); err != nil {
			t.Fatalf("RecordFeedFetches: %v", err)
		}
		out := get(t, w, "/sources/manage")
		badge := `<span class="sh-ni__n sh-ni__n--bad" title="1 failing">1<span class="stp-sr"> failing</span></span>`
		if !strings.Contains(out, badge) {
			t.Errorf("no failing badge on the nav; body=%s", out)
		}
	})
}

// Sources is a page, so the topbar and the navrail mark it current there and
// nowhere else.
func TestSourcesTopbarAndRailMarkTheActivePage(t *testing.T) {
	w, _ := sourcesTestWeb(t, config.FeedsDoc{
		Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}},
	})
	for _, path := range []string{"/sources", "/sources/manage"} {
		out := get(t, w, path)
		if !strings.Contains(out, `class="active" href="/sources"`) {
			t.Errorf("GET %s: topbar Sources link is not active", path)
		}
		if !strings.Contains(out, `navrail__item--on" href="/sources"`) {
			t.Errorf("GET %s: navrail Sources item is not active", path)
		}
	}

	feed := get(t, w, "/feed")
	if strings.Contains(feed, `class="active" href="/sources"`) {
		t.Error("GET /feed marks the Sources topbar link active")
	}
	if strings.Contains(feed, `navrail__item--on" href="/sources"`) {
		t.Error("GET /feed marks the Sources navrail item active")
	}
}

// Every entry point to Sources is a link to the page. None of them opens the
// Sources dialog through htmx.
func TestNoEntryPointOpensSourcesAsADialog(t *testing.T) {
	empty := newEmptyWeb(t)
	full := newTestWeb(t)
	for _, c := range []struct {
		w    *Web
		path string
	}{
		{empty, "/feed"},
		{empty, "/maze"},
		{empty, "/ingest"},
		{full, "/feed"},
		{full, "/settings/general"},
	} {
		if body := get(t, c.w, c.path); strings.Contains(body, `hx-get="/sources"`) {
			t.Errorf("GET %s opens Sources as a dialog", c.path)
		}
	}
}

// The feed query parameter opens that feed in the editor, so a link can land on
// one feed directly.
func TestSourcesManagePreselectsAFeed(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{
		{Name: "Alpha", URL: "https://alpha.test/feed"},
		{Name: "Beta", URL: "https://beta.test/feed"},
	}})
	out := get(t, w, "/sources/manage?feed="+feedID(t, db, "Beta"))

	if !strings.Contains(out, `name="name" value="Beta"`) {
		t.Errorf("the detail pane does not show the chosen feed; body=%s", out)
	}
}

// Saving the defaults has to close its own dialog. The response clears
// #modalTop, which is only correct while that is the layer it opened into.
func TestSourcesSaveDefaultsClosesItsDialog(t *testing.T) {
	w, _ := sourcesTestWeb(t, config.FeedsDoc{
		Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}},
	})
	body := postForm(t, w, "/sources/defaults", url.Values{"since": {"3d"}})

	if !strings.Contains(body, `hx-swap-oob="innerHTML:#modalTop"`) {
		t.Errorf("saving defaults left its dialog open; body=%s", body)
	}
	if !strings.Contains(body, `id="srcBody"`) {
		t.Errorf("saving defaults did not repaint the columns underneath; body=%s", body)
	}
}

func TestSourcesEmptyStore(t *testing.T) {
	w, _ := sourcesTestWeb(t, config.FeedsDoc{})
	out := get(t, w, "/sources/manage")

	if !strings.Contains(out, `<h2>No sources yet</h2>`) || !strings.Contains(out, "Add a source") {
		t.Errorf("an empty store should render a zero-state; body=%s", out)
	}
	if got := summaryOf(t, out); got != "No sources yet" {
		t.Errorf("summary = %q, want the zero state", got)
	}
	if strings.Contains(out, "mg-attn") || strings.Contains(out, "Nothing failing") {
		t.Errorf("an empty store rendered a status block; body=%s", out)
	}
}

func TestSourcesMissingFeedIs404(t *testing.T) {
	w, _ := sourcesTestWeb(t, config.FeedsDoc{
		Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/sources/feed/nope", strings.NewReader("name=x&url=https://x.test/f"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", rec.Code, rec.Body)
	}
}

// An academic search can be created from the page, badges its row, and RSS
// stays stored as unset so it reads back like every feed from a seed file.
func TestSourcesAddPicksType(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{})
	body := postForm(t, w, "/sources", url.Values{
		"name": {"arXiv RAG"}, "url": {"https://export.arxiv.org/api/query?search_query=all:rag"},
		"type": {"academic"},
	})
	postForm(t, w, "/sources", url.Values{
		"name": {"Blog"}, "url": {"https://blog.test/feed"}, "type": {"rss"},
	})

	if got := mustFeed(t, db, "arXiv RAG").Type; got != config.FeedTypeAcademic {
		t.Errorf("academic add stored type %q", got)
	}
	if got := mustFeed(t, db, "Blog").Type; got != "" {
		t.Errorf("rss add stored type %q, want unset", got)
	}
	if !strings.Contains(body, " · academic<") {
		t.Errorf("the academic row carries no kind note; body=%s", body)
	}
	if strings.Contains(get(t, w, "/sources/manage"), " · rss<") {
		t.Error("an rss row was noted, but rss is the default")
	}
	if out := get(t, w, "/sources/export"); strings.Contains(out, "type: rss") {
		t.Errorf("an rss feed added from the page exported a type; body=%s", out)
	}
}

// The form only offers kinds ingest can fetch, and the server holds that line
// against a hand-crafted post too.
func TestSourcesAddRejectsUnfetchableType(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{})
	form := get(t, w, "/sources/new")
	for _, typ := range []string{"rss", "academic"} {
		if !strings.Contains(form, `name="type" value="`+typ+`"`) {
			t.Errorf("the add form does not offer %s; body=%s", typ, form)
		}
	}
	if strings.Contains(form, `value="blog"`) || strings.Contains(form, `value="news"`) {
		t.Errorf("the add form offers a type ingest skips; body=%s", form)
	}

	out := postForm(t, w, "/sources", url.Values{
		"name": {"Blog"}, "url": {"https://blog.test/feed"}, "type": {"blog"},
	})
	if !strings.Contains(out, "can&#39;t be fetched yet") {
		t.Errorf("an unfetchable type was not rejected inline; body=%s", out)
	}
	if len(mustFeeds(t, db)) != 0 {
		t.Errorf("a rejected feed was stored: %+v", mustFeeds(t, db))
	}
}

// Editing keeps the kind a feed already has: an academic search stays one, an
// explicit rss stays in the export, and a seeded kind the form doesn't offer
// is shown and kept rather than quietly turned into RSS.
func TestSourcesSaveKeepsType(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{
		{Name: "arXiv", URL: "https://export.arxiv.org/api/query?search_query=all:rag", Type: config.FeedTypeAcademic},
		{Name: "Explicit", URL: "https://explicit.test/feed", Type: config.FeedTypeRSS},
		{Name: "Later", URL: "https://later.test/feed", Type: config.FeedTypeBlog},
	}})

	for _, c := range []struct {
		name, url, posted string
		want              config.FeedType
	}{
		{"arXiv", "https://export.arxiv.org/api/query?search_query=all:rag", "academic", config.FeedTypeAcademic},
		{"Explicit", "https://explicit.test/feed", "rss", config.FeedTypeRSS},
		{"Later", "https://later.test/feed", "blog", config.FeedTypeBlog},
	} {
		id := feedID(t, db, c.name)
		if pane := get(t, w, "/sources/feed/"+id); !strings.Contains(pane, `value="`+c.posted+`" checked`) {
			t.Errorf("%s: the pane does not show its type checked; body=%s", c.name, pane)
		}
		postForm(t, w, "/sources/feed/"+id, url.Values{
			"name": {c.name}, "url": {c.url}, "type": {c.posted}, "tags": {"kept"}, "since": {"3d"},
		})
		if got := mustFeed(t, db, c.name).Type; got != c.want {
			t.Errorf("%s: save left type %q, want %q", c.name, got, c.want)
		}
	}

	// Switching is allowed both ways.
	id := feedID(t, db, "arXiv")
	postForm(t, w, "/sources/feed/"+id, url.Values{
		"name": {"arXiv"}, "url": {"https://export.arxiv.org/api/query?search_query=all:rag"}, "type": {"rss"},
	})
	if got := mustFeed(t, db, "arXiv").Type; got != "" {
		t.Errorf("switching to rss stored %q, want unset", got)
	}
}

func mustFeed(t *testing.T, db *store.Store, name string) config.Feed {
	t.Helper()
	f, err := db.FeedByID(t.Context(), feedID(t, db, name))
	if err != nil {
		t.Fatalf("FeedByID: %v", err)
	}
	return f
}

// postFile sends body as the multipart "file" field of an import.
func postFile(t *testing.T, w *Web, body string) string {
	t.Helper()
	return postFileRec(t, w, body).Body.String()
}

func postFileRec(t *testing.T, w *Web, body string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", "feeds.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/sources/import", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /sources/import: status = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	return rec
}

const importFeeds = `feeds:
  - name: Gamma
    url: https://gamma.test/feed
  - name: Delta
    url: https://delta.test/feed
  - name: Broken
    url: ftp://nope.test/feed
`

func TestSourcesImportReportsCounts(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{
		Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}},
	})
	out := postFile(t, w, importFeeds)
	if !strings.Contains(out, `role="status"`) ||
		!strings.Contains(out, "added 2 feed(s), skipped 1 already known (including deleted ones)") {
		t.Errorf("status line missing or wrong; body=%s", out)
	}
	if !strings.Contains(out, "feed 2:") {
		t.Errorf("the skipped entry's warning is missing; body=%s", out)
	}
	if !strings.Contains(out, "Gamma") || !strings.Contains(out, `id="srcBody"`) {
		t.Errorf("the repainted list lacks the new feed; body=%s", out)
	}
	if got := len(mustFeeds(t, db)); got != 3 {
		t.Errorf("feeds = %d, want 3", got)
	}

	again := postFile(t, w, importFeeds)
	if !strings.Contains(again, "added 0 feed(s), skipped 3 already known") {
		t.Errorf("second import; body=%s", again)
	}
}

func TestSourcesImportCarriesTheFeedCount(t *testing.T) {
	w, _ := sourcesTestWeb(t, config.FeedsDoc{
		Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}},
	})
	if got := postFileRec(t, w, importFeeds).Header().Get(feedCountHeader); got != "3" {
		t.Errorf("%s = %q, want 3", feedCountHeader, got)
	}
}

func TestSourcesImportSkipsDeletedFeeds(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{})
	postFile(t, w, importFeeds)
	req := httptest.NewRequest(http.MethodDelete, "/sources/feed/"+feedID(t, db, "Gamma"), nil)
	w.Routes().ServeHTTP(httptest.NewRecorder(), req)
	out := postFile(t, w, importFeeds)
	if !strings.Contains(out, "added 0 feed(s)") {
		t.Errorf("a deleted feed came back; body=%s", out)
	}
}

func TestSourcesImportRejectionsRenderInline(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{})
	cases := map[string]struct{ body, want string }{
		"broken yaml":    {"feeds: [unclosed\n", "parse feeds file"},
		"empty":          {"# nothing here\n", "not a feeds file: no feeds or sources list"},
		"config shaped":  {"inference:\n  provider: ollama\n", "not a feeds file: no feeds or sources list"},
		"oversize":       {"feeds: []\n# " + strings.Repeat("x", sourceImportLimit), "too large"},
		"no feeds field": {"", "not a feeds file"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			out := postFile(t, w, c.body)
			if !strings.Contains(out, `role="alert"`) || !strings.Contains(out, c.want) {
				t.Errorf("want an inline alert with %q; body=%s", c.want, out)
			}
		})
	}
	if got := len(mustFeeds(t, db)); got != 0 {
		t.Errorf("a rejected file added %d feeds", got)
	}
}

// settings.js reads these classes from the import response: src__import for
// the status line (both outcomes) and sdet__err for the error line.
func TestSourcesImportResponseKeepsSettingsHooks(t *testing.T) {
	w, _ := sourcesTestWeb(t, config.FeedsDoc{})
	ok := postFile(t, w, importFeeds)
	if !strings.Contains(ok, "src__import") {
		t.Errorf("valid import lacks src__import; body=%s", ok)
	}
	if strings.Contains(ok, "sdet__err") {
		t.Errorf("valid import carries an error block; body=%s", ok)
	}
	bad := postFile(t, w, "feeds: [")
	for _, class := range []string{"src__import", "sdet__err"} {
		if !strings.Contains(bad, class) {
			t.Errorf("invalid import lacks %s; body=%s", class, bad)
		}
	}
}

func TestSourceCountsSummary(t *testing.T) {
	cases := []struct {
		name string
		in   sourceCounts
		want string
	}{
		{"none", sourceCounts{}, "No sources yet"},
		{"only removed", sourceCounts{Deleted: 2}, "No sources yet"},
		{"one healthy", sourceCounts{Total: 1, Enabled: 1}, "1 source · 1 enabled · none failing"},
		{"parked", sourceCounts{Total: 3, Enabled: 2}, "3 sources · 2 enabled · none failing"},
		{"failing", sourceCounts{Total: 5, Enabled: 5, Failing: 2}, "5 sources · 5 enabled · 2 failing"},
		{
			"failing and flaky",
			sourceCounts{Total: 5, Enabled: 4, Failing: 1, Flaky: 2},
			"5 sources · 4 enabled · 1 failing · 2 flaky",
		},
		{"flaky only", sourceCounts{Total: 2, Enabled: 2, Flaky: 1}, "2 sources · 2 enabled · none failing · 1 flaky"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.in.Summary(); got != c.want {
				t.Errorf("Summary() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestSourceCountsFilters(t *testing.T) {
	got := sourceCounts{Total: 5, Enabled: 3, Failing: 1, Flaky: 0, Deleted: 2}.Filters()
	want := []struct {
		key   string
		count int
		zero  bool
	}{
		{"all", 5, false}, {"enabled", 3, false}, {"disabled", 2, false},
		{"failing", 1, false}, {"flaky", 0, true}, {"removed", 2, false},
	}
	if len(got) != len(want) {
		t.Fatalf("%d pills, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Key != w.key || got[i].Count != w.count || got[i].Zero() != w.zero {
			t.Errorf("pill %d = %+v (zero %v), want %+v", i, got[i], got[i].Zero(), w)
		}
	}
	if (sourceCounts{}).Filters()[0].Zero() {
		t.Error("the All pill is dimmed with no sources")
	}
}

func TestMissesPhrase(t *testing.T) {
	for need, want := range map[int]string{
		0: "One more miss", 1: "One more miss", 2: "Two more misses", 3: "3 more misses",
	} {
		if got := missesPhrase(need); got != want {
			t.Errorf("missesPhrase(%d) = %q, want %q", need, got, want)
		}
	}
}

// The chip colour is computed twice, here and in static/js/tagput.js; the two
// must agree or a chip would change colour when the script runs.
func TestTagColorMatchesTheScriptHash(t *testing.T) {
	for name, want := range map[string]int{"ai": 7, "Go": 3, "machine learning": 2, "é": 6} {
		if got := tagColor(name); got != want {
			t.Errorf("tagColor(%q) = %d, want %d", name, got, want)
		}
	}
}

// recordFailures stores n failed fetches for feed, newest last, with err.
func recordFailures(t *testing.T, db *store.Store, feed config.Feed, n int, err string) {
	t.Helper()
	id, now := config.FeedID(feed.URL), time.Now()
	var attempts []store.FeedFetch
	for i := range n {
		attempts = append(attempts, store.FeedFetch{
			FeedID: id, Name: feed.Name, Error: err, At: now.Add(time.Duration(i-n) * time.Minute),
		})
	}
	if e := db.RecordFeedFetches(t.Context(), attempts); e != nil {
		t.Fatalf("RecordFeedFetches: %v", e)
	}
}

// A failing source gets a card with its streak, raw error, plain-language
// reason and the three things to do about it; the Failing pill and the nav
// badge carry the same count.
func TestSourcesAttentionCardsFailingSources(t *testing.T) {
	broken := config.Feed{Name: "Broken", URL: "https://broken.test/feed"}
	fine := config.Feed{Name: "Fine", URL: "https://fine.test/feed"}
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{broken, fine}})
	recordFailures(t, db, broken, 4, "GET https://broken.test/feed: 404 Not Found")
	id := feedID(t, db, "Broken")

	out := get(t, w, "/sources/manage")
	for _, want := range []string{
		`<section class="mg-attn" aria-label="Needs attention">`,
		`aria-expanded="true" aria-controls="mg-attn-body"`,
		`<span class="mg-chip mg-chip--bad">1 failing</span>`,
		`<span class="mg-attn__names">Broken</span>`,
		`&times;4`,
		"failing since ",
		`>GET https://broken.test/feed: 404 Not Found</code>`,
		"The site says this address does not exist.",
		`>Edit URL</button>`,
		`>Disable</button>`,
		`>Remove</button>`,
		"Re-run ingestion for all 2 enabled feeds",
		`Failing <i class="mg-n">1</i>`,
		`title="1 failing"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q; body=%s", want, out)
		}
	}
	wantAttrs(t, "Edit URL", tagWith(t, out, "aria-label", "Edit the URL of Broken"), map[string]string{
		"data-src-reveal": id, "data-src-focus": "url",
	})
	wantAttrs(t, "Disable", tagWith(t, out, "aria-label", "Disable Broken"), map[string]string{
		"hx-post": "/sources/feed/" + id + "/enabled", "hx-target": "#src-" + id, "hx-swap": "outerHTML",
		"hx-vals": `{"enabled":"off"}`,
	})
	wantAttrs(t, "Remove", tagWith(t, out, "aria-label", "Remove Broken"), map[string]string{
		"hx-get": "/sources/feed/" + id + "/confirm-delete", "hx-target": "#modalTop", "hx-swap": "innerHTML",
	})
	if strings.Contains(out, "Nothing failing") {
		t.Error("the all-clear line is shown beside a failing source")
	}
}

// Disabling the failing source from its card repaints the status block in the
// same response, so the card goes without a reload.
func TestSourcesAttentionFollowsAMutation(t *testing.T) {
	broken := config.Feed{Name: "Broken", URL: "https://broken.test/feed"}
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{broken}})
	recordFailures(t, db, broken, 3, "dial tcp: no such host")

	body := postForm(t, w, "/sources/feed/"+feedID(t, db, "Broken")+"/enabled", url.Values{"enabled": {"off"}})
	if !strings.Contains(body, `id="srcAttn" hx-swap-oob="true"`) {
		t.Fatalf("no out-of-band status block; body=%s", body)
	}
	if !strings.Contains(body, "Nothing failing.") || strings.Contains(body, "mg-attn__toggle") {
		t.Errorf("the status block still shows the disabled source as failing; body=%s", body)
	}
	if !strings.Contains(body, `Failing <i class="mg-n">0</i>`) {
		t.Errorf("the Failing pill did not follow; body=%s", body)
	}
}

// With nothing failing the block is one line, followed by a note per flaky
// source saying how close it is to failing.
func TestSourcesAttentionNothingFailing(t *testing.T) {
	flaky := config.Feed{Name: "Shaky", URL: "https://shaky.test/feed"}
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{flaky}})

	out := get(t, w, "/sources/manage")
	if !strings.Contains(out, "Nothing failing. No ingestion run yet.") {
		t.Errorf("missing the all-clear line; body=%s", out)
	}
	if strings.Contains(out, "mg-flaky") {
		t.Errorf("a flaky note for a source that has not failed; body=%s", out)
	}

	recordFailures(t, db, flaky, 1, "503 unavailable")
	out = get(t, w, "/sources/manage")
	if !strings.Contains(
		out,
		"<b>Shaky</b> missed its last fetch. Two more misses in a row and it counts as failing.",
	) {
		t.Errorf("missing the flaky note; body=%s", out)
	}
	if strings.Contains(out, "mg-attn__toggle") {
		t.Errorf("a flaky source opened the attention block; body=%s", out)
	}
}

// Toggling answers with the source's unit. The editor is left closed unless the
// request says it was open, so the click does not discard what is being typed.
func TestSourcesToggleKeepsTheEditorOnlyWhenOpen(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{
		Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}},
	})
	id := feedID(t, db, "Alpha")
	body := postForm(t, w, "/sources/feed/"+id+"/enabled", url.Values{"enabled": {"off"}})
	if strings.Contains(body, `class="mg-ed"`) {
		t.Errorf("toggling opened the editor; body=%s", body)
	}
	if !strings.Contains(body, `id="src-`+id+`"`) || strings.Contains(body, `id="srcBody"`) {
		t.Errorf("a toggle did not answer with the unit alone; body=%s", body)
	}
	body = postForm(t, w, "/sources/feed/"+id+"/enabled", url.Values{"enabled": {"on"}, "open": {"1"}})
	if !strings.Contains(body, `<form class="mg-ed" id="ed-`+id+`"`) {
		t.Errorf("toggling from an open editor closed it; body=%s", body)
	}
}

// What one row carries: identity, history strip, first tag and a count of the
// rest, the fetch switch and an Edit button.
func TestSourcesRowCells(t *testing.T) {
	feed := config.Feed{
		Name: "Alpha",
		URL:  "https://www.alpha.test/feed",
		Type: config.FeedTypeAcademic,
		Tags: []string{"ai", "go", "news"},
	}
	w, db := sourcesTestWeb(
		t,
		config.FeedsDoc{Feeds: []config.Feed{feed, {Name: "Bare", URL: "https://bare.test/feed"}}},
	)
	recordFailures(t, db, feed, 2, "503 unavailable")
	id := feedID(t, db, "Alpha")

	out := get(t, w, "/sources/manage")
	for _, want := range []string{
		`<div class="mg-item" id="src-` + id + `"`,
		`data-tags="ai,go,news"`,
		`mg-row is-flaky`,
		`class="fav fav--mono fav--c`,
		`>alpha.test · academic</span>`,
		`title="https://www.alpha.test/feed"`,
		`aria-label="Last 12 fetches, newest on the right: 0 ok, 2 failed"`,
		`hs__t hs__t--flaky`,
		`<span class="mg-state mg-state--warn">Flaky ×2</span>`,
		`<span class="tag tag--clip tag--c7" data-tag="ai" title="ai">ai</span>`,
		`<span class="tag tag--more" title="go, news">+2</span>`,
		`role="switch" aria-checked="true" aria-label="Fetch Alpha"`,
		`aria-label="Edit Alpha"`,
		`<div class="mg-hrow">`,
		`<span class="sh-hint">none</span>`,
		`<span class="hs hs--none">Waiting for first run</span>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q; body=%s", want, out)
		}
	}
}

// A long raw error is cut in the card, with the whole text in its tooltip.
func TestSourcesAttentionClipsALongError(t *testing.T) {
	broken := config.Feed{Name: "Broken", URL: "https://broken.test/feed"}
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{broken}})
	long := "dial tcp: " + strings.Repeat("a long reason ", 40)
	recordFailures(t, db, broken, 3, long)

	out := get(t, w, "/sources/manage")
	_, rest, _ := strings.Cut(out, `<code class="mg-card__err"`)
	code, _, _ := strings.Cut(rest, "</code>")
	_, shown, _ := strings.Cut(code, ">")
	if n := len([]rune(shown)); n > maxErrorText+1 || !strings.HasSuffix(shown, "…") {
		t.Errorf("the card shows %d characters, want at most %d ending in an ellipsis", n, maxErrorText+1)
	}
	if !strings.Contains(code, `title="`+strings.TrimSpace(long)) {
		t.Errorf("the whole error is not in the tooltip: %.120s", code)
	}
}
