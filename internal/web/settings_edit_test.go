// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/profilemgr"
	"github.com/DanielBlei/rabbithole/internal/rank"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// send posts a form and returns the status and the body.
func send(t *testing.T, w *Web, path string, form url.Values) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// sendJSON is send for the endpoints that answer with an editReply.
func sendJSON(t *testing.T, w *Web, path string, form url.Values) (int, editReply) {
	t.Helper()
	code, body := send(t, w, path, form)
	var r editReply
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("POST %s: not JSON: %v; body=%s", path, err, body)
	}
	return code, r
}

func versionOf(t *testing.T, db *store.Store) int {
	t.Helper()
	v, _, err := db.ActiveConfig(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return v.Version
}

func formFor(t *testing.T, db *store.Store, kv ...string) url.Values {
	t.Helper()
	f := url.Values{"expected_version": {strconv.Itoa(versionOf(t, db))}}
	for i := 0; i < len(kv); i += 2 {
		f.Set(kv[i], kv[i+1])
	}
	return f
}

func activeContent(t *testing.T, db *store.Store) string {
	t.Helper()
	v, ok, err := db.ActiveConfig(t.Context())
	if err != nil || !ok {
		t.Fatalf("no active config: %v", err)
	}
	return v.Content
}

func TestSettingsSaveWritesOneVersionFromTheUI(t *testing.T) {
	w, db := configTestWeb(t)
	code, r := sendJSON(t, w, "/settings/save", formFor(t, db,
		"inference.provider", "vllm", "inference.model", "Qwen/Qwen3-4B", "inference.batch_size", "4",
		"inference.structured_output", "false", "user", "ada"))
	if code != http.StatusOK || !r.OK || r.Version != 1 {
		t.Fatalf("save = %d %+v", code, r)
	}
	if want := "Saved as version 1. The next ingest run uses it."; r.Message != want {
		t.Errorf("message = %q, want %q", r.Message, want)
	}
	hist, err := db.ConfigHistory(t.Context(), 10)
	if err != nil || len(hist) != 1 || hist[0].Source != store.ConfigSourceUI {
		t.Fatalf("history = %+v, %v; want one version with source ui", hist, err)
	}
	for _, want := range []string{"provider: vllm", "model: Qwen/Qwen3-4B", "batch_size: 4", "structured_output: false", "user: ada"} {
		if !strings.Contains(hist[0].Content, want) {
			t.Errorf("stored config lacks %q:\n%s", want, hist[0].Content)
		}
	}
}

func TestSettingsSaveIdenticalToTheActiveConfigWritesNothing(t *testing.T) {
	w, db := configTestWeb(t)
	saveConfig(t, db, "inference:\n  provider: vllm\n  model: m\n", store.ConfigSourceUI, 0)
	code, r := sendJSON(t, w, "/settings/save", formFor(t, db, "inference.provider", "vllm", "inference.model", "m"))
	if code != http.StatusOK || !r.OK || !r.NoOp || r.Message != "No changes to save." {
		t.Fatalf("no-op save = %d %+v", code, r)
	}
	if v := versionOf(t, db); v != 1 {
		t.Errorf("version = %d, a no-op save must not write one", v)
	}
	// Nothing saved at all and nothing changed: still nothing to write.
	w2, db2 := configTestWeb(t)
	if _, r := sendJSON(t, w2, "/settings/save", formFor(t, db2)); !r.NoOp {
		t.Errorf("empty save on the defaults = %+v, want a no-op", r)
	}
	if v := versionOf(t, db2); v != 0 {
		t.Errorf("version = %d, want none", v)
	}
}

func TestSettingsBlankMeansDefaultAndDefaultsStayUnpinned(t *testing.T) {
	w, db := configTestWeb(t)
	// Values the page shows for a config that never set them come back as is.
	form := formFor(t, db,
		"inference.provider", "ollama", "inference.host", "http://localhost:11434", "inference.model", "qwen3.5:4b",
		"inference.think", "true", "inference.batch_size", "1", "inference.model_tuning.tokens_per_item", "1024",
		"inference.model_tuning.num_ctx", "", "user", promptUser(""), "inference.model_tuning.max_tokens", "")
	if _, r := sendJSON(t, w, "/settings/save", form); !r.NoOp {
		t.Fatalf("saving the shown defaults = %+v, want a no-op: they must not be pinned", r)
	}
	// One real change next to them stores only the change.
	form.Set("inference.model", "other:1b")
	if _, r := sendJSON(t, w, "/settings/save", form); !r.OK || r.NoOp {
		t.Fatalf("save = %+v", r)
	}
	got := activeContent(t, db)
	if got != "inference:\n  model: other:1b\n" {
		t.Errorf("stored config = %q, want only the model", got)
	}
	// Clearing a pinned field returns it to the default.
	form = formFor(t, db, "inference.model", "")
	if _, r := sendJSON(t, w, "/settings/save", form); !r.OK {
		t.Fatalf("clear = %+v", r)
	}
	if strings.Contains(activeContent(t, db), "model") {
		t.Errorf("a blank model should unset it: %q", activeContent(t, db))
	}
	// The reset marker unpins a switch, which has no blank.
	saveConfig(t, db, "inference:\n  structured_output: true\n", store.ConfigSourceUI, versionOf(t, db))
	form = formFor(t, db, "inference.structured_output", "true", "inference.structured_output__reset", "1")
	if _, r := sendJSON(
		t,
		w,
		"/settings/save",
		form,
	); !r.OK ||
		strings.Contains(activeContent(t, db), "structured_output") {
		t.Errorf("reset = %+v, content %q", r, activeContent(t, db))
	}
}

func TestSettingsValidationErrorsAreInlineAndNothingIsSaved(t *testing.T) {
	w, db := configTestWeb(t)
	saveConfig(t, db, "user: keep\n", store.ConfigSourceUI, 0)
	form := formFor(t, db, "inference.host", "not a url", "inference.batch_size", "99",
		"inference.api_key", "literal-key", "inference.model_tuning.num_ctx", "lots")
	code, r := sendJSON(t, w, "/settings/save", form)
	if code != http.StatusUnprocessableEntity || r.OK {
		t.Fatalf("save = %d %+v, want 422", code, r)
	}
	byField := map[string]string{}
	for _, e := range r.Errors {
		byField[e.Field] = e.Msg
	}
	for _, field := range []string{"inference.host", "inference.batch_size", "inference.api_key", "inference.model_tuning.num_ctx"} {
		if byField[field] == "" {
			t.Errorf("no error for %s: %+v", field, r.Errors)
		}
	}
	if strings.Contains(byField["inference.api_key"], "literal-key") {
		t.Error("the rejected key was echoed back")
	}
	if v := versionOf(t, db); v != 1 {
		t.Errorf("version = %d, nothing may be saved on error", v)
	}
	// The form has a place for every field's error, with the alert role.
	body := get(t, w, "/settings/scoring")
	if !strings.Contains(body, `role="alert" data-stp-err`) {
		t.Error("rows lack an inline error slot")
	}
}

func TestSettingsStaleFormIsRefusedWithAConflict(t *testing.T) {
	w, db := configTestWeb(t)
	saveConfig(t, db, "user: a\n", store.ConfigSourceUI, 0)
	form := formFor(t, db, "user", "b")
	saveConfig(t, db, "user: c\n", store.ConfigSourceImport+"x.yaml", 1) // changed elsewhere
	for _, path := range []string{"/settings/save", "/settings/review"} {
		code, body := send(t, w, path, form)
		if code != http.StatusConflict ||
			!strings.Contains(body, "Settings changed elsewhere. Reload to see the latest.") {
			t.Errorf("%s = %d %s, want a 409 with the reload message", path, code, body)
		}
	}
	if got := activeContent(t, db); got != "user: c\n" {
		t.Errorf("active = %q: a stale save must not overwrite", got)
	}
}

func TestSettingsSavePreservesWhatTheFormNeverShows(t *testing.T) {
	w, db := configTestWeb(t)
	dir := t.TempDir()
	saveConfig(t, db, "inference:\n  provider: ollama\ningest:\n  digest_dir: "+dir+"\n",
		store.ConfigSourceImport+"c.yaml", 0)
	if _, r := sendJSON(t, w, "/settings/save", formFor(t, db, "inference.model", "m2")); !r.OK {
		t.Fatalf("save = %+v", r)
	}
	got := activeContent(t, db)
	for _, want := range []string{"digest_dir: " + dir, "model: m2"} {
		if !strings.Contains(got, want) {
			t.Errorf("after saving, the config lacks %q:\n%s", want, got)
		}
	}
	body := get(t, w, "/settings/general") + get(t, w, "/settings/scoring") + get(t, w, "/settings/advanced")
	for _, gone := range []string{"digest_dir", dir} {
		if strings.Contains(body, gone) {
			t.Errorf("the page shows %q", gone)
		}
	}
}

func TestSettingsProviderSwitchDropsFieldsItDoesNotRead(t *testing.T) {
	w, db := configTestWeb(t)
	saveConfig(
		t,
		db,
		"inference:\n  provider: openai\n  host: http://x\n  model: m\n  effort: high\n  api_key: $RABBITHOLE_INFERENCE_API_KEY\n"+
			"  structured_output: false\n  batch_size: 3\n",
		store.ConfigSourceUI,
		0,
	)
	form := formFor(t, db, "inference.provider", "heuristic")
	if _, r := sendJSON(t, w, "/settings/save", form); !r.OK {
		t.Fatalf("save = %+v", r)
	}
	got := activeContent(t, db)
	for _, gone := range []string{"host", "model", "effort", "api_key", "structured_output"} {
		if strings.Contains(got, gone+":") {
			t.Errorf("heuristic config still holds %s:\n%s", gone, got)
		}
	}
	if !strings.Contains(got, "provider: heuristic") || !strings.Contains(got, "batch_size: 3") {
		t.Errorf("heuristic config lost what it reads:\n%s", got)
	}
	code, body := send(t, w, "/settings/review", formFor(t, db, "inference.provider", "ollama"))
	if code != http.StatusOK || !strings.Contains(body, "Provider") {
		t.Errorf("review = %d %s", code, body)
	}
}

func TestSettingsReviewListsChangesAndNamesTheVersion(t *testing.T) {
	w, db := configTestWeb(t)
	saveConfig(t, db, "inference:\n  model: old\n", store.ConfigSourceUI, 0)
	code, body := send(t, w, "/settings/review", formFor(t, db, "inference.model", "new", "inference.batch_size", "5"))
	if code != http.StatusOK {
		t.Fatalf("review = %d %s", code, body)
	}
	for _, want := range []string{"<del>old</del>", "<ins>new</ins>", "Articles per request", "<del>Default (1)</del>", "<ins>5</ins>",
		"Saving creates version 2", "stp-diff", "<i>&#43;</i>  model: new", "stp-fold"} {
		if !strings.Contains(body, want) {
			t.Errorf("review lacks %q:\n%s", want, body)
		}
	}
	if v := versionOf(t, db); v != 1 {
		t.Error("a review must not save")
	}
	_, body = send(t, w, "/settings/review", formFor(t, db, "inference.model", "old"))
	if !strings.Contains(body, "No changes to save.") || strings.Contains(body, "data-stp-savenow") {
		t.Errorf("an unchanged review should say so and offer no save: %s", body)
	}
}

func TestSettingsHostChangeNeedsTheKeyConfirmedFirst(t *testing.T) {
	w, db := configTestWeb(t)
	saveConfig(
		t,
		db,
		"inference:\n  host: http://old:1\n  api_key: $RABBITHOLE_INFERENCE_API_KEY\n",
		store.ConfigSourceUI,
		0,
	)
	form := formFor(t, db, "inference.host", "http://new:2")

	_, review := send(t, w, "/settings/review", form)
	if !strings.Contains(review, "data-stp-confirmhost") ||
		!strings.Contains(review, "Send the API key to the new address") {
		t.Errorf("the review should ask for the key to be confirmed:\n%s", review)
	}
	code, r := sendJSON(t, w, "/settings/save", form)
	if code != http.StatusUnprocessableEntity || len(r.Errors) == 0 || r.Errors[0].Field != "inference.host" {
		t.Fatalf("unconfirmed save = %d %+v, want a refusal on the host", code, r)
	}
	if v := versionOf(t, db); v != 1 {
		t.Fatalf("version = %d, the refused save wrote one", v)
	}
	form.Set("confirm_host", "1")
	if code, r := sendJSON(t, w, "/settings/save", form); code != http.StatusOK || !r.OK {
		t.Fatalf("confirmed save = %d %+v", code, r)
	}
	// Without a key there is nothing to confirm.
	w2, db2 := configTestWeb(t)
	saveConfig(t, db2, "inference:\n  host: http://old:1\n", store.ConfigSourceUI, 0)
	if code, r := sendJSON(
		t,
		w2,
		"/settings/save",
		formFor(t, db2, "inference.host", "http://new:2"),
	); code != http.StatusOK ||
		!r.OK {
		t.Errorf("host change with no key = %d %+v", code, r)
	}
}

func TestSettingsRollbackAppendsAVersionAndHonoursExpected(t *testing.T) {
	w, db := configTestWeb(t)
	saveConfig(t, db, "user: one\n", store.ConfigSourceUI, 0)
	saveConfig(t, db, "user: two\n", store.ConfigSourceUI, 1)
	code, r := sendJSON(t, w, "/settings/rollback", url.Values{"version": {"1"}, "expected_version": {"2"}})
	if code != http.StatusOK || !r.OK || r.Version != 3 {
		t.Fatalf("rollback = %d %+v", code, r)
	}
	if got := activeContent(t, db); got != "user: one\n" {
		t.Errorf("active = %q after rolling back", got)
	}
	v, _ := db.ConfigVersion(t.Context(), 3)
	if v.Source != store.ConfigSourceRollback+"1" {
		t.Errorf("source = %q", v.Source)
	}
	if code, r := sendJSON(
		t,
		w,
		"/settings/rollback",
		url.Values{"version": {"2"}, "expected_version": {"2"}},
	); code != http.StatusConflict ||
		!r.Conflict {
		t.Errorf("stale rollback = %d %+v, want 409", code, r)
	}
	if code, _ := sendJSON(
		t,
		w,
		"/settings/rollback",
		url.Values{"version": {"9"}, "expected_version": {"3"}},
	); code != http.StatusNotFound {
		t.Errorf("rollback to a missing version = %d, want 404", code)
	}
}

func TestSettingsVersionDiffAgainstTheActiveOne(t *testing.T) {
	w, db := configTestWeb(t)
	saveConfig(t, db, "user: one\n", store.ConfigSourceUI, 0)
	saveConfig(t, db, "user: two\n", store.ConfigSourceUI, 1)
	body := get(t, w, "/settings/versions/1/diff")
	if !strings.Contains(body, "<i>-</i>user: two") || !strings.Contains(body, "<i>&#43;</i>user: one") {
		t.Errorf("diff lacks the change:\n%s", body)
	}
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/settings/versions/9/diff", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("diff of a missing version = %d", rec.Code)
	}
	body = get(t, w, "/settings/advanced")
	if !strings.Contains(body, `data-stp-compare="1"`) || !strings.Contains(body, `data-stp-rollback="1"`) {
		t.Error("history rows lack Compare and Roll back")
	}
}

