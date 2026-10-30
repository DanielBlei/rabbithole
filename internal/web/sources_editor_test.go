// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// deleteSource issues the confirmed DELETE and returns the response body.
func deleteSource(t *testing.T, w *Web, id string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/sources/feed/"+id, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE: status = %d; body=%s", rec.Code, rec.Body)
	}
	return rec.Body.String()
}

// Opening a source answers with its unit, editor open, aimed at that unit alone.
func TestSourcesEditorOpensInsideItsUnit(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{
		{Name: "Alpha", URL: "https://alpha.test/feed", Tags: []string{"ai"}},
		{Name: "Beta", URL: "https://beta.test/feed"},
	}})
	id := feedID(t, db, "Alpha")
	out := get(t, w, "/sources/feed/"+id)

	wantAttrs(t, "unit", tagWith(t, out, "id", "src-"+id), map[string]string{"class": "mg-item", "data-name": "Alpha"})
	wantAttrs(t, "close button", tagWith(t, out, "aria-label", "Close editor for Alpha"), map[string]string{
		"aria-expanded": "true", "aria-controls": "ed-" + id, "data-src-close": present,
	})
	wantAttrs(t, "form", tagWith(t, out, "id", "ed-"+id), map[string]string{
		"class": "mg-ed", "role": "region", "aria-label": "Edit Alpha",
		"hx-post": "/sources/feed/" + id, "hx-target": "#src-" + id, "hx-swap": "outerHTML",
	})
	wantAttrs(
		t,
		"name field",
		tagWith(t, out, "id", "ed-name-"+id),
		map[string]string{"name": "name", "value": "Alpha"},
	)
	wantAttrs(t, "tags field", tagWith(t, out, "name", "tags"), map[string]string{"type": "hidden", "value": "ai"})
	wantAttrs(t, "reset", tagWith(t, out, "data-src-reset", ""), map[string]string{
		"hx-get": "/sources/feed/" + id, "hx-target": "#src-" + id, "disabled": present,
	})
	for _, want := range []string{`<span>On</span>`, `<span>Off</span>`, `<span>Default</span>`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q; body=%s", want, out)
		}
	}
	wantAttrs(t, "remove", tagWith(t, out, "hx-delete", "/sources/feed/"+id), map[string]string{
		"hx-target": "#src-" + id, "hx-swap": "outerHTML", "type": "button", "data-src-remove": present,
	})
	// One unit, not the page, and no pane beside the list.
	for _, bad := range []string{`id="srcBody"`, `id="src-` + feedID(t, db, "Beta") + `"`, "src__cols--detail", "srcDetail"} {
		if strings.Contains(out, bad) {
			t.Errorf("a one-source answer carries %q; body=%s", bad, out)
		}
	}
	// A closed row offers Edit, aimed at the unit.
	list := get(t, w, "/sources/manage")
	wantAttrs(t, "edit button", tagWith(t, list, "aria-label", "Edit Alpha"), map[string]string{
		"aria-expanded": "false", "aria-controls": "ed-" + id, "data-src-open": present,
		"hx-get": "/sources/feed/" + id, "hx-target": "#src-" + id, "hx-swap": "outerHTML",
	})
	if strings.Contains(list, `class="mg-ed"`) {
		t.Errorf("a list render opened an editor; body=%s", list)
	}
}

// The ?feed= deep link renders the list with that source's editor open.
func TestSourcesDeepLinkRendersTheUnitOpen(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{
		{Name: "Alpha", URL: "https://alpha.test/feed"},
		{Name: "Beta", URL: "https://beta.test/feed"},
	}})
	id := feedID(t, db, "Beta")
	out := get(t, w, "/sources/manage?feed="+id)

	if n := strings.Count(out, `class="mg-ed"`); n != 1 {
		t.Fatalf("%d editors open, want 1; body=%s", n, out)
	}
	unit := out[strings.Index(out, `id="src-`+id+`"`):]
	if !strings.Contains(unit, `<span data-src-hint-t>7d built-in</span>`) ||
		!strings.Contains(unit, `placeholder="7d"`) {
		t.Errorf("the deep-linked editor has no fallbacks stamped; body=%s", out)
	}
	if !strings.Contains(unit, `mg-row is-open`) || !strings.Contains(unit, `<form class="mg-ed" id="ed-`+id+`"`) {
		t.Errorf("the deep-linked unit is not open; body=%s", out)
	}
}

