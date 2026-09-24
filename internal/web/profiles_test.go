// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/profile"
	"github.com/DanielBlei/rabbithole/internal/store"
)

func TestProfileSettingsRenderDefaultImmutable(t *testing.T) {
	w := newTestWeb(t)
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/feed", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /feed = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`id="stgProfiles"`,
		`id="profilesSettings"`,
		"Active profile",
		"Default",
		"built-in · immutable",
		`hx-post="/profiles/` + profile.DefaultID + `/duplicate"`,
		`hx-get="/profiles/rescore"`,
		"Existing scores stay unchanged until you rescore them.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("profile settings missing %q", want)
		}
	}
	if strings.Contains(body, `hx-get="/profiles/`+profile.DefaultID+`/confirm-delete"`) {
		t.Error("built-in Default rendered a delete action")
	}
}

func TestProfileRescoreConfirmationAndStart(t *testing.T) {
	w := newTestWeb(t)
	ctx := context.Background()
	local, err := w.profiles.Create(ctx, "Seismology", "# Seismology")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.profiles.SetActive(ctx, local.ID); err != nil {
		t.Fatal(err)
	}
	item := feeds.Item{
		ID: "scored", Source: "S", Title: "Earthquake systems",
		Link: "https://x/scored", Summary: "seismic monitoring",
		Published: time.Now().Add(-time.Hour),
	}
	if err := w.db.Record(ctx, []feeds.Item{item}, []store.DigestEntry{{
		Item: item, Score: 1, Reason: "old", Model: "old",
		ProfileID: "old-profile", ProfileName: "Old", ProfileHash: "old-hash",
	}}, time.Now()); err != nil {
		t.Fatal(err)
	}

	body := get(t, w, "/profiles/rescore")
	for _, want := range []string{
		"Rescore recent items",
		"last <strong>7 days</strong>",
		"<strong>Seismology</strong>",
		"Your ratings, notes, read state and bookmarks will remain untouched.",
		`hx-post="/profiles/rescore"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("confirmation missing %q: %s", want, body)
		}
	}

	body = postForm(t, w, "/profiles/rescore", url.Values{"window": {"7d"}})
	if !strings.Contains(body, `id="ingestModal"`) {
		t.Fatalf("start response did not open runner: %s", body)
	}
	waitForWebRun(t, w)
	last, err := w.db.LastIngestRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if last == nil || last.TriggeredBy != store.IngestTriggerProfileRescore ||
		last.Status != store.IngestStatusOK || last.Counts.Fetched != 1 ||
		last.Counts.Scored != 1 {
		t.Fatalf("rescore history = %+v", last)
	}
	row, err := w.db.Get(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.LLMProfileID == nil || *row.LLMProfileID != local.ID ||
		row.LLMProfileName == nil || *row.LLMProfileName != local.Name ||
		row.LLMProfileHash == nil || *row.LLMProfileHash != profile.ContentHash(local.Content) {
		t.Fatalf("rescored provenance = %+v", row)
	}
	body = get(t, w, "/ingest/status")
	if !strings.Contains(body, "rescore complete") ||
		!strings.Contains(body, "1</b> of <b>1") {
		t.Fatalf("completion summary = %s", body)
	}
}

func TestProfileRescoreActionOnlyTargetsActiveProfile(t *testing.T) {
	w := newTestWeb(t)
	if _, err := w.profiles.Create(t.Context(), "Inactive", "# Inactive"); err != nil {
		t.Fatal(err)
	}
	body := get(t, w, "/profiles")
	if got := strings.Count(body, `hx-get="/profiles/rescore"`); got != 1 {
		t.Fatalf("rescore action count = %d, want one global active-profile action", got)
	}
	if strings.Contains(body, `/profiles/Inactive/rescore`) {
		t.Fatal("inactive profile rendered a rescore action")
	}
}

func TestProfileRescoreRejectsInvalidWindow(t *testing.T) {
	w := newTestWeb(t)
	if got := postFormCode(w, "/profiles/rescore", url.Values{"window": {"30d"}}); got != http.StatusBadRequest {
		t.Fatalf("invalid rescore window status = %d, want 400", got)
	}
	runs, _, err := w.db.ListIngestRuns(t.Context(), 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("invalid rescore started runs: %+v", runs)
	}
}

func waitForWebRun(t *testing.T, w *Web) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		last, err := w.db.LastIngestRun(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if last != nil && last.Status != store.IngestStatusRunning && !w.ing.Status().Running {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("background run did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestProfileCreateEditSelectAndDuplicate(t *testing.T) {
	w := newTestWeb(t)
	body := postForm(t, w, "/profiles", url.Values{
		"name":       {"Systems"},
		"mode":       {"structured"},
		"interested": {"Local models\nKernel work"},
		"less":       {"Press releases"},
		"context":    {"Prefer measurements."},
	})
	if !strings.Contains(body, "profile created") || !strings.Contains(body, "Systems") {
		t.Fatalf("create response = %s", body)
	}
	locals, err := w.db.ListProfiles(t.Context())
	if err != nil || len(locals) != 1 {
		t.Fatalf("ListProfiles = %+v, %v", locals, err)
	}
	id := locals[0].ID
	if fields, ok := profile.ParseCanonical(locals[0].Content); !ok ||
		fields.Interested != "Local models\nKernel work" {
		t.Fatalf("created content not canonical: %q", locals[0].Content)
	}

	body = postForm(t, w, "/profiles/"+id, url.Values{
		"name":    {"Systems raw"},
		"mode":    {"raw"},
		"content": {"# Arbitrary profile\n\n<!-- private note -->\nKeep this Markdown."},
	})
	if !strings.Contains(body, "profile saved") {
		t.Fatalf("edit response = %s", body)
	}
	edited, err := w.db.GetProfile(t.Context(), id)
	if err != nil || !strings.Contains(edited.Content, "<!-- private note -->") {
		t.Fatalf("raw edit was not preserved: %+v, %v", edited, err)
	}

	body = postForm(t, w, "/profiles/"+id+"/active", nil)
	if !strings.Contains(body, "Systems raw") || !strings.Contains(body, "active") {
		t.Fatalf("select response = %s", body)
	}
	active, set, err := w.db.ActiveProfileID(t.Context())
	if err != nil || !set || active != id {
		t.Fatalf("active = %q, %v, %v", active, set, err)
	}

	body = postForm(t, w, "/profiles/"+profile.DefaultID+"/duplicate", nil)
	if !strings.Contains(body, "profile duplicated") || !strings.Contains(body, "Default copy") {
		t.Fatalf("duplicate response = %s", body)
	}
	locals, err = w.db.ListProfiles(t.Context())
	if err != nil || len(locals) != 2 {
		t.Fatalf("profiles after duplicate = %+v, %v", locals, err)
	}
}

func TestProfileDeleteRequiresConfirmationAndFallsBack(t *testing.T) {
	w := newTestWeb(t)
	created, err := w.profiles.Create(t.Context(), "Delete me", "# Delete me")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.profiles.SetActive(t.Context(), created.ID); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, httptest.NewRequest(
		http.MethodGet, "/profiles/"+created.ID+"/confirm-delete", nil,
	))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Delete profile") ||
		!strings.Contains(rec.Body.String(), `hx-delete="/profiles/`+created.ID+`"`) {
		t.Fatalf("confirm delete = %d %s", rec.Code, rec.Body)
	}

	rec = httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/profiles/"+created.ID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE profile = %d: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `hx-swap-oob="innerHTML:#modalTop"`) ||
		!strings.Contains(body, `id="profilesSettings"`) {
		t.Fatalf("delete HTMX response = %s", body)
	}
	active, set, err := w.db.ActiveProfileID(t.Context())
	if err != nil || !set || active != profile.DefaultID {
		t.Fatalf("active after delete = %q, %v, %v", active, set, err)
	}
}

func TestProfileInvalidFormsPreserveInput(t *testing.T) {
	w := newTestWeb(t)
	body := postForm(t, w, "/profiles", url.Values{
		"name": {"My profile"},
		"mode": {"structured"},
	})
	if !strings.Contains(body, "profile content cannot be empty") ||
		!strings.Contains(body, `value="My profile"`) {
		t.Fatalf("empty structured response = %s", body)
	}
	body = postForm(t, w, "/profiles", url.Values{
		"name":    {" "},
		"mode":    {"raw"},
		"content": {"# content"},
	})
	if !strings.Contains(body, "profile name") || !strings.Contains(body, "cannot be blank") {
		t.Fatalf("blank name response = %s", body)
	}
	locals, err := w.db.ListProfiles(t.Context())
	if err != nil || len(locals) != 0 {
		t.Fatalf("invalid form created profiles: %+v, %v", locals, err)
	}
}
