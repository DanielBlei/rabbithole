package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DanielBlei/ai-searcher/internal/config"
	"github.com/DanielBlei/ai-searcher/internal/feeds"
	"github.com/DanielBlei/ai-searcher/internal/store"
)

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
	return New(db, &config.Config{}, ":8080")
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

// A mutation on an unknown id is a 404, not a 500 or a silent write.
func TestToggleUnknownItem(t *testing.T) {
	w := newTestWeb(t)
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/items/nope/seen", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}
