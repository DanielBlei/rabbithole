// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/ingest"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// testIngestManager builds the ingest manager New requires; the manager is
// idle in these tests unless a test drives it.
func testIngestManager(t *testing.T, db *store.Store) *ingest.Manager {
	t.Helper()
	think := false
	cfg := &config.Config{}
	cfg.Inference.Think = &think
	m, err := ingest.NewManager(db, cfg, zerolog.InfoLevel)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m
}

func newTestWeb(t *testing.T) *Web {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Record(context.Background(),
		[]feeds.Item{{ID: "a", Source: "S1", Title: "A", Link: "https://x/a"}},
		nil, time.Now()); err != nil {
		t.Fatalf("Record: %v", err)
	}
	return New(db, &config.Config{}, ":8080", "", testIngestManager(t, db))
}

func post(t *testing.T, w *Web, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST %s: status = %d, want 200; body=%s", path, rec.Code, rec.Body)
	}
	return rec
}

// postFormCode is postForm without the 200 assertion, for the rejection paths.
func postFormCode(w *Web, path string, form url.Values) int {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, req)
	return rec.Code
}

func statusOf(t *testing.T, w *Web, id string) string {
	t.Helper()
	row, err := w.db.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return row.Status
}

// Clicking seen/hide persists the status to the store, and clicking the same
// control again toggles it back to unread — both writes hit the DB, not just
// the rendered row.
func TestToggleSeenHidePersists(t *testing.T) {
	w := newTestWeb(t)

	rec := post(t, w, "/items/a/seen")
	if got := statusOf(t, w, "a"); got != store.StatusRead {
		t.Errorf("after seen: status = %q, want %q", got, store.StatusRead)
	}
	if !strings.Contains(rec.Body.String(), "is-seen") {
		t.Errorf("seen response missing is-seen class; body=%s", rec.Body)
	}

	post(t, w, "/items/a/seen") // un-click
	if got := statusOf(t, w, "a"); got != store.StatusUnread {
		t.Errorf("after seen toggle-off: status = %q, want %q", got, store.StatusUnread)
	}

	post(t, w, "/items/a/hide")
	if got := statusOf(t, w, "a"); got != store.StatusSkipped {
		t.Errorf("after hide: status = %q, want %q", got, store.StatusSkipped)
	}

	post(t, w, "/items/a/hide") // un-click
	if got := statusOf(t, w, "a"); got != store.StatusUnread {
		t.Errorf("after hide toggle-off: status = %q, want %q", got, store.StatusUnread)
	}
}

// seen and hide are mutually exclusive off the unread baseline: hiding an item
// that's currently seen flips it straight to skipped, not back through unread.
func TestSeenHideMutuallyExclusive(t *testing.T) {
	w := newTestWeb(t)

	post(t, w, "/items/a/seen")
	if got := statusOf(t, w, "a"); got != store.StatusRead {
		t.Fatalf("after seen: status = %q, want %q", got, store.StatusRead)
	}
	post(t, w, "/items/a/hide")
	if got := statusOf(t, w, "a"); got != store.StatusSkipped {
		t.Errorf("hide after seen: status = %q, want %q", got, store.StatusSkipped)
	}
}

// scoreOf reads back an item's stored rating, nil when it has none.
func scoreOf(t *testing.T, w *Web, id string) *int {
	t.Helper()
	row, err := w.db.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return row.UserScore
}

// A thumb writes user_score and lights up in the returned row; the same thumb
// again clears the rating rather than rewriting it, so 0 (thumbs down) stays
// distinguishable from unrated.
func TestRateTogglesUserScore(t *testing.T) {
	w := newTestWeb(t)

	body := postForm(t, w, "/items/a/rate", url.Values{"score": {"10"}})
	if got := scoreOf(t, w, "a"); got == nil || *got != 10 {
		t.Fatalf("after thumbs up: user score = %v, want 10", got)
	}
	if !strings.Contains(body, "rate-up is-active") {
		t.Errorf("rated row missing lit up-thumb; body=%s", body)
	}

	body = postForm(t, w, "/items/a/rate", url.Values{"score": {"10"}})
	if got := scoreOf(t, w, "a"); got != nil {
		t.Fatalf("re-posting the same score: user score = %d, want cleared", *got)
	}
	if strings.Contains(body, "is-active") {
		t.Errorf("cleared row still lights a thumb; body=%s", body)
	}

	// Thumbs down is a real 0, not a clear.
	postForm(t, w, "/items/a/rate", url.Values{"score": {"0"}})
	if got := scoreOf(t, w, "a"); got == nil || *got != 0 {
		t.Fatalf("after thumbs down: user score = %v, want 0", got)
	}
}

