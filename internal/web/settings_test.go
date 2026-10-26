// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/DanielBlei/rabbithole/internal/rank"
	"github.com/DanielBlei/rabbithole/internal/store"
)

var allTabs = []string{"general", "profiles", "scoring", "appearance", "widgets", "advanced"}

func saveConfig(t *testing.T, db *store.Store, content, source string, expected int) {
	t.Helper()
	if _, err := db.SaveConfig(t.Context(), content, "h"+content+source, source, expected); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
}

func TestSettingsEveryTabRendersWithItsTabActive(t *testing.T) {
	w, _ := configTestWeb(t)
	for _, tab := range allTabs {
		body := get(t, w, "/settings/"+tab)
		if !strings.Contains(body, `class="stp-tab is-on" href="/settings/`+tab+`"`) {
			t.Errorf("%s: its tab is not marked active", tab)
		}
		for _, other := range allTabs {
			paneRE := regexp.MustCompile(`<section class="stp-pane" data-stp-pane="` + other + `"( hidden)?>`)
			pane := paneRE.FindStringSubmatch(body)
			if pane == nil {
				t.Errorf("%s: no pane for %s", tab, other)
				continue
			}
			if hidden := pane[1] != ""; hidden == (other == tab) {
				t.Errorf("%s: pane %s hidden = %v", tab, other, hidden)
			}
		}
		for _, link := range allTabs {
			if !strings.Contains(body, `href="/settings/`+link+`" data-stp-tab="`+link+`"`) {
				t.Errorf("%s: the bar lacks a real link to %s", tab, link)
			}
		}
	}
}

// The open tab names itself in the prompt readout before any script runs, and the
// page keeps one heading for screen readers.
func TestSettingsHeadNamesTheOpenTab(t *testing.T) {
	w, _ := configTestWeb(t)
	body := get(t, w, "/settings/scoring")
	for _, want := range []string{
		`<h1 class="stp-sr">Settings</h1>`,
		`<span data-stp-cmdtab>scoring</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("settings head lacks %q", want)
		}
	}
}

func TestSettingsTabOrder(t *testing.T) {
	w, _ := configTestWeb(t)
	body := get(t, w, "/settings/general")
	last := -1
	for _, tab := range allTabs {
		i := strings.Index(body, `data-stp-tab="`+tab+`"`)
		if i < last {
			t.Errorf("tab %s is out of order", tab)
		}
		last = i
	}
}

func TestSettingsIndexRedirectsAndUnknownTabIs404(t *testing.T) {
	w, _ := configTestWeb(t)
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/settings/general" {
		t.Errorf("GET /settings = %d to %q", rec.Code, rec.Header().Get("Location"))
	}
	for _, path := range []string{"/settings/nope", "/settings/account", "/settings/general/x"} {
		rec := httptest.NewRecorder()
		w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
	}
}

func TestSettingsSitsBehindTheLoginGate(t *testing.T) {
	b := newBrowser(t, gatedWeb(t))
	wantRedirect(t, b.get("/settings/scoring"), "/login?next=%2Fsettings%2Fscoring")
	wantRedirect(t, b.get("/settings"), "/login?next=%2Fsettings")
	b.login("alice", "correct horse")
	wantStatus(t, b.get("/settings/account"), http.StatusOK)
	if body := b.get("/settings/general").Body.String(); !strings.Contains(body, `data-stp-tab="account"`) {
		t.Error("the Account tab is missing once sign-in is enabled")
	}
}

func TestSettingsConfigTabsShareOneForm(t *testing.T) {
	w, _ := configTestWeb(t)
	for _, tab := range []string{"general", "scoring", "advanced"} {
		body := get(t, w, "/settings/"+tab)
		start := strings.Index(body, `<form class="stp-form"`)
		end := strings.Index(body[start:], "</form>")
		if start < 0 || end < 0 {
			t.Fatalf("%s: no config form", tab)
		}
		form := body[start : start+end]
		if n := strings.Count(body, `<form class="stp-form"`); n != 1 {
			t.Errorf("%s: %d config forms, want 1", tab, n)
		}
		for _, in := range []string{"general", "scoring", "advanced"} {
			if !strings.Contains(form, `data-stp-pane="`+in+`"`) {
				t.Errorf("%s: pane %s is not inside the form", tab, in)
			}
		}
		for _, out := range []string{"profiles", "appearance", "widgets"} {
			if strings.Contains(form, `data-stp-pane="`+out+`"`) {
				t.Errorf("%s: pane %s is inside the config form", tab, out)
			}
		}
		if !strings.Contains(form, `data-stp-savebar`) {
			t.Errorf("%s: the save bar slot is not in the form", tab)
		}
	}
}

func TestSettingsGeneralShowsEffectiveValues(t *testing.T) {
	w, db := configTestWeb(t)
	body := get(t, w, "/settings/general")
	if !strings.Contains(body, "From your computer login") ||
		!strings.Contains(body, `name="user" value="`+promptUser("")+`"`) {
		t.Error("an unset prompt name should show the login name with its note")
	}
	saveConfig(t, db, "user: ada\n", store.ConfigSourceUI, 0)
	body = get(t, w, "/settings/general")
	shownAsSet := strings.Contains(body, `name="user" value="ada"`)
	if !shownAsSet || strings.Contains(body, "From your computer login") {
		t.Errorf("a set prompt name should show as itself; body=%s", body)
	}
	for _, want := range []string{`hx-get="/sources"`, `data-stp-import`, `data-stp-feedcount`} {
		if !strings.Contains(body, want) {
			t.Errorf("General lacks %q", want)
		}
	}
	for _, gone := range []string{"Digest folder", "digest_dir", "Look back", "ingest.since"} {
		if strings.Contains(body, gone) {
			t.Errorf("the page still shows %q", gone)
		}
	}
}

// rowOff reports whether the field's row is rendered hidden for the provider in
// force, and whether the row exists at all.
func rowOff(body, key string) (off, found bool) {
	m := regexp.MustCompile(`<div class="(stp-row[^"]*)" data-fld="` + regexp.QuoteMeta(key) + `"`).
		FindStringSubmatch(body)
	return m != nil && strings.Contains(" "+m[1]+" ", " is-off "), m != nil
}