// A validation failure keeps the editor open in that unit, with the backend's
// message verbatim and what was typed still in the fields. The row keeps the
// stored values.
func TestSourcesSaveErrorsStayInTheEditor(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{
		{Name: "Alpha", URL: "https://alpha.test/feed"},
		{Name: "Beta", URL: "https://beta.test/feed"},
	}})
	id := feedID(t, db, "Alpha")

	cases := []struct {
		name string
		form url.Values
		want string
	}{
		{
			name: "bad since",
			form: url.Values{"name": {"Alpha"}, "url": {"https://alpha.test/feed"}, "since": {"2w"}},
			want: `invalid duration "2w": use a 'd' or 'h' suffix (e.g. 14d, 168h)`,
		},
		{
			name: "name taken",
			form: url.Values{"name": {"Beta"}, "url": {"https://alpha.test/feed"}},
			want: store.ErrFeedNameTaken.Error(),
		},
		{
			name: "url taken",
			form: url.Values{"name": {"Alpha"}, "url": {"https://beta.test/feed"}},
			want: store.ErrFeedURLTaken.Error(),
		},
		{
			name: "bad max items",
			form: url.Values{"name": {"Alpha"}, "url": {"https://alpha.test/feed"}, "max_items": {"lots"}},
			want: `max_items must be a whole number, got "lots"`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := postForm(t, w, "/sources/feed/"+id, c.form)
			if !strings.Contains(out, `<p class="sdet__err mg-ed__msg" role="alert">`) ||
				!strings.Contains(out, html.EscapeString(c.want)) {
				t.Errorf("the backend message %q is not in the editor; body=%s", c.want, out)
			}
			if !strings.Contains(out, `<form class="mg-ed" id="ed-`+id+`"`) {
				t.Errorf("the editor closed on an error; body=%s", out)
			}
			if strings.Contains(out, `id="srcBody"`) || strings.Contains(out, `hx-swap-oob`) {
				t.Errorf("an error answer touched more than the unit; body=%s", out)
			}
			if !strings.Contains(out, `data-name="Alpha"`) {
				t.Errorf("the row lost its stored name; body=%s", out)
			}
		})
	}
	// What was typed comes back in the fields.
	out := postForm(t, w, "/sources/feed/"+id, url.Values{
		"name": {"Renamed"}, "url": {"https://alpha.test/feed"}, "since": {"2w"},
	})
	if !strings.Contains(out, `name="name" value="Renamed"`) || !strings.Contains(out, `name="since" value="2w"`) {
		t.Errorf("the editor lost what was typed; body=%s", out)
	}
	if got := mustFeed(t, db, "Alpha"); got.Name != "Alpha" {
		t.Errorf("a rejected save changed the feed: %+v", got)
	}
}

// A rename is saved and re-files the items; the editor says so.
func TestSourcesSaveRenameNotice(t *testing.T) {
	w, db := sourcesTestWeb(
		t,
		config.FeedsDoc{Feeds: []config.Feed{{Name: "Old Name", URL: "https://alpha.test/feed"}}},
	)
	seedItem(t, db, "Old Name")
	id := feedID(t, db, "Old Name")

	out := postForm(t, w, "/sources/feed/"+id, url.Values{"name": {"New Name"}, "url": {"https://alpha.test/feed"}})
	if !strings.Contains(out, `data-src-notice="">Saved: renamed · re-filed 1 item</p>`) {
		t.Errorf("no rename notice; body=%s", out)
	}
	if !strings.Contains(out, `data-name="New Name"`) {
		t.Errorf("the row did not take the new name; body=%s", out)
	}
	if !strings.Contains(out, `aria-label="Close editor for New Name"`) {
		t.Errorf("the editor closed after the save; body=%s", out)
	}
}

// The Fetching pills are On, Off and Default: Default stores no flag, so the
// source keeps following the defaults, and On and Off store explicit values.
func TestSourcesFetchingPillsRoundTrip(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}}})
	id := feedID(t, db, "Alpha")
	save := func(enabled string) string {
		return postForm(t, w, "/sources/feed/"+id, url.Values{
			"name": {"Alpha"}, "url": {"https://alpha.test/feed"}, "enabled": {enabled},
		})
	}
	checked := func(out, value string) bool {
		return strings.Contains(out, `name="enabled" value="`+value+`" checked>`)
	}

	if out := get(t, w, "/sources/feed/"+id); !checked(out, "") {
		t.Errorf("a source with no flag does not show Default; body=%s", out)
	}
	if out := save(""); mustFeed(t, db, "Alpha").Enabled != nil || !checked(out, "") {
		t.Errorf("Default stored a flag, or the pill moved")
	}
	out := save("on")
	if got := mustFeed(t, db, "Alpha").Enabled; got == nil || !*got || !checked(out, "on") {
		t.Errorf("On did not store an explicit true: %v", got)
	}
	out = save("off")
	if got := mustFeed(t, db, "Alpha").Enabled; got == nil || *got || !checked(out, "off") {
		t.Errorf("Off did not store an explicit false: %v", got)
	}
	out = save("")
	if got := mustFeed(t, db, "Alpha").Enabled; got != nil || !checked(out, "") {
		t.Errorf("switching back to Default left a flag: %v", got)
	}
}

// The Default pill says what it follows, from the defaults as they stand.
func TestSourcesDefaultPillHint(t *testing.T) {
	for _, c := range []struct {
		name     string
		defaults config.FeedDefaults
		want     string
	}{
		{"built-in on", config.FeedDefaults{}, "Default: follows your defaults (currently on)."},
		{"defaults off", config.FeedDefaults{Enabled: boolPtr(false)}, "Default: follows your defaults (currently off)."},
		{"defaults on", config.FeedDefaults{Enabled: boolPtr(true)}, "Default: follows your defaults (currently on)."},
	} {
		t.Run(c.name, func(t *testing.T) {
			w, db := sourcesTestWeb(t, config.FeedsDoc{
				Defaults: c.defaults, Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}},
			})
			if out := get(t, w, "/sources/feed/"+feedID(t, db, "Alpha")); !strings.Contains(out, c.want) {
				t.Errorf("missing %q; body=%s", c.want, out)
			}
		})
	}
}

