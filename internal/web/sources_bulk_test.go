// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// flashAttrs reads the toast marker out of a response.
func flashAttrs(t *testing.T, body string) map[string]string {
	t.Helper()
	m := regexp.MustCompile(`<div[^<>]*\sid="srcFlash"[^<>]*>`).FindString(body)
	if m == "" {
		t.Fatalf("no toast marker; body=%s", body)
	}
	return parseTag(m)
}

// flashOf reads the toast marker out of a response: its message and undo token.
func flashOf(t *testing.T, body string) (msg, token string) {
	t.Helper()
	a := flashAttrs(t, body)
	return a["data-msg"], a["data-undo"]
}

// flashToken checks the marker's message and returns its undo token.
func flashToken(t *testing.T, body, wantMsg string) string {
	t.Helper()
	msg, token := flashOf(t, body)
	if msg != wantMsg {
		t.Errorf("toast = %q, want %q", msg, wantMsg)
	}
	if token == "" {
		t.Errorf("toast %q has no undo token", msg)
	}
	return token
}

func serve(w *Web, method, path string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, req)
	return rec
}

// bulkPost posts a bulk action over ids and wants a 200.
func bulkPost(t *testing.T, w *Web, action string, tag string, ids ...string) string {
	t.Helper()
	form := url.Values{"action": {action}, "ids": {strings.Join(ids, ",")}}
	if tag != "" {
		form.Set("tag", tag)
	}
	rec := serve(w, http.MethodPost, "/sources/bulk", form)
	if rec.Code != http.StatusOK {
		t.Fatalf("bulk %s: status = %d; body=%s", action, rec.Code, rec.Body)
	}
	return rec.Body.String()
}

func undoPost(t *testing.T, w *Web, token string) string {
	t.Helper()
	rec := serve(w, http.MethodPost, "/sources/undo/"+token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("undo: status = %d; body=%s", rec.Code, rec.Body)
	}
	return rec.Body.String()
}

func wantStatus400(t *testing.T, w *Web, form url.Values, want string) {
	t.Helper()
	rec := serve(w, http.MethodPost, "/sources/bulk", form)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), want) {
		t.Errorf("bulk %v = %d %q, want 400 %q", form, rec.Code, rec.Body, want)
	}
}

func ownFeed(t *testing.T, db *store.Store, id string) config.Feed {
	t.Helper()
	f, err := db.FeedByID(t.Context(), id)
	if err != nil {
		t.Fatalf("FeedByID: %v", err)
	}
	return f
}

func enabledOf(f config.Feed) string {
	switch {
	case f.Enabled == nil:
		return "nil"
	case *f.Enabled:
		return "true"
	default:
		return "false"
	}
}

// Enable and Disable write only where the resolved value changes, so a source
// that already behaves that way keeps following the defaults, and undo puts
// back exactly what each row held, nil included.
func TestBulkEnableDisableKeepDefaultAndUndoRestoresExactly(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{
		{Name: "Inherits", URL: "https://a.test/feed"},
		{Name: "Off", URL: "https://b.test/feed", Enabled: boolPtr(false)},
		{Name: "On", URL: "https://c.test/feed", Enabled: boolPtr(true)},
	}})
	inherits, off, on := feedID(t, db, "Inherits"), feedID(t, db, "Off"), feedID(t, db, "On")
	state := func() string {
		return enabledOf(
			ownFeed(t, db, inherits),
		) + "/" + enabledOf(
			ownFeed(t, db, off),
		) + "/" + enabledOf(
			ownFeed(t, db, on),
		)
	}

	// Enabling all touches only the parked one: the others already run.
	body := bulkPost(t, w, "enable", "", inherits, off, on)
	token := flashToken(t, body, "Enabled 1 source")
	if got := state(); got != "nil/true/true" {
		t.Fatalf("after enable state = %s, want nil/true/true", got)
	}
	undoPost(t, w, token)
	if got := state(); got != "nil/false/true" {
		t.Errorf("after undo state = %s, want nil/false/true", got)
	}

	// Disabling writes false on the two that run; undo restores nil and true.
	body = bulkPost(t, w, "disable", "", inherits, off, on)
	token = flashToken(t, body, "Disabled 2 sources")
	if got := state(); got != "false/false/false" {
		t.Fatalf("after disable state = %s, want false/false/false", got)
	}
	undoPost(t, w, token)
	if got := state(); got != "nil/false/true" {
		t.Errorf("after undo state = %s, want nil/false/true", got)
	}
}