func TestSettingsImportPreviewThenApplyThroughTheSharedCore(t *testing.T) {
	w, db := configTestWeb(t)
	env, err := config.LoadEnvFile(filepath.Join(t.TempDir(), ".env"))
	if err != nil {
		t.Fatal(err)
	}
	w.SetEnvFile(env, false)
	t.Setenv("RABBITHOLE_INFERENCE_API_KEY", "")
	const literal = "sk-import-secret-123"
	file := "user: grace\ninference:\n  provider: vllm\n  host: http://gpu:8000\n  model: m1\n  effort: high\n  api_key: " + literal +
		"\n  think: true\nprofile: /etc/hostname\n"

	_, preview := send(t, w, "/settings/import/preview", url.Values{"yaml": {file}, "expected_version": {"0"}})
	for _, want := range []string{"Applying this file saves version 1", "<i>&#43;</i>user: grace", "api_key: $RABBITHOLE_INFERENCE_API_KEY", "moves to",
		"not applied from here", "stp-diff"} {
		if !strings.Contains(preview, want) {
			t.Errorf("preview lacks %q:\n%s", want, preview)
		}
	}
	if strings.Contains(preview, literal) {
		t.Error("the literal key reached the preview")
	}
	if versionOf(t, db) != 0 {
		t.Fatal("a preview must not save")
	}

	code, r := sendJSON(t, w, "/settings/import/apply", url.Values{"yaml": {file}, "expected_version": {"0"}})
	if code != http.StatusOK || !r.OK || r.Version != 1 {
		t.Fatalf("apply = %d %+v", code, r)
	}
	v, _, _ := db.ActiveConfig(t.Context())
	if v.Source != store.ConfigSourceImport+"upload" || strings.Contains(v.Content, literal) ||
		!strings.Contains(v.Content, "api_key: $RABBITHOLE_INFERENCE_API_KEY") {
		t.Errorf("stored %q from %q: want the reference and an import source", v.Content, v.Source)
	}
	if raw, _ := os.ReadFile(env.Path); !strings.Contains(string(raw), literal) {
		t.Error("the literal key should have moved to .env")
	}
	if profs, _ := db.ListProfiles(t.Context()); len(profs) != 0 {
		t.Error("the file's profile path must not be read from the page")
	}
	// Applying the same file again changes nothing.
	if _, r := sendJSON(
		t,
		w,
		"/settings/import/apply",
		url.Values{"yaml": {file}, "expected_version": {"1"}},
	); !r.NoOp {
		t.Errorf("second apply = %+v, want a no-op", r)
	}
	// A bad file writes nothing and says why.
	code, r = sendJSON(
		t,
		w,
		"/settings/import/apply",
		url.Values{"yaml": {"inference:\n  provider: nope\n"}, "expected_version": {"1"}},
	)
	if code != http.StatusUnprocessableEntity || !strings.Contains(r.Message, "invalid provider") ||
		versionOf(t, db) != 1 {
		t.Errorf("bad import = %d %+v", code, r)
	}
	if code, _ := sendJSON(
		t,
		w,
		"/settings/import/apply",
		url.Values{"yaml": {file}, "expected_version": {"0"}},
	); code != http.StatusConflict {
		t.Errorf("stale import = %d, want 409", code)
	}
}