// The add form follows the defaults: with them off, a source added without
// touching Fetching stores no flag and is not fetched.
func TestSourcesAddFollowsTheDefaults(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{
		Defaults: config.FeedDefaults{Enabled: boolPtr(false)},
		Feeds:    []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}},
	})
	form := get(t, w, "/sources/new")
	if !strings.Contains(form, `name="enabled" value="" checked>`) ||
		!strings.Contains(form, "Default: follows your defaults (currently off).") {
		t.Errorf("the add form does not start on Default; body=%s", form)
	}

	// The form posts its checked pill, Default, as an empty value.
	body := postForm(t, w, "/sources", url.Values{"name": {"New"}, "url": {"https://new.test/feed"}, "enabled": {""}})
	if got := mustFeed(t, db, "New"); got.Enabled != nil {
		t.Errorf("an untouched Fetching pill stored %v", *got.Enabled)
	}
	if !strings.Contains(body, `data-name="New"`) || !strings.Contains(body, `data-state="off"`) {
		t.Errorf("the added source is not shown as off; body=%s", body)
	}
	// An explicit On overrides the defaults.
	postForm(t, w, "/sources", url.Values{"name": {"Forced"}, "url": {"https://forced.test/feed"}, "enabled": {"on"}})
	if got := mustFeed(t, db, "Forced").Enabled; got == nil || !*got {
		t.Errorf("On did not override the defaults: %v", got)
	}
}

// Plain http is flagged under the address field, and only then.
func TestSourcesEditorFlagsHTTP(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{
		{Name: "Plain", URL: "http://plain.test/feed"},
		{Name: "Secure", URL: "https://secure.test/feed"},
	}})
	const text = "This address is not encrypted (http). Use https:// if the site supports it."

	plain := get(t, w, "/sources/feed/"+feedID(t, db, "Plain"))
	warn := tagWith(t, plain, "data-src-insecure", "")
	wantAttrs(t, "warning", warn, map[string]string{"role": "status"})
	lacksAttr(t, "the warning on an http address", warn, "hidden")
	if !strings.Contains(plain, ">"+text+"</p>") {
		t.Errorf("the warning text is missing; body=%s", plain)
	}
	secure := get(t, w, "/sources/feed/"+feedID(t, db, "Secure"))
	wantAttrs(t, "warning on https", tagWith(t, secure, "data-src-insecure", ""), map[string]string{"hidden": present})
}

// The tuning hints name where a blank field's value comes from, and tell a
// stored value from a pending one.
func TestTuningHint(t *testing.T) {
	cases := []struct {
		name                       string
		typed, own, fallback, from string
		want                       string
	}{
		{"blank from defaults", "", "", "14d", originDefaults, "14d from defaults"},
		{"blank built-in", "", "", "7d", originBuiltin, "7d built-in"},
		{"blank uncapped", "", "", "uncapped", originBuiltin, "uncapped built-in"},
		{"own value", "3d", "3d", "14d", originDefaults, "3d set on this source"},
		{"typed over defaults", "12h", "", "14d", originDefaults, "12h set here (otherwise 14d from defaults)"},
		{"typed over own", "5", "3", "uncapped", originBuiltin, "5 set here (otherwise uncapped built-in)"},
		{"zero is a value", "0", "", "25", originDefaults, "0 set here (otherwise 25 from defaults)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := tuningHint(c.typed, c.own, c.fallback, c.from); got != c.want {
				t.Errorf("tuningHint = %q, want %q", got, c.want)
			}
		})
	}
}

// The hints on a rendered editor follow the origin: the source's own value, the
// defaults, or the built-in.
func TestSourcesEditorHintsFollowTheOrigin(t *testing.T) {
	d := config.FeedDefaults{Since: durPtr(14 * 24 * time.Hour)}
	w, db := sourcesTestWeb(t, config.FeedsDoc{
		Defaults: d,
		Feeds: []config.Feed{
			{Name: "Own", URL: "https://own.test/feed", Since: durPtr(12 * time.Hour), MaxItems: intPtr(5)},
			{Name: "Inherits", URL: "https://inherits.test/feed"},
		},
	})

	own := get(t, w, "/sources/feed/"+feedID(t, db, "Own"))
	for _, want := range []string{
		`<span data-src-hint-t>12h set on this source</span>. Use 14d or 12h.`,
		`<span data-src-hint-t>5 set on this source</span>. 0 means no cap.`,
		`data-fallback="14d" data-from="from defaults" data-own="12h"`,
		`data-fallback="uncapped" data-from="built-in" data-own="5"`,
	} {
		if !strings.Contains(own, want) {
			t.Errorf("missing %q; body=%s", want, own)
		}
	}
	inherits := get(t, w, "/sources/feed/"+feedID(t, db, "Inherits"))
	for _, want := range []string{
		`<span data-src-hint-t>14d from defaults</span>`,
		`<span data-src-hint-t>uncapped built-in</span>`,
		`placeholder="14d"`,
		`placeholder="uncapped"`,
	} {
		if !strings.Contains(inherits, want) {
			t.Errorf("missing %q; body=%s", want, inherits)
		}
	}

	// With no defaults the fallback is the built-in.
	w, db = sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{{Name: "Bare", URL: "https://bare.test/feed"}}})
	if out := get(t, w, "/sources/feed/"+feedID(t, db, "Bare")); !strings.Contains(out, "7d built-in") {
		t.Errorf("no built-in origin on a source with no defaults; body=%s", out)
	}
}

