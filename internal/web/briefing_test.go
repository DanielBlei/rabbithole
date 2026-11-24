// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/briefing"
	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/profile"
	"github.com/DanielBlei/rabbithole/internal/store"
)

type blockingGenerator struct {
	mu      sync.Mutex
	calls   int
	started chan struct{}
	entered chan struct{}
	release chan struct{}
	reply   string
	err     error
}

func (g *blockingGenerator) ChatJSON(context.Context, string, string, json.RawMessage, int) (string, error) {
	g.mu.Lock()
	g.calls++
	g.mu.Unlock()
	if g.started != nil {
		g.started <- struct{}{}
	}
	if g.entered != nil {
		select {
		case <-g.entered:
		default:
			close(g.entered)
		}
	}
	if g.release != nil {
		<-g.release
	}
	return g.reply, g.err
}

func (g *blockingGenerator) callCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

// briefingPeriodDay is yesterday, the newest daily period a generate accepts.
func briefingPeriodDay() string { return time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02") }

type failingWebProfiles struct{}

func (failingWebProfiles) Resolve(context.Context) (profile.Snapshot, error) {
	return profile.Snapshot{}, errors.New("profile store down")
}

func seedScoredItem(t *testing.T, w *Web) {
	t.Helper()
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Truncate(24 * time.Hour).Add(12 * time.Hour)
	if err := w.db.Record(context.Background(),
		[]feeds.Item{{ID: "scored", Source: "S1", Title: "Scored", Link: "https://x/scored", Published: yesterday}},
		nil, time.Now()); err != nil {
		t.Fatalf("Record: %v", err)
	}
	n, err := w.db.ReplaceItemScores(context.Background(), []store.ScoreReplacement{{
		ItemID: "scored", Score: 9, Reason: "worth it", Model: "test",
		ProfileID: "builtin-default", ProfileName: "Default", ProfileHash: "hash",
	}})
	if err != nil {
		t.Fatalf("ReplaceItemScores: %v", err)
	}
	if n != 1 {
		t.Fatalf("seeded %d items, want 1", n)
	}
}

const webBriefingJSON = `{
  "executive_summary": "One article mattered.",
  "developments": [
    {"headline": "A development", "synthesis": "It happened. <script>alert(1)</script>", "why_it_matters": "Because.", "item_ids": [1]}
  ],
  "emerging_topics": ["A topic"],
  "recommended_reading": [1]
}`

func newBriefingWeb(t *testing.T, gen briefing.Generator) *Web {
	t.Helper()
	w := newTestWeb(t)
	seedScoredItem(t, w)
	w.src = config.Static(config.Config{Inference: config.InferenceConfig{
		Provider: "vllm", Model: "test-model", Host: "http://127.0.0.1:1",
	}})
	w.SetBriefingGenerator(func(context.Context, config.Config) (briefing.Generator, error) {
		return gen, nil
	})
	return w
}

func TestBriefingPageAndGenerateFlow(t *testing.T) {
	gen := &blockingGenerator{entered: make(chan struct{}), release: make(chan struct{}), reply: webBriefingJSON}
	w := newBriefingWeb(t, gen)

	page := httptest.NewRecorder()
	w.Routes().ServeHTTP(page, httptest.NewRequest(http.MethodGet,
		"/briefing?kind=daily&period="+briefingPeriodDay(), nil))
	if page.Code != http.StatusOK {
		t.Fatalf("page status = %d", page.Code)
	}
	if !strings.Contains(page.Body.String(), "No briefing is stored") {
		t.Errorf("empty state missing:\n%s", page.Body.String())
	}
	if !strings.Contains(page.Body.String(), `href="/briefing"`) {
		t.Error("nav entry missing")
	}

	form := url.Values{"kind": {"daily"}, "period": {briefingPeriodDay()}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/briefing/generate", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("generate status = %d; body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "Generating this briefing") {
		t.Errorf("generating fragment missing:\n%s", rec.Body.String())
	}
	select {
	case <-gen.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("model was never called")
	}
	close(gen.release)

	start, _ := time.ParseInLocation("2006-01-02", briefingPeriodDay(), time.UTC)
	deadline := time.Now().Add(3 * time.Second)
	for {
		b, err := w.db.BriefingForPeriod(context.Background(), "daily", start)
		if err == nil && b.Status == store.BriefingReady {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("briefing never became ready: %+v %v", b, err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	stored, err := w.db.BriefingForPeriod(context.Background(), "daily", start)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != store.BriefingReady || !strings.Contains(stored.BodyMD, "One article mattered") {
		t.Errorf("stored briefing = %+v", stored)
	}
	if stored.Provider == "" || stored.Model == "" {
		t.Errorf("provenance missing: %+v", stored)
	}

	ready := httptest.NewRecorder()
	w.Routes().
		ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/briefing?kind=daily&period="+briefingPeriodDay(), nil))
	body := ready.Body.String()
	for _, want := range []string{"One article mattered", "A development", "Download markdown", "Regenerate"} {
		if !strings.Contains(body, want) {
			t.Errorf("ready page missing %q", want)
		}
	}
	if strings.Contains(body, "<script>alert") {
		t.Error("model markdown was not sanitised")
	}
	if strings.Contains(body, "every 2s") {
		t.Error("ready page kept the status poll")
	}
}

func TestBriefingStatusTransitions(t *testing.T) {
	w := newTestWeb(t)
	start, _ := time.ParseInLocation("2006-01-02", briefingPeriodDay(), time.UTC)
	if _, err := w.db.SaveBriefing(context.Background(), store.Briefing{
		ID: "b", PeriodKind: "daily", PeriodStart: start, PeriodEnd: start.AddDate(0, 0, 1),
		Status: store.BriefingGenerating,
	}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/briefing/status?kind=daily&period="+briefingPeriodDay(), nil))
	if !strings.Contains(rec.Body.String(), "every 2s") || !strings.Contains(rec.Body.String(), "Generating") {
		t.Errorf("generating status lost its poll:\n%s", rec.Body.String())
	}

	b, _ := w.db.BriefingForPeriod(context.Background(), "daily", start)
	b.Status = store.BriefingReady
	b.BodyMD = "# Ready\n\nBody."
	if _, err := w.db.SaveBriefing(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/briefing/status?kind=daily&period="+briefingPeriodDay(), nil))
	if strings.Contains(rec.Body.String(), "every 2s") {
		t.Errorf("ready status kept polling:\n%s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Ready") {
		t.Errorf("ready status missing body:\n%s", rec.Body.String())
	}
}

func TestBriefingExportDownloadsTheStoredBody(t *testing.T) {
	w := newTestWeb(t)
	start, _ := time.ParseInLocation("2006-01-02", briefingPeriodDay(), time.UTC)
	body := "# Briefing\n\nStored body.\n"
	if _, err := w.db.SaveBriefing(context.Background(), store.Briefing{
		ID: "b", PeriodKind: "daily", PeriodStart: start, PeriodEnd: start.AddDate(0, 0, 1),
		Status: store.BriefingReady, BodyMD: body,
	}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/briefing/export?kind=daily&period="+briefingPeriodDay(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
		t.Errorf("Content-Type = %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") ||
		!strings.Contains(cd, ".md") {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if rec.Body.String() != body {
		t.Errorf("export body = %q, want the stored body", rec.Body.String())
	}

	missing := httptest.NewRecorder()
	w.Routes().ServeHTTP(missing, httptest.NewRequest(http.MethodGet,
		"/briefing/export?kind=daily&period=2020-01-01", nil))
	if missing.Code != http.StatusNotFound {
		t.Errorf("missing export status = %d, want 404", missing.Code)
	}
}

func TestBriefingGenerateWithHeuristicIsAProductState(t *testing.T) {
	w := newTestWeb(t)
	seedScoredItem(t, w)
	w.src = config.Static(config.Config{Inference: config.InferenceConfig{Provider: "heuristic"}})

	form := url.Values{"kind": {"daily"}, "period": {briefingPeriodDay()}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/briefing/generate", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("generate status = %d; body=%s", rec.Code, rec.Body)
	}

	start, _ := time.ParseInLocation("2006-01-02", briefingPeriodDay(), time.UTC)
	deadline := time.Now().Add(2 * time.Second)
	for {
		b, err := w.db.BriefingForPeriod(context.Background(), "daily", start)
		if err == nil && b.Status == store.BriefingFailed {
			if !strings.Contains(b.Error, "needs a model provider") {
				t.Errorf("error = %q", b.Error)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("briefing never failed: %+v %v", b, err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	page := httptest.NewRecorder()
	w.Routes().ServeHTTP(page, httptest.NewRequest(http.MethodGet,
		"/briefing?kind=daily&period="+briefingPeriodDay(), nil))
	if !strings.Contains(page.Body.String(), "needs a model provider") {
		t.Errorf("failure state missing from page:\n%s", page.Body.String())
	}
}

func TestBriefingNextNavigationStopsAtLastCompletedPeriod(t *testing.T) {
	w := newTestWeb(t)
	for _, kind := range []string{"daily", "weekly"} {
		last, err := briefing.PeriodFor(kind, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			name     string
			period   briefing.Period
			wantNext bool
		}{
			{"next lands on the last completed period", last.Previous(), true},
			{"next would pass the last completed period", last, false},
		} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				rec := httptest.NewRecorder()
				path := "/briefing?kind=" + kind + "&period=" + tc.period.Start.Format("2006-01-02")
				w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
				body := rec.Body.String()

				nextHref := `href="/briefing?kind=` + kind + `&period=` +
					tc.period.Next().Start.Format("2006-01-02") + `">Next <span aria-hidden="true">&rarr;</span></a>`
				disabled := `aria-disabled="true">Next <span aria-hidden="true">&rarr;</span></span>`
				if tc.wantNext {
					if !strings.Contains(body, nextHref) {
						t.Errorf("Next link %q missing:\n%s", nextHref, body)
					}
					if strings.Contains(body, disabled) {
						t.Errorf("Next is disabled on a reachable period:\n%s", body)
					}
					return
				}
				if !strings.Contains(body, disabled) {
					t.Errorf("Next is not disabled past the last completed period:\n%s", body)
				}
				if strings.Contains(body, nextHref) {
					t.Errorf("Next link reaches a future period:\n%s", body)
				}
			})
		}
	}
}

func TestBriefingGenerateSingleFlightAcrossRequests(t *testing.T) {
	gen := &blockingGenerator{
		started: make(chan struct{}), entered: make(chan struct{}),
		release: make(chan struct{}), reply: webBriefingJSON,
	}
	w := newBriefingWeb(t, gen)

	form := url.Values{"kind": {"daily"}, "period": {briefingPeriodDay()}}
	newRequest := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/briefing/generate", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return req
	}

	first := httptest.NewRecorder()
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		w.Routes().ServeHTTP(first, newRequest())
	}()
	select {
	case <-gen.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first request never reached the model")
	}
	<-firstDone

	second := httptest.NewRecorder()
	w.Routes().ServeHTTP(second, newRequest())
	if second.Code != http.StatusOK {
		t.Fatalf("second generate status = %d; body=%s", second.Code, second.Body)
	}
	if !strings.Contains(second.Body.String(), "Generating this briefing") {
		t.Errorf("second request did not receive the generating state:\n%s", second.Body.String())
	}
	select {
	case <-gen.started:
		t.Error("second request started a duplicate model call")
	case <-time.After(200 * time.Millisecond):
	}
	if got := gen.callCount(); got != 1 {
		t.Errorf("model calls = %d, want 1 across concurrent requests", got)
	}

	close(gen.release)
	start, _ := time.ParseInLocation("2006-01-02", briefingPeriodDay(), time.UTC)
	deadline := time.Now().Add(3 * time.Second)
	for {
		b, err := w.db.BriefingForPeriod(context.Background(), "daily", start)
		if err == nil && b.Status == store.BriefingReady {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("briefing never became ready: %+v %v", b, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// blockingWebProfiles stalls profile resolution so a generate request sits
// deterministically inside the single-flight window after the flight is taken
// but before the generating row is persisted.
type blockingWebProfiles struct {
	entered chan struct{}
	release chan struct{}
}

func (b blockingWebProfiles) Resolve(context.Context) (profile.Snapshot, error) {
	close(b.entered)
	<-b.release
	return profile.Default(), nil
}

// TestBriefingGenerateRendersGeneratingDuringPrePersistWindow pins the web
// half of the single-flight fix: the generate response renders from
// svc.Start's return value, so a request that joins an in-flight generation
// inside the pre-persistence window gets the generating panel, not the "No
// briefing is stored" empty state with an active Generate button.
func TestBriefingGenerateRendersGeneratingDuringPrePersistWindow(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	gen := &blockingGenerator{reply: webBriefingJSON}
	w := newBriefingWeb(t, gen)
	w.briefingSvc = briefing.New(w.db, blockingWebProfiles{entered: entered, release: release},
		func(context.Context) (briefing.Generator, error) { return gen, nil })

	form := url.Values{"kind": {"daily"}, "period": {briefingPeriodDay()}}
	newRequest := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/briefing/generate", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return req
	}

	first := httptest.NewRecorder()
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		w.Routes().ServeHTTP(first, newRequest())
	}()
	<-entered // the owner holds the flight; no generating row exists yet

	second := httptest.NewRecorder()
	w.Routes().ServeHTTP(second, newRequest())
	if second.Code != http.StatusOK {
		t.Fatalf("joining generate status = %d; body=%s", second.Code, second.Body)
	}
	body := second.Body.String()
	if !strings.Contains(body, "Generating this briefing") {
		t.Errorf("joining request did not receive the generating panel:\n%s", body)
	}
	if strings.Contains(body, "No briefing is stored for this period yet") {
		t.Errorf("joining request received the empty state:\n%s", body)
	}
	if !strings.Contains(body, `type="submit" disabled`) {
		t.Errorf("joining request left the Generate button enabled:\n%s", body)
	}

	close(release)
	<-firstDone
	start, _ := time.ParseInLocation("2006-01-02", briefingPeriodDay(), time.UTC)
	deadline := time.Now().Add(3 * time.Second)
	for {
		b, err := w.db.BriefingForPeriod(context.Background(), "daily", start)
		if err == nil && b.Status == store.BriefingReady {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("briefing never became ready: %+v %v", b, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestBriefingStatusShowsGeneratingDuringPrePersistWindow pins the read half
// of the single-flight fix: the status poll and the page render consult the
// service's in-memory state, so a reader that arrives during the window
// before the owner's first write still sees the generating panel instead of
// the empty state with an active Generate button.
func TestBriefingStatusShowsGeneratingDuringPrePersistWindow(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	gen := &blockingGenerator{reply: webBriefingJSON}
	w := newBriefingWeb(t, gen)
	w.briefingSvc = briefing.New(w.db, blockingWebProfiles{entered: entered, release: release},
		func(context.Context) (briefing.Generator, error) { return gen, nil })

	form := url.Values{"kind": {"daily"}, "period": {briefingPeriodDay()}}
	owner := httptest.NewRequest(http.MethodPost, "/briefing/generate", strings.NewReader(form.Encode()))
	owner.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	first := httptest.NewRecorder()
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		w.Routes().ServeHTTP(first, owner)
	}()
	<-entered // the owner holds the flight; no generating row exists yet

	readers := []struct{ name, path string }{
		{"status poll", "/briefing/status?kind=daily&period=" + briefingPeriodDay()},
		{"page", "/briefing?kind=daily&period=" + briefingPeriodDay()},
	}
	for _, rd := range readers {
		rec := httptest.NewRecorder()
		w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, rd.path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d; body=%s", rd.name, rec.Code, rec.Body)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "Generating this briefing") {
			t.Errorf("%s did not receive the generating panel:\n%s", rd.name, body)
		}
		if strings.Contains(body, "No briefing is stored for this period yet") {
			t.Errorf("%s received the empty state:\n%s", rd.name, body)
		}
		if !strings.Contains(body, `type="submit" disabled`) {
			t.Errorf("%s left the Generate button enabled:\n%s", rd.name, body)
		}
	}

	close(release)
	<-firstDone
	start, _ := time.ParseInLocation("2006-01-02", briefingPeriodDay(), time.UTC)
	deadline := time.Now().Add(3 * time.Second)
	for {
		b, err := w.db.BriefingForPeriod(context.Background(), "daily", start)
		if err == nil && b.Status == store.BriefingReady {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("briefing never became ready: %+v %v", b, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The poll converges on the stored result once the run finishes.
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/briefing/status?kind=daily&period="+briefingPeriodDay(), nil))
	if !strings.Contains(rec.Body.String(), "One article mattered.") {
		t.Errorf("status after completion did not render the briefing:\n%s", rec.Body)
	}
}

// TestBriefingRestartRecoveryRendersRetry pins the UI half of the restart
// recovery: after the boot sweep (cmd/serve.go runs it before serving, and
// aborts the boot when it fails) fails a row a previous process left
// generating, the page explains the interruption and offers a retry instead
// of polling a generation that can never finish.
func TestBriefingRestartRecoveryRendersRetry(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "restart.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Record(context.Background(),
		[]feeds.Item{{ID: "a", Source: "S1", Title: "A", Link: "https://x/a"}},
		nil, time.Now()); err != nil {
		t.Fatalf("Record: %v", err)
	}
	start := time.Date(2026, 1, 6, 0, 0, 0, 0, time.UTC)
	if _, err := db.SaveBriefing(context.Background(), store.Briefing{
		ID: "briefing-orphaned", PeriodKind: "daily",
		PeriodStart: start, PeriodEnd: start.AddDate(0, 0, 1),
		Status: store.BriefingGenerating, ItemCount: 3,
	}); err != nil {
		t.Fatalf("SaveBriefing: %v", err)
	}

	// The boot step serve performs before serving, over the same database.
	if n, err := db.RecoverInterruptedBriefings(context.Background()); err != nil || n != 1 {
		t.Fatalf("RecoverInterruptedBriefings = (%d, %v), want (1, nil)", n, err)
	}
	w := New(db, config.Static{}, testIngestManager(t, db))

	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/briefing?kind=daily&period=2026-01-06", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /briefing status = %d; body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "could not be generated") {
		t.Errorf("recovered row did not render as failed:\n%s", body)
	}
	if !strings.Contains(body, "generation was interrupted") {
		t.Errorf("recovered row lost the retry hint:\n%s", body)
	}
	if !strings.Contains(body, ">Try again<") {
		t.Errorf("recovered row offers no retry:\n%s", body)
	}
}

func TestBriefingRejectsBadPeriod(t *testing.T) {
	w := newTestWeb(t)
	for _, path := range []string{
		"/briefing?kind=monthly",
		"/briefing?kind=daily&period=not-a-date",
		"/briefing/status?kind=weekly&period=2026-13-01",
	} {
		rec := httptest.NewRecorder()
		w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", path, rec.Code)
		}
	}
}

func TestBriefingFuturePeriodIsNotGenerable(t *testing.T) {
	w := newTestWeb(t)
	future := time.Now().UTC().AddDate(0, 0, 2).Format("2006-01-02")

	form := url.Values{"kind": {"daily"}, "period": {future}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/briefing/generate", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("future generate status = %d, want 400", rec.Code)
	}

	page := httptest.NewRecorder()
	w.Routes().ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/briefing?kind=daily&period="+future, nil))
	if page.Code != http.StatusOK {
		t.Fatalf("future page status = %d", page.Code)
	}
	body := page.Body.String()
	if !strings.Contains(body, "No briefing is stored") {
		t.Errorf("future page missing empty state:\n%s", body)
	}
	if strings.Contains(body, `hx-post="/briefing/generate"`) {
		t.Errorf("future page offers generate:\n%s", body)
	}
}

func TestBriefingStartFailureShowsInlineMessage(t *testing.T) {
	gen := &blockingGenerator{reply: webBriefingJSON}
	w := newBriefingWeb(t, gen)
	w.briefingSvc = briefing.New(w.db, failingWebProfiles{},
		func(context.Context) (briefing.Generator, error) { return gen, nil })

	form := url.Values{"kind": {"daily"}, "period": {briefingPeriodDay()}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/briefing/generate", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("start failure status = %d; body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "Could not start the briefing") {
		t.Errorf("start failure message missing:\n%s", rec.Body.String())
	}
	if got := gen.callCount(); got != 0 {
		t.Errorf("model calls = %d, want 0 after a start failure", got)
	}
}

func TestBriefingReadyRowShowsRegenerateFailure(t *testing.T) {
	w := newTestWeb(t)
	start, _ := time.ParseInLocation("2006-01-02", briefingPeriodDay(), time.UTC)
	if _, err := w.db.SaveBriefing(context.Background(), store.Briefing{
		ID: "b", PeriodKind: "daily", PeriodStart: start, PeriodEnd: start.AddDate(0, 0, 1),
		Status: store.BriefingReady, BodyMD: "# Kept\n\nPrior body.", Error: "model timed out",
	}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/briefing?kind=daily&period="+briefingPeriodDay(), nil))
	body := rec.Body.String()
	if !strings.Contains(body, "Regenerate failed: model timed out") {
		t.Errorf("regenerate failure missing:\n%s", body)
	}
	if !strings.Contains(body, "Prior body.") {
		t.Errorf("prior body missing:\n%s", body)
	}
}

func TestBriefingGeneratingRowShowsPriorBody(t *testing.T) {
	w := newTestWeb(t)
	start, _ := time.ParseInLocation("2006-01-02", briefingPeriodDay(), time.UTC)
	if _, err := w.db.SaveBriefing(context.Background(), store.Briefing{
		ID: "b", PeriodKind: "daily", PeriodStart: start, PeriodEnd: start.AddDate(0, 0, 1),
		Status: store.BriefingGenerating, BodyMD: "# Old\n\nOld body.",
	}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/briefing?kind=daily&period="+briefingPeriodDay(), nil))
	body := rec.Body.String()
	if !strings.Contains(body, "Generating this briefing") {
		t.Errorf("generating state missing:\n%s", body)
	}
	if !strings.Contains(body, "Old body.") {
		t.Errorf("prior body missing from generating row:\n%s", body)
	}
	if !strings.Contains(body, "Download markdown") {
		t.Errorf("download link missing from generating row with a body:\n%s", body)
	}
}

func TestBriefingBodyDropsImages(t *testing.T) {
	w := newTestWeb(t)
	start, _ := time.ParseInLocation("2006-01-02", briefingPeriodDay(), time.UTC)
	if _, err := w.db.SaveBriefing(context.Background(), store.Briefing{
		ID: "b", PeriodKind: "daily", PeriodStart: start, PeriodEnd: start.AddDate(0, 0, 1),
		Status: store.BriefingReady, BodyMD: "Text.\n\n![x](http://example.com/p.png)\n",
	}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/briefing?kind=daily&period="+briefingPeriodDay(), nil))
	if !strings.Contains(rec.Body.String(), "Text.") {
		t.Fatalf("body text missing:\n%s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "<img") {
		t.Errorf("briefing body rendered an image:\n%s", rec.Body.String())
	}
}

func TestBriefingExportAllowsGeneratingRowWithBody(t *testing.T) {
	w := newTestWeb(t)
	start, _ := time.ParseInLocation("2006-01-02", briefingPeriodDay(), time.UTC)
	body := "# Prior\n\nKept body.\n"
	if _, err := w.db.SaveBriefing(context.Background(), store.Briefing{
		ID: "b", PeriodKind: "daily", PeriodStart: start, PeriodEnd: start.AddDate(0, 0, 1),
		Status: store.BriefingGenerating, BodyMD: body,
	}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/briefing/export?kind=daily&period="+briefingPeriodDay(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("export status = %d; body=%s", rec.Code, rec.Body)
	}
	if rec.Body.String() != body {
		t.Errorf("export body = %q, want the prior body", rec.Body.String())
	}
}

var _ briefing.Generator = (*blockingGenerator)(nil)

func seedCalBriefing(t *testing.T, w *Web, kind, day, status string) {
	t.Helper()
	start, err := time.ParseInLocation("2006-01-02", day, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	end := start.AddDate(0, 0, 1)
	if kind == "weekly" {
		end = start.AddDate(0, 0, 7)
	}
	if _, err := w.db.SaveBriefing(context.Background(), store.Briefing{
		ID: kind + "-" + day, PeriodKind: kind, PeriodStart: start, PeriodEnd: end,
		Status: status, BodyMD: "body",
	}); err != nil {
		t.Fatalf("SaveBriefing: %v", err)
	}
}

func getBody(t *testing.T, w *Web, target string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec.Code, rec.Body.String()
}

func TestBriefingCalendarDaily(t *testing.T) {
	w := newTestWeb(t)
	seedCalBriefing(t, w, "daily", "2026-03-10", store.BriefingReady)
	seedCalBriefing(t, w, "daily", "2026-03-11", store.BriefingFailed)
	seedCalBriefing(t, w, "weekly", "2026-03-09", store.BriefingReady)

	code, body := getBody(t, w, "/briefing/calendar?kind=daily&month=2026-03&period=2026-03-12")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	for _, want := range []string{
		"March 2026",
		`aria-label="10 March 2026, briefing available"`,
		`aria-label="11 March 2026"`,
		`href="/briefing?kind=daily&amp;period=2026-03-10"`,
		`hx-get="/briefing/calendar?kind=daily&month=2026-02&period=2026-03-12"`,
		`hx-get="/briefing/calendar?kind=daily&month=2026-04&period=2026-03-12"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	// Only the ready daily briefing is marked; the weekly one is another kind.
	if n := strings.Count(body, `class="cal__dot"`); n != 1 {
		t.Errorf("dots = %d, want 1", n)
	}
	// 12 March is the viewed period.
	if !strings.Contains(body, `is-sel" data-cal-cell href="/briefing?kind=daily&amp;period=2026-03-12"`) {
		t.Error("viewed day is not marked selected")
	}
	// Monday-first: the grid starts on 23 February, dimmed.
	first := strings.Index(body, `aria-label="23 February 2026"`)
	if first < 0 || !strings.Contains(body[max(0, first-120):first], "is-out") {
		t.Error("adjacent-month day missing or not dimmed")
	}
	if !strings.Contains(body[strings.Index(body, "cal__dow"):], "<span>Mo</span>") {
		t.Error("weekday header does not start Monday")
	}
}

func TestBriefingCalendarDisabledAndToday(t *testing.T) {
	w := newTestWeb(t)
	now := time.Now().UTC()
	_, body := getBody(t, w, "/briefing/calendar?kind=daily&month="+now.Format("2006-01"))
	label := now.Format("2 January 2006") + ", not finished yet"
	i := strings.Index(body, label)
	if i < 0 {
		t.Fatalf("today is not disabled: %q missing", label)
	}
	chunk := body[max(0, i-200):i]
	if !strings.Contains(chunk, `aria-disabled="true"`) || !strings.Contains(chunk, "is-today") || strings.Contains(chunk, "<a ") {
		t.Errorf("today cell = %q", chunk)
	}
	// No later month to browse to.
	if !strings.Contains(body, `id="briefingCalNext" aria-label="Next month" disabled`) {
		t.Error("next month is not disabled in the current month")
	}
}

func TestBriefingCalendarWeekly(t *testing.T) {
	w := newTestWeb(t)
	seedCalBriefing(t, w, "weekly", "2026-03-09", store.BriefingReady)
	seedCalBriefing(t, w, "weekly", "2026-03-16", store.BriefingFailed)

	code, body := getBody(t, w, "/briefing/calendar?kind=weekly&month=2026-03&period=2026-03-16")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	for _, want := range []string{
		`class="cal__week is-ready" data-cal-cell href="/briefing?kind=weekly&amp;period=2026-03-09"`,
		`aria-label="Week of 9 March 2026, briefing available"`,
		`href="/briefing?kind=weekly&amp;period=2026-02-23"`,
		`aria-label="Week of 16 March 2026"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if n := strings.Count(body, `class="cal__dot"`); n != 1 {
		t.Errorf("dots = %d, want 1", n)
	}
	if !strings.Contains(body, `is-sel" data-cal-cell href="/briefing?kind=weekly&amp;period=2026-03-16"`) {
		t.Error("viewed week is not selected")
	}
}

func TestBriefingCalendarBadInput(t *testing.T) {
	w := newTestWeb(t)
	for _, target := range []string{
		"/briefing/calendar?kind=monthly",
		"/briefing/calendar?kind=daily&month=2026-13",
		"/briefing/calendar?kind=daily&month=garbage",
		"/briefing/calendar?kind=daily&period=nope",
	} {
		if code, _ := getBody(t, w, target); code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", target, code)
		}
	}
}

func TestBriefingPageCalendarButton(t *testing.T) {
	w := newTestWeb(t)
	for kind, label := range map[string]string{"daily": "Pick a date", "weekly": "Pick a week"} {
		_, body := getBody(t, w, "/briefing?kind="+kind+"&period=2026-03-09")
		for _, want := range []string{
			`id="briefingCalBtn" aria-label="` + label + `"`,
			`aria-haspopup="dialog" aria-expanded="false" aria-controls="briefingCal"`,
			`id="briefingCal" role="dialog" aria-label="Choose a period" hidden`,
			`/static/js/briefing-cal.js`,
			`href="/briefing?kind=` + kind + `&amp;period=2026-03-09"`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s page missing %q", kind, want)
			}
		}
	}
}