func TestSettingsImportRefusesALiteralKeyWhenEnvWritingIsOff(t *testing.T) {
	w, db := configTestWeb(t)
	env, _ := config.LoadEnvFile(filepath.Join(t.TempDir(), ".env"))
	w.SetEnvFile(env, true)
	code, r := sendJSON(t, w, "/settings/import/apply", url.Values{
		"yaml": {"inference:\n  api_key: sk-nope-1234\n"}, "expected_version": {"0"}})
	if code != http.StatusUnprocessableEntity || strings.Contains(r.Message, "sk-nope-1234") || versionOf(t, db) != 0 {
		t.Errorf("import = %d %+v", code, r)
	}
}

func TestSettingsExportHoldsReferencesOnly(t *testing.T) {
	w, db := configTestWeb(t)
	saveConfig(t, db, "inference:\n  api_key: $RABBITHOLE_INFERENCE_API_KEY\n", store.ConfigSourceUI, 0)
	t.Setenv("RABBITHOLE_INFERENCE_API_KEY", "sk-export-secret")
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/settings/export", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("export = %d %v", rec.Code, rec.Header())
	}
	if body := rec.Body.String(); !strings.Contains(body, "$RABBITHOLE_INFERENCE_API_KEY") ||
		strings.Contains(body, "sk-export-secret") {
		t.Errorf("export = %q, want the reference and never the key", body)
	}
}