// With the defaults off, enabling writes an explicit true and undo returns the
// row to following the defaults.
func TestBulkEnableUnderParkedDefaults(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{
		Defaults: config.FeedDefaults{Enabled: boolPtr(false)},
		Feeds:    []config.Feed{{Name: "Inherits", URL: "https://a.test/feed"}},
	})
	id := feedID(t, db, "Inherits")
	token := flashToken(t, bulkPost(t, w, "enable", "", id), "Enabled 1 source")
	if got := enabledOf(ownFeed(t, db, id)); got != "true" {
		t.Fatalf("enabled = %s, want true", got)
	}
	undoPost(t, w, token)
	if got := enabledOf(ownFeed(t, db, id)); got != "nil" {
		t.Errorf("enabled after undo = %s, want nil", got)
	}
}

// The reply is out-of-band only: the changed rows, the page counts and the
// marker, with the feed count in the header. Unchanged rows are not resent.
func TestBulkReplyShape(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{
		{Name: "Alpha", URL: "https://alpha.test/feed"},
		{Name: "Beta", URL: "https://beta.test/feed"},
	}})
	alpha, beta := feedID(t, db, "Alpha"), feedID(t, db, "Beta")

	rec := serve(w, http.MethodPost, "/sources/bulk", url.Values{"action": {"disable"}, "ids": {alpha}})
	body := rec.Body.String()
	for _, want := range []string{
		`id="src-` + alpha + `"`, `hx-swap-oob="true"`,
		`id="srcSummary" hx-swap-oob="true"`,
		`id="srcAttn" hx-swap-oob="true"`,
		`id="srcFilter" role="radiogroup" aria-label="Filter sources" hx-swap-oob="true"`,
		`id="srcNavBadge" hx-swap-oob="true"`,
		`data-msg="Disabled 1 source"`, `data-undo="`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q; body=%s", want, body)
		}
	}
	if strings.Contains(body, `id="src-`+beta+`"`) || strings.Contains(body, `id="srcBody"`) {
		t.Errorf("the reply carries an unchanged row or the list; body=%s", body)
	}
	if got := rec.Header().Get(feedCountHeader); got != "2" {
		t.Errorf("%s = %q, want 2", feedCountHeader, got)
	}
	if !strings.Contains(body, `class="mg-row is-off`) {
		t.Errorf("the changed row is not in its parked state; body=%s", body)
	}
}

func TestBulkRemoveRestoreAndStaleIDs(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{
		{Name: "Alpha", URL: "https://alpha.test/feed"},
		{Name: "Beta", URL: "https://beta.test/feed"},
		{Name: "Gone", URL: "https://gone.test/feed"},
	}})
	alpha, beta, gone := feedID(t, db, "Alpha"), feedID(t, db, "Beta"), feedID(t, db, "Gone")
	if err := db.SoftDeleteFeed(t.Context(), gone); err != nil {
		t.Fatalf("SoftDeleteFeed: %v", err)
	}

	// An unknown id and an already removed one are skipped.
	body := bulkPost(t, w, "remove", "", alpha, gone, "nosuchid", alpha)
	token := flashToken(t, body, "Removed 1 source")
	if live := mustFeeds(t, db); len(live) != 1 || live[0].ID != beta {
		t.Fatalf("live after remove = %v, want only Beta", live)
	}
	if !strings.Contains(body, `data-removed="1"`) || !strings.Contains(body, `Removed <i class="mg-n">2</i>`) {
		t.Errorf("the removed row or the pill count is missing; body=%s", body)
	}

	// Restore acts on removed rows only.
	body = bulkPost(t, w, "restore", "", alpha, beta, gone)
	restoreToken := flashToken(t, body, "Restored 2 sources")
	if len(mustFeeds(t, db)) != 3 {
		t.Fatalf("live after restore = %d, want 3", len(mustFeeds(t, db)))
	}
	if msg, _ := flashOf(t, bulkPost(t, w, "restore", "", alpha, beta)); msg != "Nothing to change." {
		t.Errorf("restoring live rows = %q, want Nothing to change.", msg)
	}

	// Undoing the restore removes both again, with their original times kept.
	undoPost(t, w, restoreToken)
	if live := mustFeeds(t, db); len(live) != 1 || live[0].ID != beta {
		t.Fatalf("live after undoing the restore = %v, want only Beta", live)
	}
	// The first remove's token is still good: it brings Alpha back, but Gone was
	// never part of it.
	undoPost(t, w, token)
	if live := mustFeeds(t, db); len(live) != 2 {
		t.Errorf("live after undoing the remove = %d, want 2", len(live))
	}
}

