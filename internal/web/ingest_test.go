package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/ingest"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// newIngestWeb builds a Web whose manager can really run: a profile file
// exists and the config has no feeds, so a triggered run completes quickly
// (nothing to fetch or score) and records an ok history row.
func newIngestWeb(t *testing.T) *Web {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	profile := filepath.Join(t.TempDir(), "profile.md")
	if err := os.WriteFile(profile, []byte("# interests"), 0o644); err != nil {
		t.Fatalf("write profile: %v", err)
	}
	think := false
	cfg := &config.Config{Profile: profile}
	cfg.Inference.Think = &think
	m, err := ingest.NewManager(db, cfg, zerolog.InfoLevel)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return New(db, cfg, ":8080", "", m)
}

func get(t *testing.T, w *Web, path string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: status = %d, want 200; body=%s", path, rec.Code, rec.Body)
	}
	return rec.Body.String()
}

// The modal renders the fresh-store state, and a triggered run round-trips:
// history gains an ok row, the status body reports it, and the chip goes back
// to hidden (idle) once the run is done.
func TestIngestModalAndRun(t *testing.T) {
	w := newIngestWeb(t)

	out := get(t, w, "/ingest")
	if !strings.Contains(out, "never ran") {
		t.Errorf("fresh modal missing never-ran state; body=%s", out)
	}

	post(t, w, "/ingest/run")

	// The run is backgrounded; with no feeds it finishes near-instantly.
	deadline := time.Now().Add(5 * time.Second)
	for {
		last, err := w.db.LastIngestRun(t.Context())
		if err != nil {
			t.Fatalf("LastIngestRun: %v", err)
		}
		// Both must settle: the row is finalized just before the manager clears
		// its in-memory flag, and the assertions below read the manager.
		if last != nil && last.Status != store.IngestStatusRunning && !w.ing.Status().Running {
			if last.Status != store.IngestStatusOK {
				t.Fatalf("run finished %q, want ok: %+v", last.Status, last)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("run did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}

	out = get(t, w, "/ingest/status")
	if !strings.Contains(out, "✓ ok") {
		t.Errorf("status body missing ok runline; body=%s", out)
	}
	if !strings.Contains(out, `id="ingChip"`) || !strings.Contains(out, "hx-swap-oob") {
		t.Errorf("status response missing OOB chip; body=%s", out)
	}
	if !strings.Contains(out, "ingest complete") {
		t.Errorf("status body missing captured log lines; body=%s", out)
	}
	// Idle chip after an ok run: present for OOB targeting but hidden.
	if !strings.Contains(out, "hidden") {
		t.Errorf("idle chip should render hidden; body=%s", out)
	}
}