func TestSettingsScoringHidesFieldsTheProviderIgnores(t *testing.T) {
	w, db := configTestWeb(t)
	scoring := func() string { return get(t, w, "/settings/scoring") }
	ollama := scoring()
	for _, key := range []string{"inference.provider", "inference.host", "inference.model", "inference.api_key", "inference.think"} {
		if off, found := rowOff(ollama, key); !found || off {
			t.Errorf("ollama scoring should show %s (found=%v off=%v)", key, found, off)
		}
	}
	if off, _ := rowOff(ollama, "inference.effort"); !off {
		t.Error("ollama does not read effort")
	}
	if strings.Contains(ollama, "does not read") {
		t.Error("hidden rows carry no explanatory note")
	}
	saveConfig(t, db, "inference:\n  provider: openai\n", store.ConfigSourceUI, 0)
	if off, _ := rowOff(scoring(), "inference.effort"); off {
		t.Error("openai reads effort")
	}
	saveConfig(t, db, "inference:\n  provider: heuristic\n", store.ConfigSourceUI, 1)
	heuristic := scoring()
	for _, key := range []string{"inference.host", "inference.model", "inference.api_key", "inference.think", "inference.effort"} {
		if off, _ := rowOff(heuristic, key); !off {
			t.Errorf("heuristic scoring still shows %s", key)
		}
	}
	if strings.Contains(heuristic, "rabbithole config import") {
		t.Error("the editing note is stale: Scoring is editable now")
	}
}

type fakeEnv map[string]string

func (f fakeEnv) Source(name string) string {
	if v, ok := f[name]; ok {
		return v
	}
	return "missing"
}

func TestSettingsKeyChipNeverLeaksTheValue(t *testing.T) {
	const secret = "sk-live-PLEASE-DO-NOT-PRINT"
	w, db := configTestWeb(t)
	saveConfig(t, db, "inference:\n  api_key: $RABBITHOLE_INFERENCE_API_KEY\n", store.ConfigSourceUI, 0)
	t.Setenv("RABBITHOLE_INFERENCE_API_KEY", secret)

	tests := []struct {
		name string
		env  EnvSource
		want string
	}{
		{"process environment", nil, "Set in environment"},
		{"environment", fakeEnv{"RABBITHOLE_INFERENCE_API_KEY": "environment"}, "Set in environment"},
		{"env file", fakeEnv{"RABBITHOLE_INFERENCE_API_KEY": "envfile"}, "Set in .env"},
		{"missing", fakeEnv{}, "Missing"},
	}
	for _, tt := range tests {
		w.env = tt.env
		body := get(t, w, "/settings/scoring")
		if !strings.Contains(body, tt.want) || !strings.Contains(body, "$RABBITHOLE_INFERENCE_API_KEY") {
			t.Errorf("%s: chip %q or the variable name missing", tt.name, tt.want)
		}
		if strings.Contains(body, secret) {
			t.Errorf("%s: the key value reached the page", tt.name)
		}
	}
}

