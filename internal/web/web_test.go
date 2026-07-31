// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"net/http"
	"net/http/httptest"
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

// A mutation on an unknown id is a 404, not a 500 or a silent write.
func TestToggleUnknownItem(t *testing.T) {
	w := newTestWeb(t)
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/items/nope/seen", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}