func TestSettingsExportFormats(t *testing.T) {
	w, db := configTestWeb(t)
	saveConfig(
		t, db, "inference:\n  api_key: $RABBITHOLE_INFERENCE_API_KEY\n  batch_size: 7\n",
		store.ConfigSourceUI, 0,
	)
	t.Setenv("RABBITHOLE_INFERENCE_API_KEY", "sk-export-secret")
	tests := []struct {
		name, query, file, ctype string
		code                     int
		want, not                []string
	}{
		{"default is saved yaml", "", "rabbithole-config.yaml", "application/yaml", 200,
			[]string{"# rabbithole config version 1", "batch_size: 7"}, []string{"sk-export-secret", "value in force"}},
		{"saved json", "?format=json", "rabbithole-config.json", "application/json", 200,
			[]string{`"batch_size": 7`, "$RABBITHOLE_INFERENCE_API_KEY"}, []string{"sk-export-secret", "# rabbithole"}},
		{"defaults yaml", "?defaults=1", "rabbithole-config-defaults.yaml", "application/yaml", 200,
			[]string{"value in force", "batch_size: 7", "$RABBITHOLE_INFERENCE_API_KEY"}, []string{"sk-export-secret"}},
		{"defaults json", "?format=json&defaults=1", "rabbithole-config-defaults.json", "application/json", 200,
			[]string{"$RABBITHOLE_INFERENCE_API_KEY", `"batch_size": 7`}, []string{"sk-export-secret"}},
		{"defaults=0 is saved", "?defaults=0", "rabbithole-config.yaml", "application/yaml", 200,
			[]string{"# rabbithole config version 1"}, []string{"value in force"}},
		{"bad format", "?format=toml", "", "", http.StatusBadRequest, nil, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/settings/export"+tc.query, nil))
			if rec.Code != tc.code {
				t.Fatalf("status = %d, want %d", rec.Code, tc.code)
			}
			if tc.code != 200 {
				return
			}
			if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, `filename="`+tc.file+`"`) {
				t.Errorf("Content-Disposition = %q, want %s", cd, tc.file)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, tc.ctype) {
				t.Errorf("Content-Type = %q, want %s", ct, tc.ctype)
			}
			body := rec.Body.String()
			for _, x := range tc.want {
				if !strings.Contains(body, x) {
					t.Errorf("body lacks %q:\n%s", x, body)
				}
			}
			for _, x := range tc.not {
				if strings.Contains(body, x) {
					t.Errorf("body has %q:\n%s", x, body)
				}
			}
			if strings.HasSuffix(tc.file, ".json") && !json.Valid([]byte(body)) {
				t.Errorf("not valid JSON:\n%s", body)
			}
		})
	}
}

func TestSettingsExportEmptyState(t *testing.T) {
	const empty = "Everything is at its default."
	w, db := configTestWeb(t)
	if !strings.Contains(get(t, w, "/settings/advanced"), empty) {
		t.Error("nothing saved: the export row does not say everything is at its default")
	}
	saveConfig(t, db, "{}\n", store.ConfigSourceUI, 0)
	if !strings.Contains(get(t, w, "/settings/advanced"), empty) {
		t.Error("an empty saved config: the export row does not say everything is at its default")
	}
	saveConfig(t, db, "scoring:\n  batch_size: 7\n", store.ConfigSourceUI, 1)
	body := get(t, w, "/settings/advanced")
	if strings.Contains(body, empty) {
		t.Error("a changed setting: the empty-state line is still shown")
	}
	if !strings.Contains(body, `form="exportForm"`) || strings.Contains(body, `data-fld="defaults"`) {
		t.Error("the export choices must sit outside the settings draft")
	}
}

// logCapture swaps the global logger for one that writes to a buffer.
func logCapture(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := log.Logger
	log.Logger = zerolog.New(&buf).Level(zerolog.TraceLevel)
	t.Cleanup(func() { log.Logger = old })
	return &buf
}

func TestSettingsPastedKeyGoesToEnvAndNowhereElse(t *testing.T) {
	const secret = "sk-pasted-SECRET-4242"
	w, db := configTestWeb(t)
	logs := logCapture(t)
	env, err := config.LoadEnvFile(filepath.Join(t.TempDir(), ".env"))
	if err != nil {
		t.Fatal(err)
	}
	w.SetEnvFile(env, false)
	t.Setenv("RABBITHOLE_INFERENCE_API_KEY", "")

	if !strings.Contains(get(t, w, "/settings/scoring"), "data-stp-paste") {
		t.Error("the paste action is missing")
	}
	code, body := send(t, w, "/settings/key", formFor(t, db, "value", secret))
	if code != http.StatusOK {
		t.Fatalf("paste = %d %s", code, body)
	}
	var r editReply
	_ = json.Unmarshal([]byte(body), &r)
	if r.Ref != "$RABBITHOLE_INFERENCE_API_KEY" || r.Chip != "Set in .env" {
		t.Errorf("reply = %+v, want the reference and the .env chip", r)
	}
	if raw, _ := os.ReadFile(env.Path); string(raw) != "RABBITHOLE_INFERENCE_API_KEY="+secret+"\n" {
		t.Errorf(".env = %q", raw)
	}
	// Saving the reference leaves the key out of every surface.
	form := formFor(t, db, "inference.api_key", r.Ref)
	if _, rr := sendJSON(t, w, "/settings/save", form); !rr.OK {
		t.Fatalf("save = %+v", rr)
	}
	surfaces := []string{
		body,
		activeContent(t, db),
		logs.String(),
		get(t, w, "/settings/scoring"),
		get(t, w, "/settings/advanced"),
		get(t, w, "/settings/export"),
	}
	_, review := send(
		t,
		w,
		"/settings/review",
		formFor(t, db, "inference.api_key", "$RABBITHOLE_INFERENCE_API_KEY", "inference.model", "z"),
	)
	surfaces = append(surfaces, review)
	for i, s := range surfaces {
		if strings.Contains(s, secret) {
			t.Errorf("surface %d carries the key", i)
		}
	}
	code, body = send(t, w, "/settings/key", formFor(t, db, "value", "bad value with spaces"))
	if code != http.StatusUnprocessableEntity || strings.Contains(body, "bad value with spaces") {
		t.Errorf("a refused value = %d %s", code, body)
	}
	if strings.Contains(logs.String(), "bad value") {
		t.Error("a refused value was logged")
	}
}