func TestBulkTags(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{
		Defaults: config.FeedDefaults{Tags: []string{"shared"}},
		Feeds: []config.Feed{
			{Name: "Alpha", URL: "https://alpha.test/feed", Tags: []string{"ai"}},
			{Name: "Beta", URL: "https://beta.test/feed"},
			{Name: "Cap", URL: "https://cap.test/feed", Tags: []string{"new"}},
		},
	})
	alpha, beta, capID := feedID(t, db, "Alpha"), feedID(t, db, "Beta"), feedID(t, db, "Cap")

	body := bulkPost(t, w, "addtag", "New", alpha, beta, capID)
	token := flashToken(t, body, `Added tag "New" to 2 sources`)
	if got := ownFeed(t, db, alpha).Tags; !slices.Equal(got, []string{"ai", "New"}) {
		t.Errorf("Alpha tags = %v", got)
	}
	if got := ownFeed(t, db, capID).Tags; !slices.Equal(got, []string{"new"}) {
		t.Errorf("Cap tags = %v, want untouched", got)
	}
	undoPost(t, w, token)
	if got := ownFeed(t, db, alpha).Tags; !slices.Equal(got, []string{"ai"}) {
		t.Errorf("Alpha tags after undo = %v", got)
	}
	if got := ownFeed(t, db, beta).Tags; got != nil {
		t.Errorf("Beta tags after undo = %v, want none", got)
	}

	token = flashToken(t, bulkPost(t, w, "removetag", "AI", alpha, beta), `Removed tag "AI" from 1 source`)
	if got := ownFeed(t, db, alpha).Tags; got != nil {
		t.Errorf("Alpha tags = %v, want none", got)
	}
	undoPost(t, w, token)
	if got := ownFeed(t, db, alpha).Tags; !slices.Equal(got, []string{"ai"}) {
		t.Errorf("Alpha tags after undo = %v, want [ai]", got)
	}

	// A default tag is not the source's own, so it cannot be removed here.
	if msg, _ := flashOf(t, bulkPost(t, w, "removetag", "shared", alpha)); msg != "Nothing to change." {
		t.Errorf("removing an inherited tag = %q, want Nothing to change.", msg)
	}
	// A source that already shows the tag from the defaults is left out of an add.
	if msg, _ := flashOf(t, bulkPost(t, w, "addtag", "Shared", alpha, beta)); msg != "Nothing to change." {
		t.Errorf("adding an inherited tag = %q, want Nothing to change.", msg)
	}
	if got := ownFeed(t, db, beta).Tags; got != nil {
		t.Errorf("Beta tags = %v, want the inherited tag not copied", got)
	}
	// The toast names the tag as it is stored.
	flashToken(t, bulkPost(t, w, "addtag", "  Fresh , ", beta), `Added tag "Fresh" to 1 source`)
}

func TestUndoMarkerCarriesTheIDs(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{
		{Name: "Alpha", URL: "https://alpha.test/feed"},
		{Name: "Beta", URL: "https://beta.test/feed"},
	}})
	alpha, beta := feedID(t, db, "Alpha"), feedID(t, db, "Beta")

	a := flashAttrs(t, bulkPost(t, w, "disable", "", alpha, beta))
	if got := strings.Split(a["data-ids"], ","); !slices.Equal(got, []string{alpha, beta}) {
		t.Errorf("data-ids = %q, want %s,%s", a["data-ids"], alpha, beta)
	}
	if a["data-undo"] == "" {
		t.Error("the marker lacks its undo token")
	}
	if _, has := flashAttrs(t, bulkPost(t, w, "disable", "", alpha))["data-ids"]; has {
		t.Error("a message with no undo carries ids")
	}
}