// The delete answers with the unit in its removed state and a toast marker;
// restoring answers with the live unit. Neither repaints the list.
func TestSourcesRemoveAndRestoreAnswerWithTheUnit(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}}})
	id := feedID(t, db, "Alpha")

	removed := deleteSource(t, w, id)
	for _, want := range []string{
		`id="srcFlash" hx-swap-oob="true" data-msg="Removed Alpha" data-undo="`,
		`<div class="mg-item" id="src-` + id + `"`,
		`mg-row is-removed`,
		`aria-label="Restore Alpha"`,
		`id="srcSummary" hx-swap-oob="true"`,
		`id="srcAttn" hx-swap-oob="true"`,
		`id="srcFilter" role="radiogroup" aria-label="Filter sources" hx-swap-oob="true"`,
		`id="srcNavBadge" hx-swap-oob="true"`,
	} {
		if !strings.Contains(removed, want) {
			t.Errorf("missing %q; body=%s", want, removed)
		}
	}
	wantAttrs(t, "restore button", tagWith(t, removed, "aria-label", "Restore Alpha"), map[string]string{
		"hx-post": "/sources/feed/" + id + "/restore", "hx-target": "#src-" + id, "hx-swap": "outerHTML",
	})
	if strings.Contains(removed, `id="srcBody"`) || strings.Contains(removed, "aria-label=\"Edit Alpha\"") {
		t.Errorf("the removed unit is not a bare removed row; body=%s", removed)
	}

	restored := postForm(t, w, "/sources/feed/"+id+"/restore", nil)
	if !strings.Contains(restored, `aria-label="Fetch Alpha"`) || strings.Contains(restored, `id="srcBody"`) {
		t.Errorf("a restore did not answer with the live unit; body=%s", restored)
	}
}

// A removed source has no editor: Edit is not offered and opening it by address
// renders the removed row alone.
func TestSourcesRemovedUnitHasNoEditor(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}}})
	id := feedID(t, db, "Alpha")
	deleteSource(t, w, id)

	if out := get(
		t,
		w,
		"/sources/feed/"+id,
	); strings.Contains(out, `class="mg-ed"`) ||
		!strings.Contains(out, "is-removed") {
		t.Errorf("a removed source opened an editor; body=%s", out)
	}
	if out := get(t, w, "/sources/manage?feed="+id); strings.Contains(out, `class="mg-ed"`) {
		t.Errorf("a removed source's deep link opened an editor; body=%s", out)
	}
}