func TestSettingsHistoryNamesSourcesInWords(t *testing.T) {
	w, db := configTestWeb(t)
	saveConfig(t, db, "user: a\n", "import:/home/x/c2.yaml", 0)
	saveConfig(t, db, "user: b\n", store.ConfigSourceUI, 1)
	if _, err := db.RollbackConfig(t.Context(), 1, 2); err != nil {
		t.Fatal(err)
	}
	body := get(t, w, "/settings/advanced")
	for _, want := range []string{"Imported from c2.yaml", "Settings page", "Rollback to version 1", "Version 3", `stp-chip--ok">Active`} {
		if !strings.Contains(body, want) {
			t.Errorf("history lacks %q", want)
		}
	}
	if strings.Contains(body, "/home/x") {
		t.Error("the import path leaked past its file name")
	}
	pane := body[strings.Index(body, `data-stp-pane="advanced"`):]
	pane = pane[:strings.Index(pane, "</form>")]
	if n := strings.Count(pane, `stp-chip--ok">Active`); n != 1 {
		t.Errorf("%d versions marked active, want 1", n)
	}
}

func TestSettingsAdvancedShowsStoreAndTuning(t *testing.T) {
	w, _ := configTestWeb(t)
	body := get(t, w, "/settings/advanced")
	if !strings.Contains(body, "Not reported") {
		t.Error("without store info the page should say so")
	}
	for _, want := range []string{"Structured replies", "Leave on unless scoring fails.", "Context window", "Reply budget per article", "Nothing saved yet"} {
		if !strings.Contains(body, want) {
			t.Errorf("Advanced lacks %q", want)
		}
	}
	w.SetStoreInfo(StoreInfo{
		Engine: "SQLite", Location: "~/.local/share/rabbithole/rabbithole.db", Origin: "the default location",
	})
	body = get(t, w, "/settings/advanced")
	for _, want := range []string{"SQLite", "~/.local/share/rabbithole/rabbithole.db", "the default location"} {
		if !strings.Contains(body, want) {
			t.Errorf("store info lacks %q", want)
		}
	}
}

func TestSettingsManagedNoteOnlyWithTheSetter(t *testing.T) {
	w, _ := configTestWeb(t)
	if strings.Contains(get(t, w, "/settings/scoring"), "Managed by") {
		t.Error("managed note shown without --config")
	}
	w.SetManagedBy("./rabbithole.yaml")
	body := get(t, w, "/settings/general")
	if n := strings.Count(body, "Managed by"); n != 2 {
		t.Errorf("managed note appears %d times, want 2 (Scoring and Advanced)", n)
	}
	general := body[strings.Index(body, `data-stp-pane="general"`):strings.Index(body, `data-stp-pane="scoring"`)]
	if strings.Contains(general, "Managed by") {
		t.Error("General should not carry the note")
	}
}

func TestPopupSettingsAreGone(t *testing.T) {
	w, _ := configTestWeb(t)
	for _, path := range []string{"/feed", "/maze", "/settings/general"} {
		body := get(t, w, path)
		if !strings.Contains(body, `id="settingsToggle" href="/settings"`) {
			t.Errorf("%s: the gear does not link to /settings", path)
		}
		if !strings.Contains(body, `navrail__item navrail__item--cmd" href="/settings"`) {
			t.Errorf("%s: the side menu item does not link to /settings", path)
		}
		for _, gone := range []string{"settingsModal", "data-stg-open", "stg__", "modal--stg", "/static/js/resize.js"} {
			if strings.Contains(body, gone) {
				t.Errorf("%s: leftover popup markup %q", path, gone)
			}
		}
	}
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/config", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /config = %d, want 404", rec.Code)
	}
}

