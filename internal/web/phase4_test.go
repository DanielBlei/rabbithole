// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/decisionbrief"
	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/store"
)

func recordPhase4Item(t *testing.T, w *Web, id string) {
	t.Helper()
	it := feeds.Item{ID: id, Source: "S", Title: id, Link: "https://x/phase4/" + id}
	if err := w.db.Record(context.Background(), []feeds.Item{it}, nil, time.Now()); err != nil {
		t.Fatalf("Record %q: %v", id, err)
	}
}

func savePhase4Brief(t *testing.T, w *Web, b store.ItemBrief) {
	t.Helper()
	if b.Status == "" {
		b.Status = store.ItemBriefReady
	}
	if b.CreatedAt.IsZero() {
		b.CreatedAt = time.Now().UTC()
	}
	if err := w.db.SaveItemBrief(context.Background(), b); err != nil {
		t.Fatalf("SaveItemBrief %q: %v", b.ItemID, err)
	}
}

func feedPage(t *testing.T, w *Web) string {
	t.Helper()
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/feed", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /feed = %d; body=%s", rec.Code, rec.Body)
	}
	return rec.Body.String()
}

// Finding #15: a refresh keeps the prior ready brief visible under a
// non-disruptive status line, without offering a second refresh.
func TestBriefRefreshKeepsPreviousWhileRunning(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	releaseGen := func() { once.Do(func() { close(release) }) }
	t.Cleanup(releaseGen)

	var mu sync.Mutex
	calls := 0
	w := newBriefWeb(t, func(context.Context) (decisionbrief.Generator, decisionbrief.Provenance, error) {
		return briefGen(func(context.Context, string, string, json.RawMessage, int) (string, error) {
			mu.Lock()
			calls++
			n := calls
			mu.Unlock()
			if n == 1 {
				return briefJSON, nil
			}
			<-release
			return strings.Replace(briefJSON, "A new benchmark", "A changed benchmark", 1), nil
		}), decisionbrief.Provenance{}, nil
	}, briefFetcher(new(int)))

	post(t, w, "/items/a/brief")
	waitBriefBody(t, w, "a", "Full-text 8/10")

	body := post(t, w, "/items/a/brief/refresh").Body.String()
	for _, want := range []string{"Full-text 8/10", "A new benchmark", "showing the last brief"} {
		if !strings.Contains(body, want) {
			t.Errorf("refreshing panel missing %q:\n%s", want, body)
		}
	}
	if !strings.Contains(body, `hx-trigger="every 2s`) {
		t.Errorf("refreshing panel does not poll: %s", body)
	}
	if strings.Contains(body, `hx-post="/items/a/brief/refresh"`) {
		t.Errorf("refreshing panel still offers refresh: %s", body)
	}

	releaseGen()
	updated := waitBriefBody(t, w, "a", "A changed benchmark")
	if strings.Contains(updated, "A new benchmark") {
		t.Errorf("old brief survived a successful refresh: %s", updated)
	}
	if strings.Contains(updated, `hx-trigger="every 2s`) {
		t.Errorf("updated panel still polls: %s", updated)
	}
}

// Finding #15: polling during refresh repeatedly shows the old brief.
func TestBriefRepeatedPollingDuringRefresh(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	releaseGen := func() { once.Do(func() { close(release) }) }
	t.Cleanup(releaseGen)

	var mu sync.Mutex
	calls := 0
	w := newBriefWeb(t, func(context.Context) (decisionbrief.Generator, decisionbrief.Provenance, error) {
		return briefGen(func(context.Context, string, string, json.RawMessage, int) (string, error) {
			mu.Lock()
			calls++
			n := calls
			mu.Unlock()
			if n == 1 {
				return briefJSON, nil
			}
			<-release
			return strings.Replace(briefJSON, "A new benchmark", "A changed benchmark", 1), nil
		}), decisionbrief.Provenance{}, nil
	}, briefFetcher(new(int)))

	post(t, w, "/items/a/brief")
	waitBriefBody(t, w, "a", "Full-text 8/10")
	post(t, w, "/items/a/brief/refresh")

	for i := 0; i < 3; i++ {
		body := briefStatusBody(t, w, "a")
		for _, want := range []string{"Full-text 8/10", "showing the last brief"} {
			if !strings.Contains(body, want) {
				t.Errorf("poll %d missing %q:\n%s", i, want, body)
			}
		}
		if strings.Contains(body, `hx-post="/items/a/brief/refresh"`) {
			t.Errorf("poll %d offers a duplicate refresh: %s", i, body)
		}
	}
	releaseGen()
	waitBriefBody(t, w, "a", "A changed benchmark")
}