// A score outside the store's range, or one that isn't a number, is the
// request's fault — a 400, not the 500 the store's own range error would map to.
func TestRateRejectsBadScore(t *testing.T) {
	w := newTestWeb(t)

	for _, score := range []string{"99", "-1", "up", ""} {
		if code := postFormCode(w, "/items/a/rate", url.Values{"score": {score}}); code != http.StatusBadRequest {
			t.Errorf("rate score=%q: status = %d, want 400", score, code)
		}
	}
	if got := scoreOf(t, w, "a"); got != nil {
		t.Errorf("a rejected rate wrote %d, want nothing stored", *got)
	}
	if code := postFormCode(w, "/items/nope/rate", url.Values{"score": {"10"}}); code != http.StatusNotFound {
		t.Errorf("rating an unknown item: status = %d, want 404", code)
	}
}

// newEmptyWeb is newTestWeb's counterpart for the first-run path: a store with
// no items and no ingest history at all.
func newEmptyWeb(t *testing.T) *Web {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "empty.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return New(db, &config.Config{}, ":8080", "", testIngestManager(t, db))
}

// A never-ingested store lands on the first-run zero-state: the ingest chip and
// the side-menu hint both show, and the pane offers the runner rather than an
// empty page.
func TestFeedZeroStateNeverIngested(t *testing.T) {
	body := get(t, newEmptyWeb(t), "/feed")

	for _, want := range []string{
		"ingest never ran", // topbar chip
		`id="navHint"`,     // one-time pointer at the side menu
		"navtab--warn",     // the edge tab it points at, amber
		"Nothing has been ingested yet",
		"run ingest",
		`class="filter filter--source"`, // the bar keeps its shape with no sources
	} {
		if !strings.Contains(body, want) {
			t.Errorf("first-run feed missing %q", want)
		}
	}
	// Nothing to page through, so the pager isn't rendered.
	if strings.Contains(body, `id="btnFirst"`) {
		t.Error("empty feed should not render the pager")
	}
}

// An ingest that ran and brought nothing home reads differently from one that
// never ran: same actions, but it points at the feed list rather than claiming
// the feed was never pulled.
func TestFeedZeroStateAfterEmptyRun(t *testing.T) {
	w := newEmptyWeb(t)
	id, err := w.db.StartIngestRun(context.Background(), store.IngestTriggerManual)
	if err != nil {
		t.Fatalf("StartIngestRun: %v", err)
	}
	if err := w.db.FinishIngestRun(
		context.Background(),
		id,
		store.IngestStatusOK,
		store.IngestCounts{},
		"",
	); err != nil {
		t.Fatalf("FinishIngestRun: %v", err)
	}

	body := get(t, w, "/feed")
	if !strings.Contains(body, "The last run came back empty") {
		t.Errorf("post-run empty feed missing the dry wording; body=%s", body)
	}
	// The run cleared the first-run chrome, so neither the chip nor the hint
	// should still be nagging.
	if strings.Contains(body, "ingest never ran") || strings.Contains(body, `id="navHint"`) {
		t.Error("first-run chrome should clear once a run is recorded")
	}
}

// With items in the store, an empty page is the filters' doing — say so, and
// offer the way back rather than an ingest that would change nothing.
func TestFeedZeroStateFiltered(t *testing.T) {
	body := get(t, newTestWeb(t), "/feed?view=1")

	if !strings.Contains(body, "No items match these filters") {
		t.Errorf("filtered-empty feed missing its wording; body=%s", body)
	}
	if !strings.Contains(body, "clear filters") {
		t.Error("filtered-empty feed missing the clear-filters way out")
	}
	if strings.Contains(body, "run ingest") {
		t.Error("filtered-empty feed should not offer an ingest")
	}
}

