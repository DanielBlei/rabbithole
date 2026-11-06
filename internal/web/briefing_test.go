// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/briefing"
	"github.com/DanielBlei/rabbithole/internal/config"
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

func briefingToday() string { return time.Now().UTC().Format("2006-01-02") }

func seedScoredItem(t *testing.T, w *Web) {
	t.Helper()
	n, err := w.db.ReplaceItemScores(context.Background(), []store.ScoreReplacement{{
		ItemID: "a", Score: 9, Reason: "worth it", Model: "test",
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
	w.Routes().ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/briefing?kind=daily&period="+briefingToday(), nil))
	if page.Code != http.StatusOK {
		t.Fatalf("page status = %d", page.Code)
	}
	if !strings.Contains(page.Body.String(), "No briefing is stored") {
		t.Errorf("empty state missing:\n%s", page.Body.String())
	}
	if !strings.Contains(page.Body.String(), `href="/briefing"`) {
		t.Error("nav entry missing")
	}

	form := url.Values{"kind": {"daily"}, "period": {briefingToday()}}
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

	start, _ := time.ParseInLocation("2006-01-02", briefingToday(), time.UTC)
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
		ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/briefing?kind=daily&period="+briefingToday(), nil))
	body := ready.Body.String()
	for _, want := range []string{"One article mattered", "A development", "Download markdown", "Regenerate"} {
		if !strings.Contains(body, want) {
			t.Errorf("ready page missing %q", want)
		}
	}
	if strings.Contains(body, "<script>alert") {
		t.Error("model markdown was not sanitised")
	}
	if strings.Contains(body, "every 3s") {
		t.Error("ready page kept the status poll")
	}
}

func TestBriefingStatusTransitions(t *testing.T) {
	w := newTestWeb(t)
	start, _ := time.ParseInLocation("2006-01-02", briefingToday(), time.UTC)
	if _, err := w.db.SaveBriefing(context.Background(), store.Briefing{
		ID: "b", PeriodKind: "daily", PeriodStart: start, PeriodEnd: start.AddDate(0, 0, 1),
		Status: store.BriefingGenerating,
	}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/briefing/status?kind=daily&period="+briefingToday(), nil))
	if !strings.Contains(rec.Body.String(), "every 3s") || !strings.Contains(rec.Body.String(), "Generating") {
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
		"/briefing/status?kind=daily&period="+briefingToday(), nil))
	if strings.Contains(rec.Body.String(), "every 3s") {
		t.Errorf("ready status kept polling:\n%s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Ready") {
		t.Errorf("ready status missing body:\n%s", rec.Body.String())
	}
}

func TestBriefingExportDownloadsTheStoredBody(t *testing.T) {
	w := newTestWeb(t)
	start, _ := time.ParseInLocation("2006-01-02", briefingToday(), time.UTC)
	body := "# Briefing\n\nStored body.\n"
	if _, err := w.db.SaveBriefing(context.Background(), store.Briefing{
		ID: "b", PeriodKind: "daily", PeriodStart: start, PeriodEnd: start.AddDate(0, 0, 1),
		Status: store.BriefingReady, BodyMD: body,
	}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/briefing/export?kind=daily&period="+briefingToday(), nil))
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

	form := url.Values{"kind": {"daily"}, "period": {briefingToday()}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/briefing/generate", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("generate status = %d; body=%s", rec.Code, rec.Body)
	}

	start, _ := time.ParseInLocation("2006-01-02", briefingToday(), time.UTC)
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
	w.Routes().ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/briefing?kind=daily&period="+briefingToday(), nil))
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
					tc.period.Next().Start.Format("2006-01-02") + `">Next &rarr;</a>`
				disabled := `aria-disabled="true">Next &rarr;</span>`
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

	form := url.Values{"kind": {"daily"}, "period": {briefingToday()}}
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
	start, _ := time.ParseInLocation("2006-01-02", briefingToday(), time.UTC)
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

var _ briefing.Generator = (*blockingGenerator)(nil)