func TestSettingsPastedKeyRefusedWhenEnvironmentSetsItOrWritingIsOff(t *testing.T) {
	w, db := configTestWeb(t)
	env, _ := config.LoadEnvFile(filepath.Join(t.TempDir(), ".env"))
	w.SetEnvFile(env, false)
	t.Setenv("RABBITHOLE_INFERENCE_API_KEY", "from-the-real-environment")
	code, body := send(t, w, "/settings/key", formFor(t, db, "value", "sk-new-key-1"))
	if code != http.StatusConflict || !strings.Contains(body, "set in the environment") {
		t.Errorf("env-set paste = %d %s", code, body)
	}
	if _, err := os.Stat(env.Path); err == nil {
		t.Error("the .env was written")
	}

	w.SetEnvFile(env, true) // --no-env-write
	code, body = send(t, w, "/settings/key", formFor(t, db, "value", "sk-new-key-1"))
	if code != http.StatusForbidden || !strings.Contains(body, "turned off") {
		t.Errorf("--no-env-write paste = %d %s", code, body)
	}
	if strings.Contains(get(t, w, "/settings/scoring"), "data-stp-paste") {
		t.Error("the paste action should be hidden under --no-env-write")
	}

	// An unwritable .env says so, without the key.
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "ro"), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RABBITHOLE_INFERENCE_API_KEY", "")
	bad, _ := config.LoadEnvFile(filepath.Join(dir, "ro", "sub", ".env"))
	w.SetEnvFile(bad, false)
	code, body = send(t, w, "/settings/key", formFor(t, db, "value", "sk-new-key-2"))
	if code != http.StatusUnprocessableEntity || strings.Contains(body, "sk-new-key-2") {
		t.Errorf("unwritable paste = %d %s", code, body)
	}
}

func TestSettingsTestConnectionChecksUnsavedValuesAndWritesNothing(t *testing.T) {
	const secret = "sk-test-leaky-9999"
	w, db := configTestWeb(t)
	saveConfig(t, db, "inference:\n  host: http://saved:1\n", store.ConfigSourceUI, 0)
	t.Setenv("RABBITHOLE_INFERENCE_API_KEY", secret)
	var seen config.Config
	var fail error
	w.scorerCheck = func(_ context.Context, cfg config.Config) error { seen = cfg; return fail }

	form := formFor(
		t,
		db,
		"inference.host",
		"http://unsaved:2",
		"inference.model",
		"m",
		"inference.api_key",
		"$RABBITHOLE_INFERENCE_API_KEY",
	)
	code, r := sendJSON(t, w, "/settings/test", form)
	if code != http.StatusOK || !r.OK || seen.Inference.Host != "http://unsaved:2" {
		t.Fatalf("test = %d %+v, checked %q", code, r, seen.Inference.Host)
	}
	fail = errors.New("backend validation: 401 for key " + secret + " at http://unsaved:2")
	_, r = sendJSON(t, w, "/settings/test", form)
	if r.OK || strings.Contains(r.Message, secret) || !strings.Contains(r.Message, "401") {
		t.Errorf("failed test = %+v, want a short error without the key", r)
	}
	if versionOf(t, db) != 1 || activeContent(t, db) != "inference:\n  host: http://saved:1\n" {
		t.Error("a connection test must not write anything")
	}
	_, r = sendJSON(t, w, "/settings/test", formFor(t, db, "inference.provider", "heuristic"))
	if !r.OK || !strings.Contains(r.Message, "no model") {
		t.Errorf("heuristic test = %+v", r)
	}
	if code, _ := sendJSON(
		t,
		w,
		"/settings/test",
		formFor(t, db, "inference.host", "nope"),
	); code != http.StatusUnprocessableEntity {
		t.Errorf("an invalid address = %d, want 422", code)
	}
}

func TestSettingsTestConnectionUsesTheRealProviderChecks(t *testing.T) {
	w, db := configTestWeb(t)
	// Nothing listens on this address: the provider's own startup check fails.
	form := formFor(t, db, "inference.host", "http://127.0.0.1:1", "inference.provider", "ollama")
	code, r := sendJSON(t, w, "/settings/test", form)
	if code != http.StatusOK || r.OK || r.Message == "" {
		t.Errorf("test against nothing = %d %+v, want a plain failure", code, r)
	}
}