// Adding opens an empty editor on a unit of its own at the top of the list; a
// rejected add keeps the typed values inside it.
func TestSourcesAddFormIsAnInlineUnit(t *testing.T) {
	w, _ := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}}})
	out := get(t, w, "/sources/new")

	wantAttrs(t, "add unit", tagWith(t, out, "id", "src-new"), map[string]string{"class": "mg-item mg-item--add"})
	wantAttrs(t, "add form", tagWith(t, out, "id", "ed-new"), map[string]string{
		"class": "mg-ed", "role": "region", "aria-label": "Add a source",
		"hx-post": "/sources", "hx-target": "#srcBody", "hx-swap": "outerHTML",
	})
	if strings.Index(out, `id="src-new"`) > strings.Index(out, `data-src-list`) {
		t.Errorf("the add unit is below the list; body=%s", out)
	}
	wantAttrs(t, "cancel", tagWith(t, out, "data-src-close", ""), map[string]string{"type": "button"})
	for _, want := range []string{`Add source</button>`, `id="srcBody"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q; body=%s", want, out)
		}
	}

	bad := postForm(t, w, "/sources", url.Values{"name": {"Dup"}, "url": {"https://alpha.test/feed"}, "since": {"3d"}})
	if !strings.Contains(bad, `id="src-new"`) || !strings.Contains(bad, store.ErrFeedURLTaken.Error()) ||
		!strings.Contains(bad, `name="name" value="Dup"`) {
		t.Errorf("a rejected add lost the form; body=%s", bad)
	}
}

// With no sources the add form takes the first-run prompt's place: the prompt
// block is sent hidden, for the script to show again later.
func TestSourcesAddFormReplacesTheFirstRunPrompt(t *testing.T) {
	w, _ := sourcesTestWeb(t, config.FeedsDoc{})
	out := get(t, w, "/sources/new")
	if !strings.Contains(out, `id="src-new"`) || strings.Contains(out, `data-src-empty="first">`) {
		t.Errorf("the first-run prompt shows beside the add form; body=%s", out)
	}
}

// The running note polls itself while a run is live, so a page with no ingest
// bar still sees it clear. With a bar on the page the bar carries the note.
func TestIngestNotePollsOnlyWhileRunning(t *testing.T) {
	render := func(d ingestBarData) string {
		var b strings.Builder
		if err := feedTmpl.ExecuteTemplate(&b, "ingestNote", d); err != nil {
			t.Fatalf("ingestNote: %v", err)
		}
		return b.String()
	}
	const poll = `hx-get="/ingest/status?view=note" hx-trigger="every 2s" hx-swap="outerHTML"`
	if out := render(ingestBarData{Running: true, Poll: true}); !strings.Contains(out, poll) ||
		strings.Contains(out, "hidden") {
		t.Errorf("a live run's note does not poll or is hidden: %s", out)
	}
	if out := render(ingestBarData{Running: true}); strings.Contains(out, "hx-get") ||
		strings.Contains(out, "hidden") {
		t.Errorf("a note beside a bar polls or is hidden: %s", out)
	}
	if out := render(ingestBarData{Poll: true}); strings.Contains(out, "hx-get") || !strings.Contains(out, " hidden") {
		t.Errorf("an idle note polls or shows: %s", out)
	}

	w, _ := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}}})
	out := get(t, w, "/ingest/status?view=note")
	if !strings.HasPrefix(strings.TrimSpace(out), `<p class="sdet__warn ingb__note" id="ingestNote"`) ||
		strings.Contains(out, "<section") {
		t.Errorf("the note view is not the note alone: %s", out)
	}
}

// "Last run" names when the run ended, not when it started.
func TestFinishedAgoUsesTheEndTime(t *testing.T) {
	now := time.Now()
	end := now.Add(-2 * time.Hour)
	run := store.IngestRun{StartedAt: now.Add(-3 * time.Hour), FinishedAt: &end}
	if got := finishedAgo(run, now); got != "2h ago" {
		t.Errorf("finishedAgo = %q, want 2h ago", got)
	}
	run.FinishedAt = nil
	if got := finishedAgo(run, now); got != "3h ago" {
		t.Errorf("without an end time finishedAgo = %q, want the start, 3h ago", got)
	}
}

// An ingestion run starting or ending refreshes the page's live parts out of
// band. Manage gets the status block, counts and every row without an open
// editor, so a form in progress is never swapped.
func TestSourcesLiveRefreshOnManage(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{
		{Name: "Alpha", URL: "https://alpha.test/feed"},
		{Name: "Beta", URL: "https://beta.test/feed"},
	}})
	alpha, beta := feedID(t, db, "Alpha"), feedID(t, db, "Beta")
	recordFetches(t, db, config.Feed{Name: "Beta", URL: "https://beta.test/feed"}, 0, 3)

	out := get(t, w, "/sources/live?page=manage&open="+alpha)
	for _, id := range []string{"srcSummary", "srcAttn", "srcFilter", "srcNavBadge", "ingestNote"} {
		wantAttrs(t, "#"+id, tagWith(t, out, "id", id), map[string]string{"hx-swap-oob": "true"})
	}
	wantAttrs(t, "refreshed row", tagWith(t, out, "id", "src-"+beta), map[string]string{
		"class": "mg-item", "hx-swap-oob": "true", "data-state": "error",
	})
	if strings.Contains(out, `id="src-`+alpha+`"`) {
		t.Errorf("a row with an open editor was refreshed; body=%s", out)
	}
	if strings.Contains(out, `id="srcBody"`) || strings.Contains(out, `class="mg-ed"`) {
		t.Errorf("a live refresh carries the list or an editor; body=%s", out)
	}
	if !strings.Contains(out, "1 failing") {
		t.Errorf("the refreshed status does not show the failing source; body=%s", out)
	}

	// With nothing open every row comes back.
	all := get(t, w, "/sources/live?page=manage")
	if !strings.Contains(all, `id="src-`+alpha+`"`) || !strings.Contains(all, `id="src-`+beta+`"`) {
		t.Errorf("a live refresh with nothing open left rows out; body=%s", all)
	}
}

// Overview gets its whole body and the nav badge, out of band.
func TestSourcesLiveRefreshOnOverview(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{{Name: "Beta", URL: "https://beta.test/feed"}}})
	recordFetches(t, db, config.Feed{Name: "Beta", URL: "https://beta.test/feed"}, 0, 3)

	out := get(t, w, "/sources/live?page=overview")
	wantAttrs(t, "overview", tagWith(t, out, "id", "srcOverview"), map[string]string{"hx-swap-oob": "true"})
	wantAttrs(t, "badge", tagWith(t, out, "id", "srcNavBadge"), map[string]string{"hx-swap-oob": "true"})
	if !strings.Contains(out, "1 source, 1 needs attention.") || strings.Contains(out, "mg-item") {
		t.Errorf("the overview refresh is not the overview alone; body=%s", out)
	}
}

// Both pages listen for the event the chrome raises when a run starts or ends.
func TestSourcesPagesListenForIngestChanges(t *testing.T) {
	w, _ := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}}})
	for _, page := range []string{"manage", "overview"} {
		path := "/sources"
		if page == "manage" {
			path = "/sources/manage"
		}
		out := get(t, w, path)
		wantAttrs(t, page+" listener", tagWith(t, out, "hx-get", "/sources/live?page="+page), map[string]string{
			"hx-trigger": "sources-ingest-change from:body", "hx-swap": "none", "hidden": present,
		})
	}
}

// Both pages lead with an h1, and the first-run Overview does too.
func TestSourcesPagesHaveAnH1(t *testing.T) {
	w, _ := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}}})
	if out := get(
		t,
		w,
		"/sources",
	); !strings.Contains(out, "<h1>Overview</h1>") ||
		strings.Contains(out, "<h2>Overview") {
		t.Errorf("the Overview heading is not an h1; body=%s", out)
	}
	if out := get(
		t,
		w,
		"/sources/manage",
	); strings.Count(out, "<h1") != 1 ||
		!strings.Contains(out, "Manage sources</h1>") {
		t.Errorf("Manage does not have exactly one h1; body=%s", out)
	}
	empty, _ := sourcesTestWeb(t, config.FeedsDoc{})
	if out := get(t, empty, "/sources"); !strings.Contains(out, "<h1>No sources yet</h1>") {
		t.Errorf("the first-run Overview has no h1; body=%s", out)
	}
}

// The first-run block is always sent, hidden while there is something to list,
// so removing the last source can bring it back without a reload. With nothing
// failing the status block is an empty element.
func TestSourcesFirstRunBlockIsAlwaysSent(t *testing.T) {
	w, _ := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}}})
	out := get(t, w, "/sources/manage")
	wantAttrs(t, "first-run block", tagWith(t, out, "data-src-empty", "first"), map[string]string{"hidden": present})

	empty, _ := sourcesTestWeb(t, config.FeedsDoc{})
	out = get(t, empty, "/sources/manage")
	lacksAttr(t, "the first-run block of an empty store", tagWith(t, out, "data-src-empty", "first"), "hidden")
	if !strings.Contains(out, `<div class="mg-status" id="srcAttn"></div>`) {
		t.Errorf("the status block of an empty store is not an empty element; body=%s", out)
	}
}

func TestAttentionRunPhrase(t *testing.T) {
	cases := []struct {
		name string
		bar  ingestBarData
		want string
	}{
		{"never ran", ingestBarData{Status: "none"}, "No ingestion run yet."},
		{"finished", ingestBarData{Status: "ok", When: "2h ago"}, "Last run 2h ago."},
		{"running", ingestBarData{Status: "running", Running: true, When: "just now"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := (sourcesAttention{Bar: c.bar}).RunPhrase(); got != c.want {
				t.Errorf("RunPhrase() = %q, want %q", got, c.want)
			}
		})
	}
}

// sidePanelOf returns the editor's side panel markup.
func sidePanelOf(t *testing.T, body string) string {
	t.Helper()
	_, rest, ok := strings.Cut(body, `<aside class="mg-ed__side"`)
	if !ok {
		t.Fatalf("no side panel; body=%s", body)
	}
	panel, _, _ := strings.Cut(rest, "</aside>")
	return panel
}

// factOf returns the text of one fact's value, tags stripped.
func factOf(t *testing.T, panel, label string) string {
	t.Helper()
	_, rest, ok := strings.Cut(panel, "<dt>"+label+"</dt><dd>")
	if !ok {
		t.Fatalf("no fact %q; panel=%s", label, panel)
	}
	val, _, _ := strings.Cut(rest, "</dd>")
	var b strings.Builder
	inTag := false
	for _, r := range val {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
		case !inTag:
			b.WriteRune(r)
		}
	}
	return html.UnescapeString(b.String())
}

// The panel states the last 30 days from the stored items, the user's reads
// and the fetch attempts, and the settings the source runs with.
func TestSourcesEditorSidePanelFacts(t *testing.T) {
	alpha := config.Feed{
		Name:     "Alpha",
		URL:      "https://alpha.test/feed",
		Since:    durPtr(7 * 24 * time.Hour),
		MaxItems: intPtr(20),
	}
	w, db := sourcesTestWeb(t, config.FeedsDoc{
		Defaults: config.FeedDefaults{MaxItems: intPtr(50)},
		Feeds:    []config.Feed{alpha, {Name: "Beta", URL: "https://beta.test/feed"}},
	})
	seedItems(t, db, "Alpha", 12, 2, 1, 3)
	read, up := store.StatusRead, 10
	if err := db.UpdateUserState(t.Context(), "Alpha-0", store.UserPatch{Status: &read, UserScore: &up}); err != nil {
		t.Fatalf("UpdateUserState: %v", err)
	}
	now := time.Now()
	if err := db.RecordFeedFetches(t.Context(), []store.FeedFetch{
		{
			FeedID:  config.FeedID(alpha.URL),
			Name:    "Alpha",
			Items:   3,
			Elapsed: 400 * time.Millisecond,
			At:      now.Add(-3 * time.Hour),
		},
		{
			FeedID:  config.FeedID(alpha.URL),
			Name:    "Alpha",
			Items:   3,
			Elapsed: 1500 * time.Millisecond,
			At:      now.Add(-2 * time.Hour),
		},
		{
			FeedID:  config.FeedID(alpha.URL),
			Name:    "Alpha",
			Items:   3,
			Elapsed: 800 * time.Millisecond,
			At:      now.Add(-time.Hour),
		},
	}); err != nil {
		t.Fatalf("RecordFeedFetches: %v", err)
	}

	panel := sidePanelOf(t, get(t, w, "/sources/feed/"+feedID(t, db, "Alpha")))
	for _, want := range []string{"Last 30 days", "Fetch history", `<span class="sh-hint">last 3 attempts</span>`} {
		if !strings.Contains(panel, want) {
			t.Errorf("panel is missing %q; panel=%s", want, panel)
		}
	}
	for label, want := range map[string]string{
		"Status":          "Fetching fine. Last fetch 1h ago.",
		"Stored items":    "2.3 per week, last item just now",
		"Signal":          "80%: 12 of 15 scored are 7+, 3 unscored",
		"Reader activity": "1 read, 0 hidden, 0 bookmarked, 1 thumbs up, 0 down",
		"Fetch time":      "900 ms average, 800 ms last",
		"In effect":       "7d back (set on this source), up to 20 items (set on this source)",
	} {
		if got := factOf(t, panel, label); got != want {
			t.Errorf("%s = %q, want %q", label, got, want)
		}
	}
	if !strings.Contains(panel, `class="spark"`) || !strings.Contains(panel, `class="tier"`) {
		t.Errorf("the panel has no spark or tier bar; panel=%s", panel)
	}

	// Beta sets nothing itself: the origins come from the defaults and the built-in.
	beta := sidePanelOf(t, get(t, w, "/sources/feed/"+feedID(t, db, "Beta")))
	if got, want := factOf(t, beta, "In effect"), "7d back (built-in), up to 50 items (from defaults)"; got != want {
		t.Errorf("In effect = %q, want %q", got, want)
	}
}

// An uncapped source says so rather than printing a dash.
func TestSourcesEditorSidePanelUncapped(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}}})
	panel := sidePanelOf(t, get(t, w, "/sources/feed/"+feedID(t, db, "Alpha")))
	if got, want := factOf(t, panel, "In effect"), "7d back (built-in), no item cap (built-in)"; got != want {
		t.Errorf("In effect = %q, want %q", got, want)
	}
}

// A source that was never fetched has nothing to report yet, and says so.
func TestSourcesEditorSidePanelNeverFetched(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}}})
	panel := sidePanelOf(t, get(t, w, "/sources/feed/"+feedID(t, db, "Alpha")))
	for label, want := range map[string]string{
		"Status":          "Waiting for first run",
		"Stored items":    "0 per week",
		"Signal":          "No items yet.",
		"Reader activity": "None yet.",
		"Fetch time":      "Not fetched yet.",
	} {
		if got := factOf(t, panel, label); got != want {
			t.Errorf("%s = %q, want %q", label, got, want)
		}
	}
	if !strings.Contains(panel, "No fetches yet. The first run fills this in.") || strings.Contains(panel, "mg-hist") {
		t.Errorf("the empty history is not the empty state; panel=%s", panel)
	}
	if strings.Contains(panel, "last 0 attempts") {
		t.Errorf("an empty history carries a count; panel=%s", panel)
	}
}

// A disabled source says ingestion skips it, and still shows what it has.
func TestSourcesEditorSidePanelDisabled(t *testing.T) {
	off := false
	alpha := config.Feed{Name: "Alpha", URL: "https://alpha.test/feed", Enabled: &off}
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{alpha}})
	recordTimed(t, db, alpha, 400*time.Millisecond)
	seedItems(t, db, "Alpha", 1, 0, 0, 0)

	panel := sidePanelOf(t, get(t, w, "/sources/feed/"+feedID(t, db, "Alpha")))
	if got, want := factOf(t, panel, "Status"), "Disabled. Ingestion skips it. Last fetch 1h ago."; got != want {
		t.Errorf("Status = %q, want %q", got, want)
	}
	if got, want := factOf(
		t,
		panel,
		"Signal",
	), "Not enough data yet. Only 1 scored so far (needs 10), 0 unscored"; got != want {
		t.Errorf("Signal = %q, want %q", got, want)
	}
}

// A failing source gives the plain-language reason, and its history keeps the
// raw error, clipped, with the full text on hover.
func TestSourcesEditorSidePanelFailingAndHistory(t *testing.T) {
	alpha := config.Feed{Name: "Alpha", URL: "https://alpha.test/feed"}
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{alpha}})
	long := "http error: 503 " + strings.Repeat("unavailable ", 40)
	now := time.Now()
	id := config.FeedID(alpha.URL)
	if err := db.RecordFeedFetches(t.Context(), []store.FeedFetch{
		{FeedID: id, Name: "Alpha", Items: 1, Elapsed: 2 * time.Second, At: now.Add(-4 * time.Hour)},
		{FeedID: id, Name: "Alpha", Error: long, At: now.Add(-3 * time.Hour)},
		{FeedID: id, Name: "Alpha", Error: long, At: now.Add(-2 * time.Hour)},
		{FeedID: id, Name: "Alpha", Error: long, Elapsed: 30 * time.Second, At: now.Add(-time.Hour)},
	}); err != nil {
		t.Fatalf("RecordFeedFetches: %v", err)
	}

	panel := sidePanelOf(t, get(t, w, "/sources/feed/"+feedID(t, db, "Alpha")))
	status := factOf(t, panel, "Status")
	if want := explainFeedError(long) + " Last fetch 1h ago."; status != want {
		t.Errorf("Status = %q, want %q", status, want)
	}
	if got, want := factOf(t, panel, "Fetch time"), "2.0 s average, 30 s last"; got != want {
		t.Errorf("Fetch time = %q, want %q", got, want)
	}

	if !strings.Contains(panel, `last 4 attempts`) || strings.Count(panel, `class="mg-hist__r`) != 4 {
		t.Errorf("history is not four rows; panel=%s", panel)
	}
	if strings.Count(panel, `<span class="mg-chip mg-chip--bad">error</span>`) != 3 ||
		strings.Count(panel, `<span class="stp-chip stp-chip--ok">ok</span>`) != 1 {
		t.Errorf("history chips are wrong; panel=%s", panel)
	}
	if !strings.Contains(panel, `class="mg-hist__r is-err"`) ||
		!strings.Contains(panel, `<span class="mg-hist__ms">30 s</span>`) ||
		!strings.Contains(panel, `<span class="mg-hist__ms">2.0 s</span>`) ||
		!strings.Contains(panel, `<span class="mg-hist__d">1 item</span>`) {
		t.Errorf("history rows are missing their detail or elapsed; panel=%s", panel)
	}
	// The error is clipped in the row and whole in its title; the attempt's
	// ago carries the full timestamp.
	if !strings.Contains(panel, `title="`+html.EscapeString(strings.TrimSpace(long))) {
		t.Errorf("the error has no full-text title; panel=%s", panel)
	}
	if !strings.Contains(panel, clipText(long)+`</span>`) ||
		strings.Contains(panel, html.EscapeString(long)+`</span>`) {
		t.Errorf("the error is not clipped in the row; panel=%s", panel)
	}
	stamp := now.Add(-time.Hour).Local().Format("2 Jan 2006, 15:04")
	if !strings.Contains(panel, `<span class="mg-hist__t" title="`+stamp) {
		t.Errorf("the ago has no timestamp title %q; panel=%s", stamp, panel)
	}
}

