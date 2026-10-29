// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"bytes"
	"database/sql"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// previewDoc plans as one row of every status against a store holding Alpha
// (live) and Gone (removed).
const previewDoc = `feeds:
  - name: Gamma
    url: https://gamma.test/feed
    tags: [ai, news]
  - name: Delta
    url: https://delta.test/feed
  - name: Alpha
    url: https://alpha.test/feed
  - name: Gone
    url: https://gone.test/feed
  - name: Alpha
    url: https://other.test/feed
  - name: Broken
    url: ftp://nope.test/feed
`

func importTestWeb(t *testing.T) (*Web, *store.Store) {
	t.Helper()
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{
		{Name: "Alpha", URL: "https://alpha.test/feed"},
		{Name: "Gone", URL: "https://gone.test/feed"},
	}})
	if err := db.SoftDeleteFeed(t.Context(), feedID(t, db, "Gone")); err != nil {
		t.Fatalf("SoftDeleteFeed: %v", err)
	}
	return w, db
}

// postPreview sends the file as the multipart "file" part, or as the "yaml"
// field when filename is empty.
func postPreview(t *testing.T, w *Web, filename, body string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if filename != "" {
		part, err := mw.CreateFormFile("file", filename)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = part.Write([]byte(body))
	} else {
		_ = mw.WriteField("yaml", body)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/sources/import/preview", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview: status = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	return rec
}

// postApply posts the way the plan form does: multipart, with the file in the
// hidden "yaml" field.
func postApply(t *testing.T, w *Web, yaml string, pick ...string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("yaml", yaml)
	for _, key := range pick {
		_ = mw.WriteField("pick", key)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/sources/import/apply", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("apply: status = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	return rec
}

var pickInputRe = regexp.MustCompile(`<input[^>]*data-plan-pick[^>]*>`)

// pickInputs returns the attributes of every row checkbox, in file order.
func pickInputs(body string) []map[string]string {
	var out []map[string]string
	for _, m := range pickInputRe.FindAllString(body, -1) {
		out = append(out, parseTag(m))
	}
	return out
}

func TestSourcesImportDialogIsTheChooser(t *testing.T) {
	w, _ := importTestWeb(t)
	body := get(t, w, "/sources/import")
	for _, want := range []string{
		"Import sources",
		"Choose a sources.yaml file, or drop one anywhere on this page. You see what changes before anything is added.",
		"Choose a file",
		"Try the sample file",
		"data-src-file",
		"data-modal-close",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("chooser lacks %q; body=%s", want, body)
		}
	}
	if strings.Contains(body, "data-plan>") {
		t.Errorf("chooser shows a plan; body=%s", body)
	}
	if strings.Contains(body, "No file handy") {
		t.Errorf("chooser repeats the sample button as a hint; body=%s", body)
	}
	sample := regexp.MustCompile(`<button[^>]*data-imp-sample[^>]*>`).FindString(body)
	if cls := parseTag(sample)["class"]; cls != "clk-btn" {
		t.Errorf("sample button class = %q, want a plain clk-btn", cls)
	}
}

func TestSourcesImportButtonsOpenTheDialog(t *testing.T) {
	w, _ := importTestWeb(t)
	body := get(t, w, "/sources/manage")
	btn := regexp.MustCompile(`<button[^>]*data-src-import[^>]*>`).FindString(body)
	if btn == "" {
		t.Fatalf("no import button; body=%s", body)
	}
	wantAttrs(t, "import button", parseTag(btn), map[string]string{
		"hx-get": "/sources/import", "hx-target": "#modalTop", "hx-swap": "innerHTML",
	})
}

func TestSourcesImportPreviewShowsEveryStatus(t *testing.T) {
	w, db := importTestWeb(t)
	before := len(mustFeeds(t, db))
	body := postPreview(t, w, "my-feeds.yaml", previewDoc).Body.String()

	if !strings.Contains(body, "Import my-feeds.yaml") {
		t.Errorf("title lacks the file name; body=%s", body)
	}
	counts := []struct {
		status, label string
		n             int
	}{
		{"new", "New", 2}, {"subscribed", "Subscribed", 1}, {"removed", "Removed", 1},
		{"conflict", "Name taken", 1}, {"invalid", "Invalid", 1},
	}
	last := -1
	for _, c := range counts {
		chip := `stchip--` + c.status + `" data-plan-count="` + c.status + `">` + c.label + `<b>` + string(
			rune('0'+c.n),
		) + `</b>`
		i := strings.Index(body, chip)
		if i < 0 {
			t.Errorf("legend lacks %q; body=%s", chip, body)
		}
		if i < last {
			t.Errorf("legend chip %s is out of order", c.status)
		}
		last = i
	}

	// Checkboxes sit on new and removed rows only; new starts ticked.
	picks := pickInputs(body)
	if len(picks) != 3 {
		t.Fatalf("checkboxes = %d, want 3 (2 new + 1 removed); body=%s", len(picks), body)
	}
	wantPick := []struct {
		key     string
		checked bool
	}{
		{"https://gamma.test/feed", true}, {"https://delta.test/feed", true}, {"https://gone.test/feed", false},
	}
	for i, p := range wantPick {
		wantAttrs(t, "pick", picks[i], map[string]string{"value": p.key, "name": "pick"})
		if _, on := picks[i]["checked"]; on != p.checked {
			t.Errorf("pick %s checked = %v, want %v", p.key, on, p.checked)
		}
	}
	for _, status := range []string{"subscribed", "conflict", "invalid"} {
		if !strings.Contains(body, `data-status="`+status+`"`) {
			t.Errorf("no %s row; body=%s", status, body)
		}
	}
	for _, want := range []string{
		"Already in your sources as Alpha",
		"You removed this source. Tick to restore it.",
		"Name taken by a source with a different URL.",
		`tag tag--clip tag--c`,
		"Select all new",
		"Add 2",
		`name="yaml"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("plan lacks %q; body=%s", want, body)
		}
	}
	apply := regexp.MustCompile(`<button[^>]*data-plan-apply[^>]*>`).FindString(body)
	if apply == "" {
		t.Fatalf("no apply button; body=%s", body)
	}
	lacksAttr(t, "apply button", parseTag(apply), "disabled")

	if got := len(mustFeeds(t, db)); got != before {
		t.Errorf("a preview wrote: feeds %d -> %d", before, got)
	}
	if got := len(mustDeleted(t, db)); got != 1 {
		t.Errorf("a preview restored a removed feed: deleted = %d", got)
	}
}

func TestSourcesImportPreviewAcceptsSourcesKeyAndPlainField(t *testing.T) {
	w, _ := importTestWeb(t)
	doc := "sources:\n  - name: Gamma\n    url: https://gamma.test/feed\n"
	body := postPreview(t, w, "", doc).Body.String()
	if !strings.Contains(body, `<h3 class="plan__title">Preview</h3>`) {
		t.Errorf("a body without a name should be titled Preview; body=%s", body)
	}
	if len(pickInputs(body)) != 1 || strings.Contains(body, `role="alert"`) {
		t.Errorf("sources: file did not preview; body=%s", body)
	}
}

func TestSourcesImportPreviewErrorsStayInTheDialog(t *testing.T) {
	w, db := importTestWeb(t)
	cases := map[string]struct{ file, body, want string }{
		"no sources": {
			"empty.yaml",
			"feeds: []\n",
			"No sources found in empty.yaml. Expected a sources: or feeds: list with a name and url for each.",
		},
		"parse error":  {"bad.yaml", "feeds: [unclosed\n", "parse feeds file"},
		"not a feeds":  {"cfg.yaml", "inference:\n  provider: ollama\n", "not a feeds file: no feeds or sources list"},
		"both keys":    {"both.yaml", "feeds: []\nsources: []\n", "has both feeds: and sources:"},
		"nothing sent": {"", "", "choose a feeds YAML file to import"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			body := postPreview(t, w, c.file, c.body).Body.String()
			if !strings.Contains(body, `role="alert"`) || !strings.Contains(body, c.want) {
				t.Errorf("want an alert with %q; body=%s", c.want, body)
			}
			if strings.Contains(body, "data-plan-apply") {
				t.Errorf("an error state offers an apply; body=%s", body)
			}
			if !strings.Contains(body, `hx-get="/sources/import"`) {
				t.Errorf("an error state lacks a way back; body=%s", body)
			}
		})
	}
	if got := len(mustFeeds(t, db)); got != 1 {
		t.Errorf("a rejected file changed the store: feeds = %d", got)
	}
}

func TestSourcesImportApplyAddsTheTickedRows(t *testing.T) {
	w, db := importTestWeb(t)
	rec := postApply(t, w, previewDoc, "https://gamma.test/feed", "https://gone.test/feed")
	body := rec.Body.String()

	if !strings.Contains(body, `hx-swap-oob="innerHTML:#modalTop"`) {
		t.Errorf("the dialog stays open; body=%s", body)
	}
	if !strings.Contains(body, `id="srcBody"`) || !strings.Contains(body, "Added 1 source, restored 1") {
		t.Errorf("no repainted list or banner; body=%s", body)
	}
	if !strings.Contains(body, `role="status"`) || strings.Contains(body, "skipped") {
		t.Errorf("banner is wrong; body=%s", body)
	}
	live := map[string]bool{}
	for _, f := range mustFeeds(t, db) {
		live[f.Name] = true
	}
	if !live["Gamma"] || !live["Gone"] || live["Delta"] {
		t.Errorf("live feeds = %v, want Alpha, Gamma and Gone but not Delta", live)
	}
}

func TestSourcesImportApplyCountsBannerWording(t *testing.T) {
	cases := []struct {
		res  store.ApplyResult
		want string
	}{
		{store.ApplyResult{Added: 1}, "Added 1 source"},
		{store.ApplyResult{Added: 3}, "Added 3 sources"},
		{store.ApplyResult{Restored: 2}, "Restored 2"},
		{store.ApplyResult{Added: 2, Restored: 1}, "Added 2 sources, restored 1"},
		{store.ApplyResult{Added: 1, Skipped: 2}, "Added 1 source, skipped 2 that changed since the preview"},
	}
	for _, c := range cases {
		if got := importBanner(c.res); got != c.want {
			t.Errorf("importBanner(%+v) = %q, want %q", c.res, got, c.want)
		}
	}
}

func TestSourcesImportApplySkipsStaleAndUnknownPicks(t *testing.T) {
	w, db := importTestWeb(t)
	// Alpha is already subscribed and the other key names no row: both are
	// stale picks, counted and not applied.
	body := postApply(
		t,
		w,
		previewDoc,
		"https://delta.test/feed",
		"https://alpha.test/feed",
		"https://nowhere.test/feed",
	).Body.String()
	if !strings.Contains(body, "Added 1 source, skipped 2 that changed since the preview") {
		t.Errorf("banner lacks the skip count; body=%s", body)
	}
	if got := len(mustFeeds(t, db)); got != 2 {
		t.Errorf("live feeds = %d, want Alpha and Delta", got)
	}
}

func TestSourcesImportApplyNothingPickedRerendersThePlan(t *testing.T) {
	w, db := importTestWeb(t)
	rec := postApply(t, w, previewDoc)
	body := rec.Body.String()
	if !strings.Contains(body, "Nothing selected.") || !strings.Contains(body, "sdet__err") ||
		!strings.Contains(body, "data-plan-apply") {
		t.Errorf("want the plan back with an error; body=%s", body)
	}
	if rec.Header().Get("HX-Retarget") != "#modalTop" {
		t.Errorf("HX-Retarget = %q, want #modalTop", rec.Header().Get("HX-Retarget"))
	}
	if strings.Contains(body, `hx-swap-oob`) {
		t.Errorf("a refused apply must not close the dialog; body=%s", body)
	}
	if got := len(mustFeeds(t, db)); got != 1 {
		t.Errorf("nothing picked changed the store: feeds = %d", got)
	}
}

func TestSourcesImportApplyRejectsABadFile(t *testing.T) {
	w, db := importTestWeb(t)
	for name, c := range map[string]struct{ yaml, want string }{
		"missing":   {"", "choose a feeds YAML file to import"},
		"malformed": {"feeds: [", "parse feeds file"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := postApply(t, w, c.yaml, "https://gamma.test/feed")
			body := rec.Body.String()
			if !strings.Contains(body, `role="alert"`) || !strings.Contains(body, c.want) {
				t.Errorf("want an alert with %q; body=%s", c.want, body)
			}
			if rec.Header().Get("HX-Retarget") != "#modalTop" {
				t.Errorf("the error would land in the list; HX-Retarget = %q", rec.Header().Get("HX-Retarget"))
			}
		})
	}
	if got := len(mustFeeds(t, db)); got != 1 {
		t.Errorf("a rejected apply changed the store: feeds = %d", got)
	}
}

func TestSourcesImportRoutesRejectAnOversizeBody(t *testing.T) {
	w, _ := importTestWeb(t)
	big := "feeds: []\n# " + strings.Repeat("x", sourceImportLimit)
	for name, body := range map[string]string{
		"preview file":  postPreview(t, w, "big.yaml", big).Body.String(),
		"preview field": postPreview(t, w, "", big).Body.String(),
		"apply":         postApply(t, w, big, "https://gamma.test/feed").Body.String(),
	} {
		if !strings.Contains(body, "too large") || !strings.Contains(body, `role="alert"`) {
			t.Errorf("%s: want the too-large alert; body=%.300s", name, body)
		}
	}
}

// The sample the chooser offers is the documented example, and it plans as
// new on a fresh install.
func TestSourcesSampleIsTheDocumentedExample(t *testing.T) {
	sample, err := staticFS.ReadFile("static/sources-sample.yaml")
	if err != nil {
		t.Fatalf("read sample: %v", err)
	}
	example, err := os.ReadFile("../../docs/examples/feeds.yaml")
	if err != nil {
		t.Fatalf("read example: %v", err)
	}
	if !bytes.Equal(sample, example) {
		t.Error("static/sources-sample.yaml differs from docs/examples/feeds.yaml")
	}

	w, db := sourcesTestWeb(t, config.FeedsDoc{})
	doc, err := config.ParseFeeds(sample)
	if err != nil {
		t.Fatalf("ParseFeeds: %v", err)
	}
	rows, err := db.PlanImport(t.Context(), *doc)
	if err != nil {
		t.Fatalf("PlanImport: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("the sample plans no rows")
	}
	for _, r := range rows {
		if r.Status != store.PlanNew {
			t.Errorf("%s plans as %s (%s), want new", r.Feed.Name, r.Status, r.Reason)
		}
	}

	body := postPreview(t, w, "sources-sample.yaml", string(sample)).Body.String()
	if len(pickInputs(body)) != len(rows) {
		t.Errorf("preview shows %d checkboxes, want %d", len(pickInputs(body)), len(rows))
	}
}

// hiddenYAML is the value of the plan form's hidden "yaml" field, unescaped.
func hiddenYAML(t *testing.T, body string) string {
	t.Helper()
	tag := regexp.MustCompile(`<input[^>]*name="yaml"[^>]*>`).FindString(body)
	if tag == "" {
		t.Fatalf("no hidden yaml field; body=%s", body)
	}
	return parseTag(tag)["value"]
}

func TestSourcesImportCarriesTheFileThroughThePlanForm(t *testing.T) {
	w, db := importTestWeb(t)
	const file = `# friends <3 & "family"
feeds:
  - name: 'Tom''s <Feed> & "Co"'   # trailing comment
    url: https://rt.test/feed?a=1&b=2
    tags: [x, y]
  - name: "Line\nBreak"
    url: https://rt2.test/feed
`
	body := postPreview(t, w, "rt.yaml", file).Body.String()
	carried := hiddenYAML(t, body)
	if carried != file {
		t.Fatalf("hidden yaml = %q, want the file back byte for byte", carried)
	}

	doc, err := config.ParseFeeds([]byte(file))
	if err != nil {
		t.Fatalf("ParseFeeds: %v", err)
	}
	keys := make([]string, 0, len(doc.Feeds))
	for _, f := range doc.Feeds {
		keys = append(keys, config.FeedURLKey(f.URL))
	}
	postApply(t, w, carried, keys...)

	byName := make(map[string]config.Feed)
	for _, f := range mustFeeds(t, db) {
		byName[f.Name] = f
	}
	for _, want := range doc.Feeds {
		got, ok := byName[want.Name]
		if !ok {
			t.Fatalf("no stored feed %q; stored=%+v", want.Name, byName)
		}
		if got.URL != want.URL || strings.Join(got.Tags, ",") != strings.Join(want.Tags, ",") {
			t.Errorf("stored %q = %+v, want url %q tags %v", want.Name, got, want.URL, want.Tags)
		}
	}
}

func TestSourcesImportPlanFormPostsMultipart(t *testing.T) {
	w, _ := importTestWeb(t)
	body := postPreview(t, w, "f.yaml", previewDoc).Body.String()
	form := regexp.MustCompile(`<form[^>]*class="plan__form"[^>]*>`).FindString(body)
	wantAttrs(t, "plan form", parseTag(form), map[string]string{
		"hx-post": "/sources/import/apply", "hx-encoding": "multipart/form-data",
	})
}

func TestSourcesImportRemovedNameConflictHasItsOwnReason(t *testing.T) {
	w, _ := importTestWeb(t)
	body := postPreview(t, w, "f.yaml", "feeds:\n  - name: Gone\n    url: https://elsewhere.test/feed\n").Body.String()
	if !strings.Contains(body, `data-status="conflict"`) ||
		!strings.Contains(body, "A removed source uses this name.") {
		t.Errorf("want a conflict naming the removed holder; body=%s", body)
	}
	if strings.Contains(body, "data-plan-pick") {
		t.Errorf("a conflict row must not be selectable; body=%s", body)
	}
}

func TestSourcesImportLegendDimsEmptyStatuses(t *testing.T) {
	w, _ := importTestWeb(t)
	body := postPreview(t, w, "f.yaml", "feeds:\n  - name: Solo\n    url: https://solo.test/feed\n").Body.String()
	chip := func(status string) string {
		return regexp.MustCompile(`<span[^>]*data-plan-count="` + status + `"[^>]*>`).FindString(body)
	}
	if !strings.Contains(chip("new"), "stchip--new\"") || strings.Contains(chip("new"), "stchip--zero") {
		t.Errorf("populated chip is dimmed: %s", chip("new"))
	}
	for _, st := range []string{"subscribed", "removed", "conflict", "invalid"} {
		if !strings.Contains(chip(st), "stchip--zero") {
			t.Errorf("%s chip is not dimmed at zero: %s", st, chip(st))
		}
	}
}

func TestSourcesImportApplyAllStalePicksSaysSo(t *testing.T) {
	w, db := importTestWeb(t)
	if _, err := db.AddFeed(t.Context(), config.Feed{Name: "Gamma", URL: "https://gamma-moved.test/feed"}); err != nil {
		t.Fatalf("AddFeed: %v", err)
	}
	// The preview offered Gamma; its name has been taken since.
	rec := postApply(t, w, previewDoc, "https://gamma.test/feed")
	body := rec.Body.String()
	if !strings.Contains(body, "Everything you picked changed since the preview. Review the list and try again.") {
		t.Errorf("want the all-stale wording; body=%s", body)
	}
	if strings.Contains(body, "Nothing selected.") {
		t.Errorf("stale picks were worded as nothing selected; body=%s", body)
	}
	if rec.Header().Get("HX-Retarget") != "#modalTop" {
		t.Errorf("HX-Retarget = %q, want #modalTop", rec.Header().Get("HX-Retarget"))
	}
}

// A writer that slips a name in after the apply's own plan is only caught by
// the unique index. A trigger on a second connection stands in for it, with
// the message SQLite gives the real violation.
func TestSourcesImportApplyNameTakenRerendersThePlan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "race.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	w := New(db, config.Static{}, testIngestManager(t, db))

	other, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open second connection: %v", err)
	}
	t.Cleanup(func() { _ = other.Close() })
	if _, err := other.ExecContext(t.Context(), `CREATE TRIGGER race BEFORE INSERT ON feeds WHEN NEW.name = 'Racy'
		BEGIN SELECT RAISE(ABORT, 'UNIQUE constraint failed: feeds.name'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	const file = "feeds:\n  - name: Racy\n    url: https://racy.test/feed\n  - name: Calm\n    url: https://calm.test/feed\n"
	rec := postApply(t, w, file, "https://calm.test/feed", "https://racy.test/feed")
	body := rec.Body.String()
	if !strings.Contains(body, "Something changed while importing. Review the list and try again.") ||
		!strings.Contains(body, "data-plan-apply") {
		t.Errorf("want the plan back with the changed-meanwhile error; body=%s", body)
	}
	if rec.Header().Get("HX-Retarget") != "#modalTop" {
		t.Errorf("HX-Retarget = %q, want #modalTop", rec.Header().Get("HX-Retarget"))
	}
	feeds, err := db.Feeds(t.Context())
	if err != nil {
		t.Fatalf("Feeds: %v", err)
	}
	if len(feeds) != 0 {
		t.Errorf("feeds = %+v, want the whole import rolled back", feeds)
	}
}

func TestSourcesImportApplyAcceptsAURLEncodedForm(t *testing.T) {
	w, db := importTestWeb(t)
	form := url.Values{"yaml": {previewDoc}, "pick": {"https://delta.test/feed"}}
	req := httptest.NewRequest(http.MethodPost, "/sources/import/apply", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Added 1 source") {
		t.Errorf("status = %d, body=%s", rec.Code, rec.Body)
	}
	if got := len(mustFeeds(t, db)); got != 2 {
		t.Errorf("live feeds = %d, want Alpha and Delta", got)
	}
}

// TestSourcePlanCancelWithoutGet renders the plan with no CancelGet: each
// Cancel is a client-side button with no htmx attributes, in both the error
// and the normal state.
func TestSourcePlanCancelWithoutGet(t *testing.T) {
	cancelRe := regexp.MustCompile(`<button[^>]*data-plan-cancel[^>]*>`)
	for name, p := range map[string]*importPlan{
		"error":  {Title: "Preview", Err: "bad file"},
		"normal": {Title: "Preview", Counts: []planCount{{Status: "new", Label: "New", N: 1}}},
	} {
		var b bytes.Buffer
		if err := feedTmpl.ExecuteTemplate(&b, "sourcePlan", p); err != nil {
			t.Fatalf("%s: render sourcePlan: %v", name, err)
		}
		tags := cancelRe.FindAllString(b.String(), -1)
		if len(tags) != 1 {
			t.Fatalf("%s: got %d data-plan-cancel buttons, want 1; body=%s", name, len(tags), b.String())
		}
		got := parseTag(tags[0])
		wantAttrs(t, name+" cancel", got, map[string]string{"data-plan-cancel": present, "type": "button"})
		lacksAttr(t, name+" cancel", got, "hx-get")
		lacksAttr(t, name+" cancel", got, "hx-target")
	}
}