// A store failure while undoing leaves the Undo usable.
func TestUndoFailureKeepsTheToken(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}}})
	id := feedID(t, db, "Alpha")
	token := flashToken(t, bulkPost(t, w, "disable", "", id), "Disabled 1 source")

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if rec := serve(w, http.MethodPost, "/sources/undo/"+token, nil); rec.Code != http.StatusInternalServerError {
		t.Fatalf("undo on a closed store = %d, want 500", rec.Code)
	}
	if _, ok := w.undo.take(token, w.clock()); !ok {
		t.Error("a failed undo spent its token")
	}
}

func TestBulkRejections(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}}})
	id := feedID(t, db, "Alpha")

	wantStatus400(t, w, url.Values{"action": {"frobnicate"}, "ids": {id}}, "Unknown bulk action: frobnicate")
	wantStatus400(t, w, url.Values{"ids": {id}}, "Unknown bulk action: ")
	wantStatus400(t, w, url.Values{"action": {"addtag"}, "ids": {id}}, "Tag name is required.")
	wantStatus400(t, w, url.Values{"action": {"addtag"}, "ids": {id}, "tag": {" , "}}, "Tag name is required.")
	wantStatus400(t, w, url.Values{"action": {"removetag"}, "ids": {id}, "tag": {""}}, "Tag name is required.")
	wantStatus400(t, w, url.Values{"action": {"disable"}, "ids": {" , "}}, "No sources selected.")

	ids := make([]string, maxBulkIDs+1)
	for i := range ids {
		ids[i] = "id" + strconv.Itoa(i)
	}
	wantStatus400(t, w, url.Values{"action": {"disable"}, "ids": {strings.Join(ids, ",")}}, "Too many sources selected")

	// Rejections change nothing and record nothing.
	if got := enabledOf(ownFeed(t, db, id)); got != "nil" {
		t.Errorf("enabled = %s after rejected requests, want nil", got)
	}
}

// A request that changes nothing answers 200 with the message and no token.
func TestBulkNothingToChange(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}}})
	id := feedID(t, db, "Alpha")

	for _, c := range []struct{ action, tag string }{
		{"enable", ""}, {"restore", ""}, {"removetag", "ghost"},
	} {
		body := bulkPost(t, w, c.action, c.tag, id)
		msg, token := flashOf(t, body)
		if msg != "Nothing to change." || token != "" {
			t.Errorf("%s: toast = %q token %q, want the message and no token", c.action, msg, token)
		}
		if strings.Contains(body, `id="src-`) {
			t.Errorf("%s: a nothing-to-change reply resends rows; body=%s", c.action, body)
		}
	}
	if msg, _ := flashOf(t, bulkPost(t, w, "disable", "", "nosuchid")); msg != "Nothing to change." {
		t.Errorf("stale id = %q, want Nothing to change.", msg)
	}
}

func TestUndoTokens(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}}})
	id := feedID(t, db, "Alpha")
	now := time.Now()
	w.now = func() time.Time { return now }

	gone := func(token string) {
		t.Helper()
		rec := serve(w, http.MethodPost, "/sources/undo/"+token, nil)
		if rec.Code != http.StatusGone || !strings.Contains(rec.Body.String(), "That can no longer be undone.") {
			t.Errorf("undo %q = %d %q, want 410 with the copy", token, rec.Code, rec.Body)
		}
	}
	gone("nosuchtoken")

	// A token is spent by its first use.
	token := flashToken(t, bulkPost(t, w, "disable", "", id), "Disabled 1 source")
	if len(token) != 32 {
		t.Errorf("token %q is %d chars, want 32 hex", token, len(token))
	}
	undoPost(t, w, token)
	gone(token)

	// A token lapses after the TTL.
	token = flashToken(t, bulkPost(t, w, "disable", "", id), "Disabled 1 source")
	now = now.Add(undoTTL + time.Second)
	gone(token)
	if got := enabledOf(ownFeed(t, db, id)); got != "false" {
		t.Errorf("enabled = %s, want the expired undo to change nothing", got)
	}
}

// Only the newest undoCap changes can still be undone.
func TestUndoEvictsOldestAtCap(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}}})
	id := feedID(t, db, "Alpha")
	var tokens []string
	for i := range undoCap + 1 {
		action := "disable"
		if i%2 == 1 {
			action = "enable"
		}
		_, token := flashOf(t, bulkPost(t, w, action, "", id))
		tokens = append(tokens, token)
	}
	if rec := serve(w, http.MethodPost, "/sources/undo/"+tokens[0], nil); rec.Code != http.StatusGone {
		t.Errorf("oldest token = %d, want 410", rec.Code)
	}
	if rec := serve(w, http.MethodPost, "/sources/undo/"+tokens[1], nil); rec.Code != http.StatusOK {
		t.Errorf("second token = %d, want it kept", rec.Code)
	}
}

