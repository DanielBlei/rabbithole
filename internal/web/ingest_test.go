// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"net/http"
	"net/http/httptest"
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

	think := false
	cfg := &config.Config{}
	cfg.Inference.Think = &think
	m, err := ingest.NewManager(db, config.Static(*cfg), zerolog.InfoLevel)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return New(db, config.Static(*cfg), m)
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

	// Reopening once the run is done starts over: the finished run stays as the
	// summary and in the history, but its completion banner and log pane are the
	// watched run's, not the next one's.
	out = get(t, w, "/ingest")
	if !strings.Contains(out, "✓ ok") {
		t.Errorf("reopened modal missing ok runline; body=%s", out)
	}
	if !strings.Contains(out, "ing__hist") {
		t.Errorf("reopened modal missing run history; body=%s", out)
	}
	if strings.Contains(out, "ing__done") {
		t.Errorf("reopened modal still shows the completion banner; body=%s", out)
	}
	if strings.Contains(out, "ingest complete") {
		t.Errorf("reopened modal still shows the finished run's log; body=%s", out)
	}
}

// waitIngestIdle blocks until the run started through w has finished and the
// manager has cleared its live flag.
func waitIngestIdle(t *testing.T, w *Web) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		last, err := w.db.LastIngestRun(t.Context())
		if err != nil {
			t.Fatalf("LastIngestRun: %v", err)
		}
		if last != nil && last.Status != store.IngestStatusRunning && !w.ing.Status().Running {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("run did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// postBody posts to path and returns the 200 response body.
func postBody(t *testing.T, w *Web, path string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST %s: status = %d, want 200; body=%s", path, rec.Code, rec.Body)
	}
	return rec.Body.String()
}

// The Sources bar with no run yet: no counts, no Cancel, no poll, and Run is
// disabled because nothing is enabled.
func TestIngestBarIdle(t *testing.T) {
	w := newIngestWeb(t)
	out := get(t, w, "/ingest/status?view=bar")
	for _, want := range []string{"No ingestion run yet", `data-status="none"`, "Run ingestion"} {
		if !strings.Contains(out, want) {
			t.Errorf("idle bar missing %q; body=%s", want, out)
		}
	}
	if !strings.Contains(out, `class="ingb__cnt is-idle"`) {
		t.Errorf("idle bar should keep the counts row in the layout but invisible; body=%s", out)
	}
	if strings.Contains(out, ">Cancel<") {
		t.Errorf("idle bar renders Cancel; body=%s", out)
	}
	if strings.Contains(out, "hx-trigger") {
		t.Errorf("idle bar should not poll; body=%s", out)
	}
	if !strings.Contains(out, "disabled") {
		t.Errorf("run button should be disabled with no enabled feeds; body=%s", out)
	}
}

// A finished ok run reads as "Last run ok", with its duration and counts shown.
func TestIngestBarFinishedRun(t *testing.T) {
	w := newIngestWeb(t)
	post(t, w, "/ingest/run")
	waitIngestIdle(t, w)

	out := get(t, w, "/ingest/status?view=bar")
	for _, want := range []string{"Last run ok", "took ", `data-status="ok"`, `class="ingb__c"`} {
		if !strings.Contains(out, want) {
			t.Errorf("finished bar missing %q; body=%s", want, out)
		}
	}
	if strings.Contains(out, "hx-trigger") {
		t.Errorf("finished bar should not poll; body=%s", out)
	}
}

// The rerun label counts the enabled feeds, and the variant survives the query
// round trip into the compact class.
func TestIngestBarRerunLabel(t *testing.T) {
	cases := []struct {
		name  string
		feeds []config.Feed
		want  string
	}{
		{
			name: "several enabled",
			feeds: []config.Feed{
				{Name: "Alpha", URL: "https://alpha.test/feed"},
				{Name: "Beta", URL: "https://beta.test/feed"},
				{Name: "Parked", URL: "https://parked.test/feed", Enabled: boolPtr(false)},
			},
			want: "Re-run ingestion for all 2 enabled feeds</button>",
		},
		{
			name:  "one enabled",
			feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}},
			want:  "Re-run ingestion for all 1 enabled feed</button>",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, _ := sourcesTestWeb(t, config.FeedsDoc{Feeds: tc.feeds})
			out := get(t, w, "/ingest/status?view=bar&rerun=1&compact=1")
			if !strings.Contains(out, tc.want) {
				t.Errorf("rerun bar missing %q; body=%s", tc.want, out)
			}
			if !strings.Contains(out, "ingb--compact") {
				t.Errorf("compact variant lost; body=%s", out)
			}
			if !strings.Contains(out, "rerun=1") || !strings.Contains(out, "compact=1") {
				t.Errorf("actions do not carry the variant; body=%s", out)
			}
		})
	}
}

// Run and Cancel answer with the bar when asked for it (view=bar), and with the
// modal body otherwise, so the modal's own Run/Cancel are unchanged.
func TestIngestBarActionsAndModalUnchanged(t *testing.T) {
	w := newIngestWeb(t)

	out := postBody(t, w, "/ingest/run?view=bar&compact=1")
	if !strings.Contains(out, "ingb--compact") || strings.Contains(out, `id="ingestBody"`) {
		t.Errorf("bar run should answer with the bar only; body=%s", out)
	}
	waitIngestIdle(t, w)

	out = postBody(t, w, "/ingest/cancel?view=bar")
	if !strings.Contains(out, `data-status="ok"`) || strings.Contains(out, `id="ingestBody"`) {
		t.Errorf("bar cancel should answer with the bar only; body=%s", out)
	}

	out = postBody(t, w, "/ingest/run")
	if !strings.Contains(out, `id="ingestBody"`) || strings.Contains(out, "ingb") {
		t.Errorf("modal run should answer with the modal body only; body=%s", out)
	}
	waitIngestIdle(t, w)

	out = postBody(t, w, "/ingest/cancel")
	if !strings.Contains(out, `id="ingestBody"`) || strings.Contains(out, "ingb") {
		t.Errorf("modal cancel should answer with the modal body only; body=%s", out)
	}
}

// The running note is out of band on the bar responses of a page that has one
// (note=1), and hidden while idle. A page without it gets no note, which would
// have no target to land on.
func TestIngestBarNoteHiddenWhenIdle(t *testing.T) {
	w := newIngestWeb(t)
	out := get(t, w, "/ingest/status?view=bar&note=1")
	if !strings.Contains(out, `id="ingestNote" role="status" hx-swap-oob="true" hidden>`) {
		t.Errorf("idle bar response should carry a hidden running note out of band; body=%s", out)
	}
	if out := get(t, w, "/ingest/status?view=bar"); strings.Contains(out, "ingestNote") {
		t.Errorf("a bar on a page without the note carried one; body=%s", out)
	}
}

// The bar's actions carry note=1 forward, so the swapped bar keeps carrying it.
func TestIngestBarQueryCarriesTheNote(t *testing.T) {
	q := ingestBarData{Rerun: true, Compact: true, Note: true}.Query()
	for _, want := range []string{"view=bar", "rerun=1", "compact=1", "note=1"} {
		if !strings.Contains(q, want) {
			t.Errorf("Query() = %q, missing %q", q, want)
		}
	}
	if q := (ingestBarData{}).Query(); strings.Contains(q, "note") {
		t.Errorf("a plain bar's Query() = %q carries a note", q)
	}
}
