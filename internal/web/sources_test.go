// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
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

// sourcesTestWeb builds a Web over a store seeded with doc, so the dialog is
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
	cfg := &config.Config{
		Ingest: config.IngestConfig{Since: config.Duration(globalSince), Feeds: "/tmp/feeds.yaml"},
	}
	return New(db, cfg, ":8080", "", testIngestManager(t, db)), db
}

// feedID looks up the stored ID for a feed by name, which is how the dialog
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

// The dialog's job is showing resolved values and where they came from.
func TestSourcesRendersResolvedValues(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{
		Defaults: config.FeedDefaults{MaxItems: intPtr(5), Tags: []string{"all"}},
		Feeds: []config.Feed{
			{Name: "Alpha", URL: "https://alpha.test/feed", Since: durPtr(48 * time.Hour), Tags: []string{"ai"}},
			{Name: "Beta", URL: "https://beta.test/feed"},
			{Name: "Parked", URL: "https://parked.test/feed", Enabled: boolPtr(false)},
		},
	})
	list := get(t, w, "/sources")
	detail := get(t, w, "/sources/"+feedID(t, db, "Beta"))

	cases := []struct {
		name string
		in   string
		want string
		why  string
	}{
		{name: "total count", in: list, want: "--feeds 3", why: "parked feeds still count"},
		{name: "enabled count", in: list, want: "--enabled 2", why: "one of the three is parked"},
		{name: "parked feed listed", in: list, want: "Parked", why: "disabled feeds are shown, not hidden"},
		{name: "parked dot", in: list, want: "srow__dot--off", why: "parked renders as its own state"},
		{name: "never fetched", in: list, want: "not fetched yet", why: "no history recorded yet"},
		{
			name: "search data",
			in:   list,
			want: `data-url="https://alpha.test/feed"`,
			why:  "the client filter matches on the url",
		},
		{
			name: "global since as the placeholder",
			in:   detail,
			want: `placeholder="9d"`,
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

// The tally is rendered twice — inline in the bar and again out of band by
// every mutation — so the dialog must carry exactly one of it. Two would mean a
// duplicate id and a stray visible copy below the columns.
func TestSourcesCountsRenderOnce(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{
		Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}},
	})
	if n := strings.Count(get(t, w, "/sources"), `id="srcCmd"`); n != 1 {
		t.Errorf("the counts element appears %d times in the dialog, want 1", n)
	}
	// A mutation response carries it as an out-of-band update to that element.
	body := postForm(t, w, "/sources/"+feedID(t, db, "Alpha")+"/enabled", url.Values{"enabled": {"off"}})
	if !strings.Contains(body, `hx-swap-oob="true"`) {
		t.Errorf("a mutation did not carry the out-of-band tally; body=%s", body)
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
	out := get(t, w, "/sources/"+feedID(t, db, "Beta"))

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
	list := get(t, w, "/sources")
	detail := get(t, w, "/sources/"+feedID(t, db, "Broken"))

	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "ok dot", in: list, want: "srow__dot--ok"},
		{name: "error dot", in: list, want: "srow__dot--error"},
		{name: "failing count", in: list, want: "--failing 1"},
		{name: "error surfaced in the pane", in: detail, want: "dial tcp: no such host"},
		{name: "history strip", in: detail, want: "sdet__tick"},
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
	out := get(t, w, "/sources")

	if !strings.Contains(out, "srow__dot--off") {
		t.Errorf("expected the parked dot; body=%s", out)
	}
	if strings.Contains(out, "--failing") {
		t.Errorf("a disabled feed must not count as failing; body=%s", out)
	}
	// Its history is still there to look back at.
	if !strings.Contains(get(t, w, "/sources/"+feedID(t, db, "Parked")), "sdet__tick") {
		t.Error("parked feed lost its history strip")
	}
}