// The zero-state quotes the view it's empty for, so the shell line has to carry
// the active filters — including the all-cleared case, which is the reason the
// page is empty at all.
func TestPageDataCmd(t *testing.T) {
	tests := []struct {
		name string
		data pageData
		want string
	}{
		{"default", pageData{FilterShowUnread: true}, "rabbithole feed --unread"},
		{
			"window and statuses",
			pageData{FilterShowUnread: true, FilterShowSeen: true, FilterPublished: "7d"},
			"rabbithole feed --unread --seen --published 7d",
		},
		{
			"bookmark library",
			pageData{FilterShowUnread: true, FilterShowSeen: true, FilterShowHidden: true, FilterShowBookmark: true},
			"rabbithole feed --unread --seen --hidden --bookmarked",
		},
		{"no status picked", pageData{}, "rabbithole feed --status none"},
		{
			"search, quoted because it's free text",
			pageData{FilterShowUnread: true, FilterSearch: "edge cases"},
			`rabbithole feed --unread --search "edge cases"`,
		},
		{
			// Only the picked chips make it into the line, repeated per pick.
			"multi-select chips",
			pageData{
				FilterShowUnread: true,
				Sources:          []pickChip{{Value: "Red Hat", On: true}, {Value: "HF blog"}},
				Tags:             []pickChip{{Value: "AI", On: true}, {Value: "Infra", On: true}},
			},
			`rabbithole feed --unread --source "Red Hat" --tag "AI" --tag "Infra"`,
		},
		{
			"custom range wins over the window",
			pageData{
				FilterShowUnread: true,
				FilterCustom:     true,
				FilterPublished:  "7d",
				FilterFrom:       "2026-01-02",
				FilterTo:         "2026-01-09",
			},
			"rabbithole feed --unread --from 2026-01-02 --to 2026-01-09",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.data.cmd(); got != tt.want {
				t.Errorf("cmd() = %q, want %q", got, tt.want)
			}
		})
	}
}

// The search control is a real query, not only a client-side filter: search=
// narrows what the server renders (so an item below the page's row cap is
// reachable), and the text comes back in the field it was typed into.
func TestFeedSearch(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	items := []feeds.Item{
		{ID: "a", Source: "S1", Title: "Kubernetes at the edge", Link: "https://x/a"},
		{ID: "b", Source: "S1", Title: "Fine-tuning on one GPU", Link: "https://x/b"},
	}
	if err := db.Record(context.Background(), items, nil, time.Now()); err != nil {
		t.Fatalf("Record: %v", err)
	}
	w := New(db, &config.Config{}, ":8080", "", testIngestManager(t, db))

	body := get(t, w, "/feed?view=1&unread=1&search=kuber")
	if !strings.Contains(body, "Kubernetes at the edge") {
		t.Errorf("search dropped the matching item; body=%s", body)
	}
	if strings.Contains(body, "Fine-tuning on one GPU") {
		t.Error("search rendered an item it doesn't match")
	}
	if !strings.Contains(body, `value="kuber"`) {
		t.Error("search text didn't come back in the search field")
	}
}

// Source and tag chips are store queries, not a sieve over the rendered rows:
// they narrow what comes back, their options survive being picked, and the
// clear-all way out appears only once something is narrowing.
func TestFeedSourceAndTagFilters(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	items := []feeds.Item{
		{ID: "a", Source: "Red Hat", Title: "Edge computing", Link: "https://x/a", Tags: []string{"Infra"}},
		{ID: "b", Source: "HF blog", Title: "Small models", Link: "https://x/b", Tags: []string{"AI"}},
	}
	if err := db.Record(context.Background(), items, nil, time.Now()); err != nil {
		t.Fatalf("Record: %v", err)
	}
	w := New(db, &config.Config{}, ":8080", "", testIngestManager(t, db))

	body := get(t, w, "/feed?view=1&unread=1&source=Red+Hat")
	if !strings.Contains(body, "Edge computing") || strings.Contains(body, "Small models") {
		t.Errorf("source filter didn't narrow the render; body=%s", body)
	}
	// The whole point of sourcing the options from the store: the feed you
	// filtered out is still on offer, so you can switch to it.
	if !strings.Contains(body, `value="HF blog"`) {
		t.Error("source chips lost the option that isn't in the current view")
	}
	if !strings.Contains(body, "filter__clear") {
		t.Error("a narrowed bar should offer the reset")
	}

	body = get(t, w, "/feed?view=1&unread=1&tag=AI")
	if !strings.Contains(body, "Small models") || strings.Contains(body, "Edge computing") {
		t.Errorf("tag filter didn't narrow the render; body=%s", body)
	}

	// Both at once, on rows that satisfy neither together.
	body = get(t, w, "/feed?view=1&unread=1&source=Red+Hat&tag=AI")
	if strings.Contains(body, "Edge computing") || strings.Contains(body, "Small models") {
		t.Error("source and tag should AND, not OR")
	}

	if body = get(t, w, "/feed"); strings.Contains(body, "filter__clear") {
		t.Error("an unfiltered bar should not offer the reset")
	}
}

// A mutation on an unknown id is a 404, not a 500 or a silent write.
func TestToggleUnknownItem(t *testing.T) {
	w := newTestWeb(t)
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/items/nope/seen", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}