func TestSettingsPageCarriesTheEditingMarkup(t *testing.T) {
	w, db := configTestWeb(t)
	saveConfig(t, db, "user: a\n", store.ConfigSourceUI, 0)
	body := get(t, w, "/settings/scoring")
	for _, want := range []string{
		`name="expected_version" value="1"`, `data-stp-savebar`, `data-stp-discard`, `data-stp-review`, `data-stp-test`,
		`class="stp-seg"`, `name="inference.provider"`, `data-stp-switch`, `data-stp-less`, `/static/js/settings.js`,
		`data-fld="inference.api_key"`, `data-applies="ollama vllm openai"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Scoring lacks %q", want)
		}
	}
	for _, native := range []string{"<select", `type="checkbox"`} {
		form := body[strings.Index(body, `<form class="stp-form"`):]
		if strings.Contains(form[:strings.Index(form, "</form>")], native) {
			t.Errorf("the form uses a native control: %s", native)
		}
	}
	js := get(t, w, "/static/js/settings.js")
	for _, want := range []string{"beforeunload", "You have unsaved changes", "Keep editing", "Discard"} {
		if !strings.Contains(js, want) {
			t.Errorf("settings.js lacks the leave guard piece %q", want)
		}
	}
}

func TestSettingsEditingIsOffWhileTheSavedConfigCannotBeRead(t *testing.T) {
	w, db := configTestWeb(t)
	saveConfig(t, db, "inference:\n  nonsense: true\n", store.ConfigSourceUI, 0)
	code, r := sendJSON(t, w, "/settings/save", formFor(t, db, "user", "x"))
	if code != http.StatusUnprocessableEntity || !strings.Contains(r.Message, "cannot be read") {
		t.Errorf("save over an unreadable config = %d %+v", code, r)
	}
	if v := versionOf(t, db); v != 1 {
		t.Errorf("version = %d", v)
	}
	if !strings.Contains(get(t, w, "/settings/scoring"), "The saved configuration cannot be read") {
		t.Error("the page should say the saved config cannot be read")
	}
}

func TestSettingsUploadTreatsSystemPromptAsTextNeverAPath(t *testing.T) {
	w, db := configTestWeb(t)
	for _, v := range []string{"/etc/passwd", "./prompts/system.md", "system.md"} {
		file := "inference:\n  system_prompt: " + v + "\n"
		for _, path := range []string{"/settings/import/apply", "/settings/import/preview"} {
			code, body := send(t, w, path, url.Values{"yaml": {file}, "expected_version": {"0"}})
			if path == "/settings/import/apply" && code != http.StatusUnprocessableEntity {
				t.Errorf("%s %q = %d, want 422", path, v, code)
			}
			if !strings.Contains(body, "paste the prompt text") {
				t.Errorf("%s %q gave no instruction to paste the text: %s", path, v, body)
			}
		}
	}
	if versionOf(t, db) != 0 {
		t.Fatal("a refused upload saved a version")
	}
	file := "inference:\n  system_prompt: |\n    Score each article.\n    Be strict.\n"
	if code, r := sendJSON(
		t,
		w,
		"/settings/import/apply",
		url.Values{"yaml": {file}, "expected_version": {"0"}},
	); code != 200 ||
		!r.OK {
		t.Fatalf("text prompt upload = %d %+v", code, r)
	}
	if got := activeContent(t, db); !strings.Contains(got, "Score each article.") {
		t.Errorf("stored %q lacks the prompt text", got)
	}
}

func TestSettingsImportShowsDeprecationsAndWritesFeedDefaultsSince(t *testing.T) {
	w, db := configTestWeb(t)
	file := "inference:\n  summary:\n    model: s\ningest:\n  since: 3d\n  digest_dir: /tmp/d\n"
	_, preview := send(t, w, "/settings/import/preview", url.Values{"yaml": {file}, "expected_version": {"0"}})
	for _, want := range []string{"ingest.since is deprecated", "ingest.digest_dir is deprecated", "inference.summary is not used"} {
		if !strings.Contains(preview, want) {
			t.Errorf("preview lacks %q:\n%s", want, preview)
		}
	}
	if code, r := sendJSON(
		t,
		w,
		"/settings/import/apply",
		url.Values{"yaml": {file}, "expected_version": {"0"}},
	); code != 200 ||
		!r.OK {
		t.Fatalf("apply = %d %+v", code, r)
	}
	d, err := db.FeedDefaults(t.Context())
	if err != nil || d.Since == nil || d.Since.Std() != 72*time.Hour {
		t.Errorf("feed defaults since = %+v, %v; want 3d", d.Since, err)
	}
	if got := activeContent(t, db); strings.Contains(got, "since") || strings.Contains(got, "summary") ||
		!strings.Contains(got, "digest_dir: /tmp/d") {
		t.Errorf("stored %q: since and summary must be gone, digest_dir kept", got)
	}
}

func TestSettingsCustomInstructionsAreStoredAsTextAndBuiltinIsNeverWritten(t *testing.T) {
	w, db := configTestWeb(t)
	form := formFor(
		t,
		db,
		"inference.system_prompt",
		"custom",
		"inference.system_prompt_text",
		"Be strict.\nScore 0-10.",
	)
	if code, r := sendJSON(t, w, "/settings/save", form); code != 200 || !r.OK {
		t.Fatalf("save = %d %+v", code, r)
	}
	if got := activeContent(t, db); !strings.Contains(got, "system_prompt:") || !strings.Contains(got, "Be strict.") {
		t.Errorf("stored %q lacks the instructions", got)
	}
	// The built-in text offered as a starting point is not pinned when left unedited.
	form = formFor(t, db, "inference.system_prompt", "custom", "inference.system_prompt_text", rank.DefaultSystemPrompt)
	if code, r := sendJSON(t, w, "/settings/save", form); code != 200 || !r.OK {
		t.Fatalf("save builtin text = %d %+v", code, r)
	}
	if got := activeContent(t, db); strings.Contains(got, "system_prompt") {
		t.Errorf("the built-in text was written into the document: %q", got)
	}
	// A path-looking value is refused, an empty custom one too.
	for _, text := range []string{"/etc/passwd", ""} {
		form = formFor(t, db, "inference.system_prompt", "custom", "inference.system_prompt_text", text)
		if code, _ := sendJSON(t, w, "/settings/save", form); code != http.StatusUnprocessableEntity {
			t.Errorf("custom text %q = %d, want 422", text, code)
		}
	}
	// None is stored as false.
	form = formFor(t, db, "inference.system_prompt", "none")
	if code, r := sendJSON(t, w, "/settings/save", form); code != 200 || !r.OK {
		t.Fatalf("save none = %d %+v", code, r)
	}
	if got := activeContent(t, db); !strings.Contains(got, "system_prompt: false") {
		t.Errorf("stored %q, want system_prompt: false", got)
	}
}

func TestSettingsPasteKeyHonoursExpectedVersion(t *testing.T) {
	w, db := configTestWeb(t)
	env, err := config.LoadEnvFile(filepath.Join(t.TempDir(), ".env"))
	if err != nil {
		t.Fatal(err)
	}
	w.SetEnvFile(env, false)
	t.Setenv("RABBITHOLE_INFERENCE_API_KEY", "")
	saveConfig(t, db, "user: one\n", store.ConfigSourceUI, 0)
	code, r := sendJSON(t, w, "/settings/key", url.Values{"value": {"sk-stale-1"}, "expected_version": {"0"}})
	if code != http.StatusConflict || !r.Conflict || r.Message != staleMessage {
		t.Errorf("stale paste = %d %+v, want 409 %q", code, r, staleMessage)
	}
	if _, err := os.Stat(env.Path); err == nil {
		t.Error("a stale paste wrote the .env")
	}
	if code, r := sendJSON(
		t,
		w,
		"/settings/key",
		formFor(t, db, "value", "sk-stale-1"),
	); code != http.StatusOK ||
		!r.OK {
		t.Errorf("current paste = %d %+v", code, r)
	}
}

func TestPlainErrorMasksAnyNonEmptyKey(t *testing.T) {
	t.Setenv("RABBITHOLE_INFERENCE_API_KEY", "ab")
	cfg := config.Config{}
	cfg.Inference.APIKey = "$RABBITHOLE_INFERENCE_API_KEY"
	if got := plainError(errors.New("denied for ab"), cfg); strings.Contains(got, "ab") {
		t.Errorf("plainError = %q, want the short key masked", got)
	}
}

func TestSettingsTestNeedsTheHostMoveConfirmed(t *testing.T) {
	w, db := configTestWeb(t)
	saveConfig(t, db, "inference:\n  host: http://old:1\n  api_key: $RABBITHOLE_INFERENCE_API_KEY\n",
		store.ConfigSourceUI, 0)
	t.Setenv("RABBITHOLE_INFERENCE_API_KEY", "k")
	calls := 0
	w.scorerCheck = func(context.Context, config.Config) error { calls++; return nil }
	form := formFor(t, db, "inference.host", "http://new:2")
	code, r := sendJSON(t, w, "/settings/test", form)
	if code != http.StatusUnprocessableEntity || len(r.Errors) == 0 || r.Errors[0].Field != keyHost ||
		r.Errors[0].Msg != hostConfirmMessage {
		t.Fatalf("unconfirmed test = %d %+v, want the save refusal on the host", code, r)
	}
	if calls != 0 {
		t.Error("the key was sent before the move was confirmed")
	}
	form.Set("confirm_host", "1")
	if code, r := sendJSON(t, w, "/settings/test", form); code != http.StatusOK || !r.OK || calls != 1 {
		t.Errorf("confirmed test = %d %+v, %d calls", code, r, calls)
	}
}

func TestSettingsRollbackToTheActiveContentIsANoOp(t *testing.T) {
	w, db := configTestWeb(t)
	saveConfig(t, db, "user: one\n", store.ConfigSourceUI, 0)
	// Same effective settings, written differently: an explicit default.
	saveConfig(t, db, "user: one\ninference:\n  provider: ollama\n", store.ConfigSourceUI, 1)
	code, r := sendJSON(t, w, "/settings/rollback", url.Values{"version": {"1"}, "expected_version": {"2"}})
	if code != http.StatusOK || !r.OK || !r.NoOp || r.Message != "Already the active settings." {
		t.Fatalf("rollback to the same settings = %d %+v", code, r)
	}
	if v := versionOf(t, db); v != 2 {
		t.Errorf("version = %d, a no-op rollback must not write one", v)
	}
}

func TestSettingsRollbackRefusesAnUnreadableVersion(t *testing.T) {
	w, db := configTestWeb(t)
	saveConfig(t, db, "user: [unclosed\n", store.ConfigSourceUI, 0)
	saveConfig(t, db, "user: two\n", store.ConfigSourceUI, 1)
	code, r := sendJSON(t, w, "/settings/rollback", url.Values{"version": {"1"}, "expected_version": {"2"}})
	if code != http.StatusUnprocessableEntity || !strings.Contains(r.Message, "cannot be read") {
		t.Errorf("rollback to garbage = %d %+v", code, r)
	}
	if v := versionOf(t, db); v != 2 {
		t.Errorf("version = %d, the unreadable version became active", v)
	}
}

func TestSettingsRollbackNeedsTheHostMoveConfirmed(t *testing.T) {
	w, db := configTestWeb(t)
	saveConfig(t, db, "inference:\n  host: http://old:1\n  api_key: $RABBITHOLE_INFERENCE_API_KEY\n",
		store.ConfigSourceUI, 0)
	saveConfig(t, db, "inference:\n  host: http://new:2\n  api_key: $RABBITHOLE_INFERENCE_API_KEY\n",
		store.ConfigSourceUI, 1)
	form := url.Values{"version": {"1"}, "expected_version": {"2"}}
	code, r := sendJSON(t, w, "/settings/rollback", form)
	if code != http.StatusUnprocessableEntity || len(r.Errors) == 0 || r.Errors[0].Field != keyHost ||
		!strings.Contains(r.Message, "http://old:1") || !strings.Contains(r.Message, "http://new:2") {
		t.Fatalf("unconfirmed rollback = %d %+v, want a refusal naming both addresses", code, r)
	}
	if v := versionOf(t, db); v != 2 {
		t.Fatalf("version = %d, the refused rollback wrote one", v)
	}
	form.Set("confirm_host", "1")
	if code, r := sendJSON(t, w, "/settings/rollback", form); code != http.StatusOK || !r.OK || r.Version != 3 {
		t.Errorf("confirmed rollback = %d %+v", code, r)
	}
}

// A key held in .env or the environment reaches no response and no log line,
// whichever settings endpoint is asked, on a failure path or not.
func TestSettingsNeverLeaksTheKey(t *testing.T) {
	const secret = "sk-leakcheck-7Hq2Z9"
	w, db := configTestWeb(t)
	logs := logCapture(t)
	var slogBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&slogBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	envPath := filepath.Join(t.TempDir(), ".env")
	env, err := config.LoadEnvFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	w.SetEnvFile(env, false)
	t.Setenv("RABBITHOLE_INFERENCE_API_KEY", "")
	other := filepath.Join(t.TempDir(), "elsewhere.env")

	var bodies []string
	note := func(body string) { bodies = append(bodies, body) }

	// The pasted key goes to the fixed .env path, whatever else the request says.
	_, body := send(t, w, "/settings/key", formFor(t, db, "value", secret, "path", other, "name", "PATH"))
	note(body)
	if _, err := os.Stat(other); err == nil {
		t.Error("a request chose where the key was written")
	}
	info, err := os.Stat(envPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf(".env = %v, %v; want a 0600 file at the configured path", info, err)
	}
	if raw, _ := os.ReadFile(envPath); !strings.Contains(string(raw), secret) {
		t.Fatalf(".env lacks the pasted key: %q", raw)
	}

	ref := "$RABBITHOLE_INFERENCE_API_KEY"
	_, body = send(t, w, "/settings/save", formFor(t, db, "inference.api_key", ref, "inference.model", "m"))
	note(body)
	_, body = send(
		t,
		w,
		"/settings/review",
		formFor(t, db, "inference.api_key", ref, "inference.host", "http://moved:9"),
	)
	note(body)

	w.scorerCheck = func(context.Context, config.Config) error {
		return errors.New("backend validation: 401 invalid key " + secret + " at http://moved:9")
	}
	form := formFor(t, db, "inference.host", "http://moved:9", "confirm_host", "1")
	_, body = send(t, w, "/settings/test", form)
	note(body)
	if !strings.Contains(body, "401") {
		t.Errorf("the error path did not run: %s", body)
	}

	saveConfig(t, db, "user: later\n", store.ConfigSourceUI, versionOf(t, db))
	note(get(t, w, "/settings/export"))
	note(get(t, w, "/settings/versions/1/diff"))
	note(get(t, w, "/settings/advanced"))
	note(get(t, w, "/settings/scoring"))
	_, body = send(
		t,
		w,
		"/settings/import/preview",
		url.Values{"yaml": {"inference:\n  api_key: " + ref + "\n  model: z\n"}},
	)
	note(body)
	_, body = send(
		t,
		w,
		"/settings/rollback",
		url.Values{"version": {"1"}, "expected_version": {strconv.Itoa(versionOf(t, db))}},
	)
	note(body)

	for i, b := range bodies {
		if strings.Contains(b, secret) {
			t.Errorf("response %d carries the key", i)
		}
	}
	if strings.Contains(logs.String(), secret) || strings.Contains(slogBuf.String(), secret) {
		t.Error("a log line carries the key")
	}
	for _, v := range []string{activeContent(t, db)} {
		if strings.Contains(v, secret) {
			t.Error("the saved config carries the key")
		}
	}
}

func TestDefaultTextServesEveryProviderFromTheSnapshot(t *testing.T) {
	for _, p := range append([]string{"made-up"}, config.Providers...) {
		d := config.DefaultsFor(p)
		for i := range fieldSpecs {
			f := &fieldSpecs[i]
			if f.Key == keyUser || f.Key == keySystem {
				continue
			}
			if got, want := defaultText(f, p), f.get(&d); got != want {
				t.Errorf("%s default for %s = %q, want %q", f.Key, p, got, want)
			}
		}
	}
	defer func() {
		if recover() == nil {
			t.Error("mustSpec accepted a key that names no field")
		}
	}()
	mustSpec("no.such.field")
}

func TestSettingsReviewWarnsWhenRepliesChange(t *testing.T) {
	w, db := configTestWeb(t)
	_, plain := send(t, w, "/settings/review", formFor(t, db, "inference.model", "m"))
	if strings.Contains(plain, "data-stp-repliesnote") {
		t.Error("an unrelated change should not warn about replies")
	}
	for _, kv := range [][2]string{{"inference.structured_output", "false"}, {"inference.system_prompt", "none"}} {
		_, body := send(t, w, "/settings/review", formFor(t, db, kv[0], kv[1]))
		if !strings.Contains(body, "data-stp-repliesnote") {
			t.Errorf("changing %s should warn:\n%s", kv[0], body)
		}
	}
}

func TestSettingsResetFormUnsetsEveryPinnedField(t *testing.T) {
	w, db := configTestWeb(t)
	saveConfig(t, db, `user: ada
inference:
  provider: openai
  host: https://api.example.com/v1
  model: gpt-x
  api_key: $RABBITHOLE_MY_API_KEY
  think: true
  effort: high
  batch_size: 4
  max_parallel: 3
  structured_output: false
  system_prompt: Be brief.
  model_tuning:
    reason_max_chars: 300
`, store.ConfigSourceUI, 0)
	before := versionOf(t, db)

	// What the page posts after Reset to default on every group: the default
	// shown in each control with its reset marker, the key reference emptied and
	// the instructions on Built-in.
	kv := []string{
		"user", "ada", "user__reset", "1",
		keyProvider, config.ProviderOllama, keyProvider + "__reset", "1",
		keyAPIKey, "",
		keySystem, sysBuiltin, keySystemText, rank.DefaultSystemPrompt,
	}
	for _, f := range fieldSpecs {
		if f.Key == keyProvider || f.Key == keyUser || f.Kind == kSys || f.Kind == kKey {
			continue
		}
		kv = append(kv, f.Key, defaultText(&f, config.ProviderOllama), f.Key+"__reset", "1")
	}
	code, r := sendJSON(t, w, "/settings/save", formFor(t, db, kv...))
	if code != http.StatusOK || !r.OK || r.NoOp {
		t.Fatalf("save = %d %+v", code, r)
	}
	if got := versionOf(t, db); got != before+1 {
		t.Fatalf("version = %d, want %d", got, before+1)
	}
	got := activeContent(t, db)
	for _, key := range []string{"user", "provider", "host", "model", "api_key", "think", "effort", "batch_size",
		"max_parallel", "structured_output", "system_prompt", "reason_max_chars"} {
		if strings.Contains(got, key) {
			t.Errorf("stored config still pins %s: %q", key, got)
		}
	}
}

func TestSettingsImportSinceOnlyChangeIsPreviewedAndApplied(t *testing.T) {
	w, db := configTestWeb(t)
	if err := db.SetDefaultSince(t.Context(), config.Duration(7*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	file := "ingest:\n  since: 14d\n"
	// The config itself matches the active one.
	base, err := profilemgr.PrepareRun(config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveConfig(t.Context(), string(base.Content), base.Hash, store.ConfigSourceUI, 0); err != nil {
		t.Fatal(err)
	}
	_, preview := send(t, w, "/settings/import/preview", url.Values{"yaml": {file}, "expected_version": {"1"}})
	if strings.Contains(preview, "data-nochange") || !strings.Contains(preview, "Feed defaults since: 7d -> 14d") {
		t.Errorf("preview must show the since change:\n%s", preview)
	}
	code, r := sendJSON(t, w, "/settings/import/apply", url.Values{"yaml": {file}, "expected_version": {"1"}})
	if code != http.StatusOK || !r.OK || !r.NoOp || versionOf(t, db) != 1 {
		t.Fatalf("apply = %d %+v, version %d; want ok, no new version", code, r, versionOf(t, db))
	}
	if d, _ := db.FeedDefaults(t.Context()); d.Since == nil || d.Since.Std() != 14*24*time.Hour {
		t.Errorf("feed defaults since = %v, want 14d", d.Since)
	}
	_, preview = send(t, w, "/settings/import/preview", url.Values{"yaml": {file}, "expected_version": {"1"}})
	if !strings.Contains(preview, "data-nochange") {
		t.Errorf("a matching since is no change:\n%s", preview)
	}
}

func TestSettingsImportPreviewCountsValuesEqualToTheDefault(t *testing.T) {
	w, db := configTestWeb(t)
	tests := []struct {
		name, file, want string
		absent           bool
	}{
		{
			"several",
			"inference:\n  provider: ollama\n  think: true\n  batch_size: 1\n  model: other\n",
			"3 values equal the default and stay defaults",
			false,
		},
		{
			"one",
			"inference:\n  provider: ollama\n  model: other\n",
			"1 value equals the default and stays a default",
			false,
		},
		{"none", "inference:\n  model: other\n", "the default", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, preview := send(
				t,
				w,
				"/settings/import/preview",
				url.Values{"yaml": {tc.file}, "expected_version": {"0"}},
			)
			if strings.Contains(preview, tc.want) == tc.absent {
				t.Errorf("preview for %q lacks/has %q:\n%s", tc.file, tc.want, preview)
			}
			if strings.Contains(preview, "provider: ollama") {
				t.Errorf("the diff pins a default:\n%s", preview)
			}
		})
	}
	if versionOf(t, db) != 0 {
		t.Fatal("a preview must not save")
	}
}