func TestUndoLogOrderAndPrune(t *testing.T) {
	var log undoLog
	t0 := time.Unix(1000, 0)
	a := log.put(undoEntry{op: store.BulkRemove}, t0)
	b := log.put(undoEntry{op: store.BulkEnable}, t0)
	if a == b || len(a) != 32 {
		t.Fatalf("tokens %q %q, want distinct 32-char tokens", a, b)
	}
	// Same-instant entries evict in insertion order.
	for range undoCap - 1 {
		log.put(undoEntry{}, t0)
	}
	if _, ok := log.take(a, t0); ok {
		t.Error("the oldest entry survived the cap")
	}
	if e, ok := log.take(b, t0); !ok || e.op != store.BulkEnable {
		t.Errorf("second entry = %+v, %v", e, ok)
	}
	if _, ok := log.take(b, t0); ok {
		t.Error("an entry was taken twice")
	}
	c := log.put(undoEntry{}, t0)
	if _, ok := log.take(c, t0.Add(undoTTL-time.Second)); !ok {
		t.Error("an entry expired before its TTL")
	}
	d := log.put(undoEntry{}, t0)
	log.put(undoEntry{}, t0.Add(undoTTL))
	if _, ok := log.take(d, t0.Add(undoTTL)); ok {
		t.Error("an entry outlived its TTL")
	}
}

// Rows retagged or removed since the change are left alone, and the toast says
// how many came back.
func TestUndoIsConditional(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{
		{Name: "Alpha", URL: "https://alpha.test/feed"},
		{Name: "Beta", URL: "https://beta.test/feed"},
	}})
	alpha, beta := feedID(t, db, "Alpha"), feedID(t, db, "Beta")

	token := flashToken(t, bulkPost(t, w, "disable", "", alpha, beta), "Disabled 2 sources")
	// Beta is switched back on by hand after the bulk.
	on := true
	if err := db.SetFeedEnabled(t.Context(), beta, &on); err != nil {
		t.Fatalf("SetFeedEnabled: %v", err)
	}
	body := undoPost(t, w, token)
	msg, tok := flashOf(t, body)
	if msg != "Undid 1 of 2. The rest changed since." || tok != "" {
		t.Errorf("toast = %q token %q, want the partial message and no token", msg, tok)
	}
	if got := enabledOf(ownFeed(t, db, alpha)) + "/" + enabledOf(ownFeed(t, db, beta)); got != "nil/true" {
		t.Errorf("enabled = %s, want nil/true", got)
	}
	// Both named rows are repainted, so the page shows what is true.
	for _, id := range []string{alpha, beta} {
		if !strings.Contains(body, `id="src-`+id+`"`) {
			t.Errorf("undo did not resend %s; body=%s", id, body)
		}
	}

	// A fully applied undo sends no toast at all.
	token = flashToken(t, bulkPost(t, w, "disable", "", alpha), "Disabled 1 source")
	if strings.Contains(undoPost(t, w, token), `id="srcFlash"`) {
		t.Error("a complete undo still shows a toast")
	}
}

// A tag undo needs the very tag the bulk used: swapping it for another by hand
// leaves the row alone.
func TestUndoTagNeedsTheSameTag(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{
		{Name: "Alpha", URL: "https://alpha.test/feed", Tags: []string{"x"}},
	}})
	id := feedID(t, db, "Alpha")
	token := flashToken(t, bulkPost(t, w, "addtag", "new", id), `Added tag "new" to 1 source`)
	bulkPost(t, w, "removetag", "new", id)
	bulkPost(t, w, "addtag", "other", id)

	msg, _ := flashOf(t, undoPost(t, w, token))
	if msg != undoStaleCopy {
		t.Errorf("toast = %q", msg)
	}
	if got := ownFeed(t, db, id).Tags; !slices.Equal(got, []string{"x", "other"}) {
		t.Errorf("tags = %v, want untouched", got)
	}
}