func TestSourcesToggleEnabled(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{
		Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}},
	})
	id := feedID(t, db, "Alpha")

	postForm(t, w, "/sources/"+id+"/enabled", url.Values{"enabled": {"off"}})
	feeds := mustFeeds(t, db)
	if feeds[0].Enabled == nil || *feeds[0].Enabled {
		t.Fatalf("feed was not disabled: %+v", feeds[0].Enabled)
	}

	out := postForm(t, w, "/sources/"+id+"/enabled", url.Values{"enabled": {"on"}})
	feeds = mustFeeds(t, db)
	if feeds[0].Enabled == nil || !*feeds[0].Enabled {
		t.Fatalf("feed was not re-enabled: %+v", feeds[0].Enabled)
	}
	if !strings.Contains(out, "--enabled 1") {
		t.Errorf("the command's count did not follow the toggle; body=%s", out)
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
	w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/sources/"+id, nil))
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
	if !strings.Contains(get(t, w, "/sources"), "restore") {
		t.Error("a deleted feed offers no way back")
	}

	postForm(t, w, "/sources/"+id+"/restore", nil)
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

	if !strings.Contains(body, `hx-post="/sources"`) {
		t.Errorf("the add form did not come back; body=%s", body)
	}
	if !strings.Contains(body, "Add Feed") {
		t.Errorf("the form is not titled as an add; body=%s", body)
	}
	// A flash, not the persistent notice bar: it clears itself, and being laid
	// over the pane it costs no height — as an inline row it scrolled the form.
	// The name sits in its own span so it can ellipsise on its own.
	if !strings.Contains(body, `class="sdet__flash"`) ||
		!strings.Contains(body, `sdet__flashname">Added<`) {
		t.Errorf("no self-clearing confirmation naming the feed created; body=%s", body)
	}
	if strings.Contains(body, `class="sdet__ok"`) {
		t.Errorf("the confirmation rendered as a bar that would stay put; body=%s", body)
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
func TestSourcesSaveFlashesSaved(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{
		Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}},
	})
	body := postForm(t, w, "/sources/"+feedID(t, db, "Alpha"), url.Values{
		"name": {"Alpha"}, "url": {"https://alpha.test/feed"},
	})

	if !strings.Contains(body, "&#10003; saved") {
		t.Errorf("a save gave no confirmation; body=%s", body)
	}
	if strings.Contains(body, "sdet__flash--bad") {
		t.Errorf("a save flashed as a removal; body=%s", body)
	}
	if !strings.Contains(body, `sdet__flashname">Alpha<`) {
		t.Errorf("the confirmation did not name the feed; body=%s", body)
	}
}

// Deleting asks first, in a dialog stacked over Sources rather than a second
// button under the pointer that just clicked the first one.
func TestSourcesDeleteAsksBeforeItActs(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{
		Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}},
	})
	id := feedID(t, db, "Alpha")

	ask := get(t, w, "/sources/"+id+"/confirm-delete")
	if !strings.Contains(ask, `class="modal"`) || !strings.Contains(ask, "Alpha") {
		t.Errorf("no dialog naming the feed; body=%s", ask)
	}
	if !strings.Contains(ask, `hx-delete="/sources/`+id+`"`) {
		t.Errorf("the dialog does not carry the delete; body=%s", ask)
	}
	// Asking must not act.
	if len(mustFeeds(t, db)) != 1 {
		t.Fatal("opening the confirmation deleted the feed")
	}

	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/sources/"+id, nil))
	body := rec.Body.String()

	if !strings.Contains(body, `hx-swap-oob="innerHTML:#modalTop"`) {
		t.Errorf("the confirmation dialog was left open; body=%s", body)
	}
	if !strings.Contains(body, "sdet__flash--bad") || !strings.Contains(body, "&#10007; deleted") {
		t.Errorf("no removal confirmation; body=%s", body)
	}
	// It names what went — after the delete the resolved set no longer has it,
	// so the handler has to read the feed before removing it.
	if !strings.Contains(body, `sdet__flashname">Alpha<`) {
		t.Errorf("the confirmation did not name what was deleted; body=%s", body)
	}
}