// Finding #15: a failed refresh keeps the previous brief visible with a clear
// failure/retry notice.
func TestBriefRefreshFailureShowsPreviousBrief(t *testing.T) {
	var calls int
	w := newBriefWeb(t, func(context.Context) (decisionbrief.Generator, decisionbrief.Provenance, error) {
		return briefGen(func(context.Context, string, string, json.RawMessage, int) (string, error) {
			calls++
			if calls == 1 {
				return briefJSON, nil
			}
			return "", errors.New("provider down")
		}), decisionbrief.Provenance{}, nil
	}, briefFetcher(new(int)))

	post(t, w, "/items/a/brief")
	waitBriefBody(t, w, "a", "Full-text 8/10")
	post(t, w, "/items/a/brief/refresh")

	body := waitBriefBody(t, w, "a", decisionbrief.ProductMessage(decisionbrief.CodeAnalysisFailed))
	for _, want := range []string{
		"Last successful brief",
		"It matches your interest in databases.",
		"Full-text 8/10",
		"[ retry ]",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("failed refresh panel missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "RSS") {
		t.Errorf("failed refresh panel still invents RSS provenance: %s", body)
	}
}

// Finding #15: an initial generation has no previous brief to show.
func TestBriefInitialRunHasNoPreviousBrief(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	releaseGen := func() { once.Do(func() { close(release) }) }
	t.Cleanup(releaseGen)

	w := newBriefWeb(t, func(context.Context) (decisionbrief.Generator, decisionbrief.Provenance, error) {
		return briefGen(func(context.Context, string, string, json.RawMessage, int) (string, error) {
			<-release
			return briefJSON, nil
		}), decisionbrief.Provenance{}, nil
	}, briefFetcher(new(int)))
	body := post(t, w, "/items/a/brief").Body.String()
	if strings.Contains(body, "showing the last brief") ||
		strings.Contains(body, "Full-text 8/10") {
		t.Errorf("initial panel pretends a previous brief exists: %s", body)
	}
	if !strings.Contains(body, `hx-trigger="every 2s`) {
		t.Errorf("initial panel does not poll: %s", body)
	}
	releaseGen()
	waitBriefBody(t, w, "a", "Full-text 8/10")
}

// Finding #15: after a service restart the persisted ready brief is still the
// previous brief during a refresh.
func TestBriefRestartShowsPreviousWhileRefreshing(t *testing.T) {
	w := newBriefWeb(t, func(context.Context) (decisionbrief.Generator, decisionbrief.Provenance, error) {
		return briefGen(func(context.Context, string, string, json.RawMessage, int) (string, error) {
			return briefJSON, nil
		}), decisionbrief.Provenance{}, nil
	}, briefFetcher(new(int)))
	post(t, w, "/items/a/brief")
	waitBriefBody(t, w, "a", "Full-text 8/10")

	// A fresh Service has no in-memory runs or failures; it must read the
	// previous ready brief from the store.
	release := make(chan struct{})
	var once sync.Once
	releaseGen := func() { once.Do(func() { close(release) }) }
	t.Cleanup(releaseGen)
	w.briefs = decisionbrief.New(w.db, w.profiles,
		func(context.Context) (decisionbrief.Generator, decisionbrief.Provenance, error) {
			return briefGen(func(context.Context, string, string, json.RawMessage, int) (string, error) {
				<-release
				return strings.Replace(briefJSON, "A new benchmark", "A changed benchmark", 1), nil
			}), decisionbrief.Provenance{}, nil
		})
	w.SetBriefFetcher(briefFetcher(new(int)))

	body := post(t, w, "/items/a/brief/refresh").Body.String()
	if !strings.Contains(body, "Full-text 8/10") || !strings.Contains(body, "showing the last brief") {
		t.Fatalf("restarted service lost the previous brief: %s", body)
	}
	releaseGen()
	waitBriefBody(t, w, "a", "A changed benchmark")
}

// Finding #17: wording is conditional on available provenance and never
// invents an RSS source.
func TestBriefScoreProvenanceLabels(t *testing.T) {
	w := newBriefWeb(t, nil, nil)
	score, feed := 8, 4

	savePhase4Brief(t, w, store.ItemBrief{
		ItemID: "a", Status: store.ItemBriefReady, Score: &score, FeedScore: &feed,
		Recommendation: "read", Why: "because", KeyPoints: []string{"one"},
		Provider: "ollama", Model: "m",
	})
	body := briefStatusBody(t, w, "a")
	for _, want := range []string{
		"Feed score 4/10", "Full-text 8/10",
		"A feed score was recorded", "computed from the fetched article",
		"ollama/m",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("feed+full panel missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "RSS") {
		t.Errorf("feed+full panel invents RSS provenance: %s", body)
	}

	recordPhase4Item(t, w, "fullonly")
	savePhase4Brief(t, w, store.ItemBrief{
		ItemID: "fullonly", Status: store.ItemBriefReady, Score: &score,
		Recommendation: "read", Why: "because",
	})
	body = briefStatusBody(t, w, "fullonly")
	if strings.Contains(body, "Feed score") {
		t.Errorf("full-only panel invents a feed score: %s", body)
	}
	for _, want := range []string{"Full-text 8/10", "computed from the fetched article"} {
		if !strings.Contains(body, want) {
			t.Errorf("full-only panel missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "RSS") {
		t.Errorf("full-only panel invents RSS provenance: %s", body)
	}

	recordPhase4Item(t, w, "feedonly")
	savePhase4Brief(t, w, store.ItemBrief{
		ItemID: "feedonly", Status: store.ItemBriefReady, FeedScore: &feed,
		Recommendation: "read", Why: "because",
	})
	body = briefStatusBody(t, w, "feedonly")
	for _, want := range []string{"Feed score 4/10", "no full-text score is available"} {
		if !strings.Contains(body, want) {
			t.Errorf("feed-only panel missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "Full-text 0/10") {
		t.Errorf("feed-only panel invents a full-text score: %s", body)
	}

	recordPhase4Item(t, w, "unknown")
	savePhase4Brief(t, w, store.ItemBrief{
		ItemID: "unknown", Status: store.ItemBriefReady, Recommendation: "read", Why: "because",
	})
	body = briefStatusBody(t, w, "unknown")
	for _, want := range []string{"No score recorded", "No score provenance was recorded"} {
		if !strings.Contains(body, want) {
			t.Errorf("unknown panel missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "Feed score") || strings.Contains(body, "RSS") {
		t.Errorf("unknown panel invents provenance: %s", body)
	}
}

type statusErrorStore struct {
	*store.Store
	err  error
	omit map[string]bool
}

func (s *statusErrorStore) ItemBriefsByItemIDs(
	ctx context.Context, ids []string,
) (map[string]store.ItemBrief, error) {
	if s.err != nil {
		return nil, s.err
	}
	out, err := s.Store.ItemBriefsByItemIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	for id := range s.omit {
		delete(out, id)
	}
	return out, nil
}

func (s *statusErrorStore) ItemBrief(ctx context.Context, id string) (store.ItemBrief, error) {
	if s.err != nil {
		return store.ItemBrief{}, s.err
	}
	return s.Store.ItemBrief(ctx, id)
}

// Finding #19: a failed batch status read must not turn items into empty-ID
// actions.
func TestBriefBatchStatusErrorHasNoEmptyActions(t *testing.T) {
	w := newBriefWeb(t, nil, nil)
	w.briefs = decisionbrief.New(
		&statusErrorStore{Store: w.db, err: errors.New("database down")}, w.profiles, nil)

	body := feedPage(t, w)
	if strings.Contains(body, "/items//brief") {
		t.Errorf("feed page rendered an empty item ID:\n%s", body)
	}
	if strings.Contains(body, `hx-post="/items/a/brief"`) {
		t.Errorf("unavailable panel still offers an action: %s", body)
	}
	if !strings.Contains(body, "Decision Brief status is temporarily unavailable.") {
		t.Errorf("failed batch read was rendered as an absent state:\n%s", body)
	}

	rd := w.toRow(context.Background(), store.ItemRow{ID: "a"})
	if rd.Brief.ID != "a" || rd.Brief.Status == "idle" {
		t.Errorf("single-row failure used a misleading state: %+v", rd.Brief)
	}
}

// Finding #19: item IDs requiring URL escaping are escaped in HTMX endpoints.
func TestBriefURLEscapesItemID(t *testing.T) {
	w := newBriefWeb(t, nil, nil)
	const weird = "a b/c?"
	recordPhase4Item(t, w, weird)
	score := 8
	const readyWeird = "d e/f?"
	recordPhase4Item(t, w, readyWeird)
	savePhase4Brief(t, w, store.ItemBrief{
		ItemID: readyWeird, Status: store.ItemBriefReady, Score: &score,
		Recommendation: "read", Why: "because",
	})

	body := feedPage(t, w)
	for _, want := range []string{
		`hx-post="/items/a%20b%2Fc%3F/brief"`,
		`hx-post="/items/d%20e%2Ff%3F/brief/refresh"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("endpoint not URL-escaped, missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `/items/a b/c?/brief`) {
		t.Errorf("raw item ID leaked into a URL: %s", body)
	}
}

// Finding #19: independent items keep their own valid actions.
func TestBriefMultipleItemsKeepOwnActions(t *testing.T) {
	w := newBriefWeb(t, nil, nil)
	recordPhase4Item(t, w, "b")
	recordPhase4Item(t, w, "c")
	score, feed := 8, 4
	savePhase4Brief(t, w, store.ItemBrief{
		ItemID: "a", Status: store.ItemBriefReady, Score: &score, FeedScore: &feed,
		Recommendation: "read", Why: "because",
	})
	savePhase4Brief(t, w, store.ItemBrief{
		ItemID: "b", Status: store.ItemBriefFailed,
		ErrorCode: decisionbrief.CodeAnalysisFailed,
	})

	body := feedPage(t, w)
	for _, want := range []string{
		`id="brief-a"`, `hx-post="/items/a/brief/refresh"`,
		`id="brief-b"`, `hx-post="/items/b/brief"`,
		`id="brief-c"`, `hx-post="/items/c/brief"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("feed page missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "/items//brief") {
		t.Errorf("feed page rendered an empty item ID: %s", body)
	}
}

// Finding #16: reduced motion disables the brief pulse while keeping the
// loading status text and its accessible label.
func TestBriefReducedMotionAndStatusLabels(t *testing.T) {
	css, err := staticFS.ReadFile("static/style.css")
	if err != nil {
		t.Fatalf("read style.css: %v", err)
	}
	rule := regexp.MustCompile(
		`(?s)@media \(prefers-reduced-motion: ?reduce\)\{[^}]*\.brief__dot\{animation:none`)
	if !rule.Match(css) {
		t.Error("brief dot pulse is not disabled under prefers-reduced-motion")
	}

	tmpl, err := templatesFS.ReadFile("templates/partials/brief.html")
	if err != nil {
		t.Fatalf("read brief.html: %v", err)
	}
	for _, want := range []string{`role="status"`, `{{.PhaseText}}`, `aria-hidden="true"`} {
		if !strings.Contains(string(tmpl), want) {
			t.Errorf("loading status lost %q", want)
		}
	}
}

// Finding #19: a batch response missing an item is unavailable for that item,
// while the items it did return keep their state.
func TestBriefPartialBatchStatusIsUnavailable(t *testing.T) {
	score := 8
	ready := store.ItemBrief{
		ItemID: "a", Status: store.ItemBriefReady, Score: &score,
		Recommendation: "read", Why: "because",
	}
	rows := []rowData{{ID: "a"}, {ID: "b"}}
	applyBriefViews(rows, map[string]decisionbrief.Snapshot{
		"a": {ItemID: "a", Status: decisionbrief.StatusReady, Brief: ready},
	}, nil)
	if rows[0].Brief.Status != decisionbrief.StatusReady {
		t.Errorf("returned item a = %+v, want ready", rows[0].Brief)
	}
	if rows[1].Brief.ID != "b" || rows[1].Brief.Status != briefStatusUnavailable {
		t.Errorf("missing item b = %+v, want unavailable for b", rows[1].Brief)
	}
}

// Finding #19: empty or malformed status records are never treated as an idle
// item with actions.
func TestBriefMalformedStatusRecordsAreUnavailable(t *testing.T) {
	score := 8
	readyBrief := store.ItemBrief{
		ItemID: "a", Status: store.ItemBriefReady, Score: &score,
		Recommendation: "read", Why: "because",
	}
	valid := decisionbrief.Snapshot{
		ItemID: "a", Status: decisionbrief.StatusReady, Brief: readyBrief,
	}
	if v := briefViewFor("a", valid, true); v.Status != decisionbrief.StatusReady ||
		v.Current == nil || v.URLID != "a" {
		t.Fatalf("valid snapshot = %+v, want ready/a", v)
	}

	cases := []struct {
		name string
		snap decisionbrief.Snapshot
		ok   bool
	}{
		{"missing", decisionbrief.Snapshot{}, false},
		{"zero snapshot", decisionbrief.Snapshot{}, true},
		{"other item", decisionbrief.Snapshot{ItemID: "b", Status: decisionbrief.StatusIdle}, true},
		{"empty status", decisionbrief.Snapshot{ItemID: "a"}, true},
		{"unknown status", decisionbrief.Snapshot{ItemID: "a", Status: "bogus"}, true},
		{"mismatched brief", decisionbrief.Snapshot{
			ItemID: "a", Status: decisionbrief.StatusReady,
			Brief: store.ItemBrief{ItemID: "b", Status: store.ItemBriefReady},
		}, true},
	}
	for _, tc := range cases {
		v := briefViewFor("a", tc.snap, tc.ok)
		if v.Status != briefStatusUnavailable || v.ID != "a" {
			t.Errorf("%s = %+v, want unavailable for a", tc.name, v)
		}
	}

	empty := briefViewFor("", decisionbrief.Snapshot{}, false)
	if empty.Status != briefStatusUnavailable {
		t.Errorf("empty item id = %+v, want unavailable", empty)
	}
}

// Finding #19: an escaped path segment still resolves to the same item.
func TestBriefEscapedSpaceEndpointTargetsItem(t *testing.T) {
	w := newBriefWeb(t, nil, nil)
	const id = "a b"
	recordPhase4Item(t, w, id)

	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/items/a%20b/brief/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("escaped status endpoint = %d; body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `id="brief-a%20b"`) {
		t.Errorf("escaped endpoint rendered the wrong item: %s", body)
	}
	if strings.Contains(body, "/items//brief") {
		t.Errorf("escaped endpoint rendered an empty id: %s", body)
	}
}

// Finding #19: a status read failure returns a safe error rather than echoing
// database details into the polling endpoint.
func TestBriefStatusErrorDoesNotLeakDetail(t *testing.T) {
	w := newBriefWeb(t, nil, nil)
	w.briefs = decisionbrief.New(
		&statusErrorStore{Store: w.db, err: errors.New("pq: password authentication failed")},
		w.profiles, nil)

	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/items/a/brief/status", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status error = %d, want 500", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != briefStatusFailureMessage {
		t.Errorf("status error body = %q, want the fixed status message", got)
	}
	if body := rec.Body.String(); strings.Contains(body, "password") || strings.Contains(body, "pq:") {
		t.Errorf("status error leaked database detail: %s", body)
	}
}