func TestSettingsAppearanceKeepsTheSharedLookPicker(t *testing.T) {
	w, _ := configTestWeb(t)
	body := get(t, w, "/settings/appearance")
	for _, want := range []string{"look__pv", `data-theme-pick`, `data-font-step="ui"`, `data-font-step="bodySize"`} {
		if !strings.Contains(body, want) {
			t.Errorf("Appearance lacks %q", want)
		}
	}
	w2 := get(t, w, "/settings/widgets")
	for _, want := range []string{"data-clk-fmt", "data-wx-toggle", `id="wxLocSearch"`, `data-wx-pick="side"`} {
		if !strings.Contains(w2, want) {
			t.Errorf("Widgets lacks %q", want)
		}
	}
}

var (
	tipRe  = regexp.MustCompile(`id="(tip-[^"]+)" role="tooltip">([^<]*)</span>`)
	helpRe = regexp.MustCompile(`<p class="stp-row__h">([^<]+)</p>|<small>([^<]+)</small>|</h3><p>([^<]+)</p>`)
)

// settingsPages renders every tab the page can show.
func settingsPages(t *testing.T, w *Web) map[string]string {
	t.Helper()
	pages := map[string]string{}
	for _, tab := range []string{"general", "profiles", "scoring", "appearance", "widgets", "advanced"} {
		pages[tab] = get(t, w, "/settings/"+tab)
	}
	return pages
}

func TestSettingsHelpBubblesRenderWhereSet(t *testing.T) {
	w, _ := configTestWeb(t)
	pages := settingsPages(t, w)
	for tab, ids := range map[string][]string{
		"general":    {"feeds", "reset-all"},
		"profiles":   {"profile-active", "profiles"},
		"scoring":    {keyProvider, keyAPIKey, "test", "inference.think", "inference.effort", "inference.batch_size", "inference.max_parallel"},
		"appearance": {"type"},
		"widgets":    {"weather"},
		"advanced":   {keySystem, "inference.structured_output", "inference.model_tuning.num_ctx", "tuning", "versions", "backup"},
	} {
		for _, key := range ids {
			if id := tipFor(key).ID; !strings.Contains(pages[tab], `id="`+id+`" role="tooltip"`) {
				t.Errorf("%s lacks bubble %s", tab, id)
			}
		}
	}
}

func TestSettingsEveryTipIsOnAPageOnce(t *testing.T) {
	w, _ := configTestWeb(t)
	// Every pane is in one document, so one render holds them all.
	body := get(t, w, "/settings/general")
	for key := range tips {
		if key == "stay" || key == "everywhere" {
			continue // the Account tab, covered in account_test.go
		}
		if n := strings.Count(body, `id="`+tipFor(key).ID+`" role="tooltip"`); n != 1 {
			t.Errorf("tip %q is on the page %d times, want 1", key, n)
		}
	}
	if tipFor("no such key") != nil {
		t.Error("an unknown key should have no bubble")
	}
}

func TestSettingsRowNeverExplainsTwice(t *testing.T) {
	w, _ := configTestWeb(t)
	for tab, body := range settingsPages(t, w) {
		var bubbles []string
		for _, m := range tipRe.FindAllStringSubmatch(body, -1) {
			bubbles = append(bubbles, m[2])
		}
		for _, m := range helpRe.FindAllStringSubmatch(body, -1) {
			help := strings.TrimSpace(m[1] + m[2] + m[3])
			if len(help) > 70 {
				t.Errorf("%s: visible helper over 70 characters: %q", tab, help)
			}
			for _, b := range bubbles {
				if strings.Contains(b, help) || strings.Contains(help, b) {
					t.Errorf("%s: %q is both visible and in a bubble", tab, help)
				}
			}
		}
	}
}

func TestSettingsFieldHelpFitsOneLine(t *testing.T) {
	for _, f := range fieldSpecs {
		if len(f.Help) > 70 {
			t.Errorf("%s helper is %d characters, move it into a tip", f.Key, len(f.Help))
		}
		if tip, ok := tips[f.Key]; ok && (strings.Contains(tip, f.Help) || f.Help == "") {
			t.Errorf("%s: helper empty or repeated in its tip", f.Key)
		}
	}
}