// Renaming has to carry the feed's existing items with it: items.source records
// the feed's name, so otherwise they'd be filed under a label nothing claims.
func TestSourcesRenameRefilesItems(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{
		Feeds: []config.Feed{{Name: "Old Name", URL: "https://alpha.test/feed"}},
	})
	seedItem(t, db, "Old Name")

	out := postForm(t, w, "/sources/"+feedID(t, db, "Old Name"), url.Values{
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

	postForm(t, w, "/sources/"+id, url.Values{
		"name": {"Alpha"}, "url": {"https://alpha.test/feed.xml"},
	})
	if got := feedID(t, db, "Alpha"); got != id {
		t.Errorf("the feed's ID changed with its URL: %q -> %q", id, got)
	}
	if !strings.Contains(get(t, w, "/sources/"+id), "4 items") {
		t.Error("the feed lost its history when its URL changed")
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
	out := get(t, w, "/sources/"+feedID(t, db, "Alpha"))
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

// The command's optional flags only appear when they have something to say, so
// a healthy page isn't padded with zeroes.
func TestSourcesCommandOmitsEmptyFlags(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{
		Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}},
	})
	out := get(t, w, "/sources")
	if strings.Contains(out, "--failing") || strings.Contains(out, "--deleted") {
		t.Errorf("a healthy page printed an empty flag; body=%s", out)
	}

	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/sources/"+feedID(t, db, "Alpha"), nil))

	if !strings.Contains(get(t, w, "/sources"), "--deleted 1") {
		t.Error("--deleted missing after a delete")
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
	w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/sources/"+feedID(t, db, "Alpha"), nil))
	body := rec.Body.String()

	if !strings.Contains(body, `data-state="gone"`) {
		t.Errorf("the deleted feed left the list instead of being marked; body=%s", body)
	}
	if !strings.Contains(body, "restore") {
		t.Errorf("the delete response offers no way back; body=%s", body)
	}
	if !strings.Contains(body, "--deleted 1") {
		t.Errorf("the tally did not follow the delete; body=%s", body)
	}
}

// The state filter is a single popmenu, and every state it offers is one the
// client-side script can serve — including "deleted", whose rows are in the
// list like the rest.
func TestSourcesStateFilterIsSelfContained(t *testing.T) {
	w, _ := sourcesTestWeb(t, config.FeedsDoc{
		Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}},
	})
	out := get(t, w, "/sources")

	for _, want := range []string{`value="all"`, `value="on"`, `value="off"`, `value="gone"`} {
		if !strings.Contains(out, want) {
			t.Errorf("the state filter is missing %s; body=%s", want, out)
		}
	}
	// A filter that navigated would drop whatever the search box was holding.
	if strings.Contains(out, "?deleted=1") {
		t.Errorf("the deleted filter still navigates; body=%s", out)
	}
	// The count is unconditional, so changing the filter can't resize the bar.
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
	if !strings.Contains(get(t, w, "/sources/"+feedID(t, db, "Plain")), "data-src-insecure") {
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

// Sources answers with a dialog fragment, not a page. Anything else and it
// would land a second <html> inside the modal layer.
func TestSourcesRendersAsAFragment(t *testing.T) {
	w, _ := sourcesTestWeb(t, config.FeedsDoc{
		Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}},
	})
	out := get(t, w, "/sources")

	if strings.Contains(out, "<!doctype") || strings.Contains(out, "<html") {
		t.Errorf("the dialog carried a whole page with it; body=%s", out)
	}
	if !strings.Contains(out, `class="modal__frame modal__frame--sources"`) {
		t.Errorf("no dialog frame; body=%s", out)
	}
	// Its own two dialogs stack above it, or opening one would close Sources.
	for _, want := range []string{
		`hx-get="/sources/defaults" hx-target="#modalTop"`,
		`hx-get="/sources/export" hx-target="#modalTop"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q; body=%s", want, out)
		}
	}
}

// The pager is inert markup the client script drives, so the only server-side
// contract is that it ships with the list and starts hidden — a pager visible
// before the script has counted anything would flash on every open.
func TestSourcesShipsThePagerMarkup(t *testing.T) {
	w, _ := sourcesTestWeb(t, config.FeedsDoc{
		Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}},
	})
	out := get(t, w, "/sources")

	cases := []struct {
		name string
		want string
		why  string
	}{
		{
			name: "arrows hidden until counted", want: "data-src-pagenav hidden",
			why: "they must not flash before the script knows how many pages there are",
		},
		{name: "step back", want: `data-src-page="-1"`, why: "the previous-page arrow"},
		{name: "step forward", want: `data-src-page="1"`, why: "the next-page arrow"},
		{name: "range label", want: "data-src-pagelbl", why: "where the script writes 1-10 of 24"},
		{
			name: "footer clears the names", want: "modal__foot--sources",
			why: "left-aligned it read as another feed row",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !strings.Contains(out, c.want) {
				t.Errorf("missing %q (%s); body=%s", c.want, c.why, out)
			}
		})
	}

	// The size picker is always reachable, even on a set small enough that the
	// arrows are hidden — otherwise raising it from 10 would need 11 feeds first.
	t.Run("every page size offered", func(t *testing.T) {
		for _, size := range []string{"10", "15", "20"} {
			if !strings.Contains(out, `data-src-per="`+size+`"`) {
				t.Errorf("no %s-per-page control; body=%s", size, out)
			}
		}
	})
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
	out := get(t, w, "/sources")

	if !strings.Contains(out, "No feeds yet") {
		t.Errorf("an empty store should render a zero-state; body=%s", out)
	}
	if !strings.Contains(out, "--feeds 0") {
		t.Errorf("expected a zero count; body=%s", out)
	}
}

func TestSourcesMissingFeedIs404(t *testing.T) {
	w, _ := sourcesTestWeb(t, config.FeedsDoc{
		Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/sources/nope", strings.NewReader("name=x&url=https://x.test/f"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", rec.Code, rec.Body)
	}
}