// The panel sits beside the form and holds no controls, so the editor's dirty
// guard never reads it. Adding a source has no panel.
func TestSourcesEditorSidePanelIsReadOnlyAndAbsentWhenAdding(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}}})
	recordFetches(t, db, config.Feed{Name: "Alpha", URL: "https://alpha.test/feed"}, 2, 0)

	out := get(t, w, "/sources/feed/"+feedID(t, db, "Alpha"))
	if !strings.Contains(out, `<div class="mg-ed__cols">`) {
		t.Errorf("the editor has no columns; body=%s", out)
	}
	panel := sidePanelOf(t, out)
	for _, bad := range []string{"<input", "<button", "<select", "<textarea"} {
		if strings.Contains(panel, bad) {
			t.Errorf("the panel holds a control %q; panel=%s", bad, panel)
		}
	}

	add := get(t, w, "/sources/new")
	if strings.Contains(add, "Source activity") || strings.Contains(add, "Fetch history") {
		t.Errorf("the add form has an activity panel; body=%s", add)
	}
	// What sits beside it is the preview, empty until a pick or Preview fills it.
	wantAttrs(t, "preview", tagWith(t, add, "id", "ed-prev-new"), map[string]string{
		"class": "mg-prev mg-ed__side", "aria-live": "polite",
	})
	if !strings.Contains(add, `id="ed-prev-new" aria-live="polite"></aside>`) {
		t.Errorf("the preview panel is not empty; body=%s", add)
	}
	if !strings.Contains(add, `<div class="mg-ed__cols">`) {
		t.Errorf("the add form lost its form column; body=%s", add)
	}
}

// A rejected save re-renders the editor with what was typed, and keeps the
// panel for the stored source.
func TestSourcesEditorSidePanelSurvivesFailedSave(t *testing.T) {
	alpha := config.Feed{Name: "Alpha", URL: "https://alpha.test/feed"}
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{alpha}})
	recordFetches(t, db, alpha, 2, 0)
	id := feedID(t, db, "Alpha")

	out := postForm(
		t,
		w,
		"/sources/feed/"+id,
		url.Values{"name": {"Alpha"}, "url": {"https://alpha.test/feed"}, "since": {"2w"}},
	)
	if !strings.Contains(out, `class="sdet__err mg-ed__msg"`) {
		t.Fatalf("the save was not rejected; body=%s", out)
	}
	panel := sidePanelOf(t, out)
	if got := factOf(t, panel, "Status"); !strings.HasPrefix(got, "Fetching fine. Last fetch ") {
		t.Errorf("Status = %q", got)
	}
	if strings.Count(panel, `class="mg-hist__r`) != 2 {
		t.Errorf("the history was lost; panel=%s", panel)
	}
}