func TestSettingsKeywordsNoteFollowsTheProvider(t *testing.T) {
	w, db := configTestWeb(t)
	const note = "Keywords scores by counting your profile"
	body := get(t, w, "/settings/scoring")
	if !strings.Contains(body, note) || !strings.Contains(body, `stp-callout--kw is-off`) {
		t.Error("the Keywords note should be on the page but hidden for the default provider")
	}
	saveConfig(t, db, "inference:\n  provider: heuristic\n", store.ConfigSourceUI, 0)
	body = get(t, w, "/settings/scoring")
	if !strings.Contains(body, `stp-callout--kw" role="note" data-applies="heuristic"`) {
		t.Error("the Keywords note is hidden for the heuristic provider")
	}
}

func TestSettingsAdvancedRepliesAreLockedAndShowTheBuiltInText(t *testing.T) {
	w, _ := configTestWeb(t)
	body := get(t, w, "/settings/advanced")
	for _, want := range []string{"data-stp-replies", "is-locked", "data-stp-replylock", "stp-row--stack",
		"Changes here can stop scoring from working", "readonly", rank.DefaultSystemPrompt[:30]} {
		if !strings.Contains(body, want) {
			t.Errorf("advanced lacks %q", want)
		}
	}
	if strings.Index(body, "Model tuning") > strings.Index(body, "data-stp-replies") {
		t.Error("Model tuning should come before Replies")
	}
}

func TestSettingsResetAllDialogOffersEveryPillButAccount(t *testing.T) {
	w, _ := configTestWeb(t)
	body := get(t, w, "/settings/general")
	for _, want := range []string{"data-stp-resetall", "data-stp-pillall", `data-stp-pill="general"`, `data-stp-pill="scoring"`,
		`data-stp-pill="advanced"`, `data-stp-pill="profile"`, `data-stp-pill="appearance"`, `data-stp-pill="widgets"`,
		`data-default-profile="builtin-default"`, "never included"} {
		if !strings.Contains(body, want) {
			t.Errorf("reset dialog lacks %q", want)
		}
	}
	if strings.Contains(body, `data-stp-pill="account"`) {
		t.Error("account must not be resettable")
	}
}

func TestSettingsResetAllIsACheckboxList(t *testing.T) {
	w, _ := configTestWeb(t)
	body := get(t, w, "/settings/general")
	dlg := body[strings.Index(body, "data-stp-resetdlg"):]
	if n := strings.Count(dlg, `<input type="checkbox" data-stp-pill`); n != 7 {
		t.Errorf("reset list has %d checkboxes (All plus six), want 7", n)
	}
	for _, unwanted := range []string{"stp-pills", "aria-pressed"} {
		if strings.Contains(dlg[:strings.Index(dlg, "data-stp-resetgo")], unwanted) {
			t.Errorf("reset chooser still uses toggle buttons (%q)", unwanted)
		}
	}
	for _, want := range []string{`<fieldset class="stp-checks">`, `data-stp-resetsum role="status"`, `data-stp-resetgo disabled`} {
		if !strings.Contains(dlg, want) {
			t.Errorf("reset chooser lacks %q", want)
		}
	}
}

func TestSettingsWidgetsUseThemedChoices(t *testing.T) {
	w, _ := configTestWeb(t)
	body := get(t, w, "/settings/widgets")
	for _, want := range []string{`role="listbox"`, `data-stp-menubtn`, `aria-haspopup="listbox"`, `role="option" data-value="ordinal"`,
		`data-wx-pick="unit"`, `data-wx-pick="layout"`, `data-wx-pick="side"`, `data-wx-pick="time24"`, `<span class="stp-group__t">Placement</span>`} {
		if !strings.Contains(body, want) {
			t.Errorf("Widgets lacks %q", want)
		}
	}
	for _, gone := range []string{"dfmt", "wx-layout-step", "data-wx-unit"} {
		if strings.Contains(body, gone) {
			t.Errorf("Widgets still has %q", gone)
		}
	}
}

func TestTemplatesDoNotReferenceTheRemovedConfigRoute(t *testing.T) {
	removed := regexp.MustCompile(`(?:hx-(?:get|post)|href|action)\s*=\s*["']/config(?:["'/?#])`)
	err := fs.WalkDir(templatesFS, "templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := templatesFS.ReadFile(path)
		if err != nil {
			return err
		}
		if m := removed.Find(raw); m != nil {
			t.Errorf("%s links the removed /config route: %s", path, m)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