// Export with ids renders just those sources, leaves the defaults out and words
// its title and note for a share.
func TestExportSelected(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{
		Defaults: config.FeedDefaults{Tags: []string{"shared"}},
		Feeds: []config.Feed{
			{Name: "Alpha", URL: "https://alpha.test/feed"},
			{Name: "Beta", URL: "https://beta.test/feed"},
			{Name: "Gone", URL: "https://gone.test/feed"},
		},
	})
	alpha, gone := feedID(t, db, "Alpha"), feedID(t, db, "Gone")
	if err := db.SoftDeleteFeed(t.Context(), gone); err != nil {
		t.Fatalf("SoftDeleteFeed: %v", err)
	}

	out := get(t, w, "/sources/export?ids="+alpha+","+gone)
	for _, want := range []string{
		"Export 1 selected source<", "name: Alpha",
		"Save this as sources.yaml to import these sources somewhere else. Default tags and settings are left out.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q; body=%s", want, out)
		}
	}
	for _, bad := range []string{"name: Beta", "name: Gone", "shared", "tags:"} {
		if strings.Contains(out, bad) {
			t.Errorf("selected export carries %q; body=%s", bad, out)
		}
	}
	two := get(t, w, "/sources/export?ids="+alpha+","+feedID(t, db, "Beta"))
	if !strings.Contains(two, "Export 2 selected sources<") {
		t.Errorf("plural title missing; body=%s", two)
	}

	none := get(t, w, "/sources/export?ids=nosuchid")
	if !strings.Contains(none, "None of the selected sources exist any more.") || strings.Contains(none, "yml") {
		t.Errorf("zero-match export; body=%s", none)
	}

	// Without ids it is still the whole set, defaults included.
	full := get(t, w, "/sources/export")
	if !strings.Contains(full, "name: Beta") || !strings.Contains(full, "shared") ||
		strings.Contains(full, "selected source") {
		t.Errorf("full export changed; body=%s", full)
	}
	many := make([]string, maxBulkIDs+1)
	for i := range many {
		many[i] = "id" + strconv.Itoa(i)
	}
	rec := serve(w, http.MethodGet, "/sources/export?ids="+strings.Join(many, ","), nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("oversized export = %d, want 400", rec.Code)
	}
}

// A removed row says how long ago, from the injected clock, with the full time
// as the tooltip.
func TestRemovedRowSaysHowLongAgo(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}}})
	id := feedID(t, db, "Alpha")
	if err := db.SoftDeleteFeed(t.Context(), id); err != nil {
		t.Fatalf("SoftDeleteFeed: %v", err)
	}
	w.now = func() time.Time { return time.Now().Add(3*time.Hour + time.Minute) }

	data, err := w.sourcesData(t.Context(), "", false)
	if err != nil {
		t.Fatalf("sourcesData: %v", err)
	}
	row := data.Rows[0]
	if got := row.StateText(); got != "Removed 3h ago" {
		t.Errorf("StateText = %q, want Removed 3h ago", got)
	}
	if !strings.HasPrefix(row.StateTitle(), "Removed ") || len(row.StateTitle()) < len("Removed 1 Jan 2026, 00:00") {
		t.Errorf("StateTitle = %q, want the full time", row.StateTitle())
	}
	if !strings.Contains(get(t, w, "/sources/manage"), ">Removed 3h ago<") {
		t.Error("the Manage page does not show Removed 3h ago")
	}

	row.DeletedAt, row.DeletedAgo = time.Time{}, ""
	if row.StateText() != "Removed" || row.StateTitle() != "" {
		t.Errorf("unknown time = %q / %q, want Removed and no title", row.StateText(), row.StateTitle())
	}
}

// The state span of a removed row carries the full removal time as its title,
// while its text stays the relative time from the injected clock.
func TestRemovedRowStateSpanTitleIsFullTime(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}}})
	id := feedID(t, db, "Alpha")
	if err := db.SoftDeleteFeed(t.Context(), id); err != nil {
		t.Fatalf("SoftDeleteFeed: %v", err)
	}
	w.now = func() time.Time { return time.Now().Add(3*time.Hour + time.Minute) }

	data, err := w.sourcesData(t.Context(), "", false)
	if err != nil {
		t.Fatalf("sourcesData: %v", err)
	}
	row := data.Rows[0]
	out := get(t, w, "/sources/manage")
	if !strings.Contains(out, ">Removed 3h ago</span>") {
		t.Fatalf("the Manage page does not show Removed 3h ago; body=%s", out)
	}
	span := tagWith(t, out, "class", "mg-state mg-state--dim")
	wantAttrs(t, "removed state span", span, map[string]string{
		"title": "Removed " + row.DeletedAt.Local().Format("2 Jan 2006, 15:04"),
	})
}

// Rows expose only the tags a source sets itself, never the defaults'.
func TestRowOwnTags(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{
		Defaults: config.FeedDefaults{Tags: []string{"shared"}},
		Feeds: []config.Feed{
			{Name: "Alpha", URL: "https://alpha.test/feed", Tags: []string{"ai", "go"}},
			{Name: "Beta", URL: "https://beta.test/feed"},
		},
	})
	data, err := w.sourcesData(t.Context(), "", false)
	if err != nil {
		t.Fatalf("sourcesData: %v", err)
	}
	got := map[string]string{}
	for _, r := range data.Rows {
		got[r.ID] = r.OwnTagsAttr
	}
	if got[feedID(t, db, "Alpha")] != "ai,go" || got[feedID(t, db, "Beta")] != "" {
		t.Errorf("own tags = %v", got)
	}
}

func TestSingleDisableCarriesUndoOnRequest(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}}})
	id := feedID(t, db, "Alpha")

	plain := postForm(t, w, "/sources/feed/"+id+"/enabled", url.Values{"enabled": {"off"}})
	if strings.Contains(plain, `id="srcFlash"`) {
		t.Errorf("a plain toggle carries a toast; body=%s", plain)
	}
	postForm(t, w, "/sources/feed/"+id+"/enabled", url.Values{"enabled": {"on"}})

	body := postForm(t, w, "/sources/feed/"+id+"/enabled", url.Values{"enabled": {"off"}, "undo": {"1"}})
	token := flashToken(t, body, "Disabled Alpha")
	if got := enabledOf(ownFeed(t, db, id)); got != "false" {
		t.Fatalf("enabled = %s, want false", got)
	}
	undoPost(t, w, token)
	if got := enabledOf(ownFeed(t, db, id)); got != "true" {
		t.Errorf("enabled after undo = %s, want true as before", got)
	}
}

func TestSingleRestoreCarriesUndo(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}}})
	id := feedID(t, db, "Alpha")
	deleteSource(t, w, id)

	token := flashToken(t, postForm(t, w, "/sources/feed/"+id+"/restore", nil), "Restored Alpha")
	if len(mustFeeds(t, db)) != 1 {
		t.Fatal("restore did not bring the feed back")
	}
	undoPost(t, w, token)
	if len(mustFeeds(t, db)) != 0 {
		t.Error("undoing the restore did not remove the feed again")
	}
}

// The noisy callout selects every source it names.
func TestNoisySelectJoinsIDs(t *testing.T) {
	sig := ovSignal{Noisy: []ovCall{{ID: "aa"}, {ID: "bb"}}}
	if got := sig.NoisySelect(); got != "aa,bb" {
		t.Errorf("NoisySelect = %q, want aa,bb", got)
	}
	if got := (ovSignal{}).NoisySelect(); got != "" {
		t.Errorf("empty NoisySelect = %q", got)
	}
}

// Library replies carry the nav failing badge out of band: restoring a source
// that is failing changes the count.
func TestLibraryRestoreReplyCarriesNavBadge(t *testing.T) {
	broken := config.Feed{Name: "Broken Go", URL: libGoURL}
	w, db := libraryTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{broken}})
	recordFailures(t, db, broken, 3, "dial tcp: no such host")
	if err := db.SoftDeleteFeed(t.Context(), feedID(t, db, "Broken Go")); err != nil {
		t.Fatalf("SoftDeleteFeed: %v", err)
	}

	out := postLibrary(t, w, "/sources/library/restore", url.Values{"key": {libKey(libGoURL)}}, nil)
	if !strings.Contains(out, `id="srcNavBadge" hx-swap-oob="true"`) || !strings.Contains(out, `title="1 failing"`) {
		t.Errorf("restore reply has no nav badge; body=%s", out)
	}

	applied := postLibrary(t, w, "/sources/library/apply", url.Values{"pick": {libKey(libKVURL)}}, nil)
	if !strings.Contains(applied, `id="srcNavBadge" hx-swap-oob="true"`) {
		t.Errorf("apply reply has no nav badge; body=%s", applied)
	}
}
