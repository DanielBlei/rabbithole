// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/articlefetch"
	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/decisionbrief"
	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/store"
)

const briefJSON = `{"score":8,"recommendation":"read","why":"It matches your interest in databases.","key_points":["A new benchmark","A clear method"],"novelty":"It reports a new benchmark.","caveats":"Single workload.","focus":"Look at the benchmark setup."}`

type briefGen func(context.Context, string, string, json.RawMessage, int) (string, error)

func (f briefGen) ChatJSON(
	ctx context.Context,
	system, user string,
	schema json.RawMessage,
	maxTokens int,
) (string, error) {
	return f(ctx, system, user, schema, maxTokens)
}

func briefArticle() string {
	var b strings.Builder
	b.WriteString("<html><body><article><h1>Deep article</h1><p>")
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&b, "This sentence has enough words to look like real article prose number %d. ", i)
	}
	b.WriteString("</p></article></body></html>")
	return b.String()
}

func briefFetcher(calls *int) decisionbrief.Fetcher {
	return func(_ context.Context, rawURL string, _ articlefetch.Limits) (articlefetch.Result, error) {
		*calls++
		return articlefetch.Result{
			Body:        io.NopCloser(strings.NewReader(briefArticle())),
			ContentType: "text/html; charset=utf-8",
			FinalURL:    rawURL,
		}, nil
	}
}

func newBriefWeb(t *testing.T, gen decisionbrief.GeneratorFunc, fetch decisionbrief.Fetcher) *Web {
	t.Helper()
	w := newTestWeb(t)
	ctx := context.Background()
	it := feeds.Item{ID: "a", Source: "S1", Title: "A", Link: "https://x/a"}
	if err := w.db.Record(ctx, []feeds.Item{it},
		[]store.DigestEntry{{Item: it, Score: 4, Reason: "feed summary", Model: "feed-model"}},
		time.Now()); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if gen != nil {
		w.SetBriefGenerator(gen)
	}
	if fetch != nil {
		w.SetBriefFetcher(fetch)
	}
	return w
}

func briefStatusBody(t *testing.T, w *Web, id string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/items/"+id+"/brief/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status poll: %d; body=%s", rec.Code, rec.Body)
	}
	return rec.Body.String()
}

func waitBriefBody(t *testing.T, w *Web, id string, contains ...string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		body := briefStatusBody(t, w, id)
		ok := true
		for _, want := range contains {
			if !strings.Contains(body, want) {
				ok = false
				break
			}
		}
		if ok {
			return body
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("brief %s did not reach %v; last=%s", id, contains, briefStatusBody(t, w, id))
	return ""
}

func TestBriefEndToEndReady(t *testing.T) {
	var fetchCalls, genCalls int
	w := newBriefWeb(t, func(context.Context) (decisionbrief.Generator, decisionbrief.Provenance, error) {
		return briefGen(func(context.Context, string, string, json.RawMessage, int) (string, error) {
			genCalls++
			return briefJSON, nil
		}), decisionbrief.Provenance{}, nil
	}, briefFetcher(&fetchCalls))

	rec := post(t, w, "/items/a/brief")
	if !strings.Contains(rec.Body.String(), "Queued") && !strings.Contains(rec.Body.String(), "Fetching") {
		t.Errorf("start panel = %s", rec.Body)
	}
	body := waitBriefBody(t, w, "a", "Full-text 8/10")
	for _, want := range []string{
		"Feed score 4/10", "Full-text 8/10", "min read", "Read in full",
		"WHY THIS MATTERS TO YOU", "KEY IDEAS", "A new benchmark",
		"WHAT IS ACTUALLY NEW", "LIMITATIONS / CAVEATS", "FOCUS ON",
		"A feed score was recorded", "computed from the fetched article",
		"Fetched from https://x/a",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("ready panel missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "RSS") {
		t.Errorf("ready panel invents RSS provenance: %s", body)
	}
	if strings.Contains(body, `hx-trigger="every 2s`) {
		t.Errorf("terminal panel still polls: %s", body)
	}
	if fetchCalls != 1 || genCalls != 1 {
		t.Errorf("fetch calls = %d, gen calls = %d; want 1 each", fetchCalls, genCalls)
	}
	row, err := w.db.Get(context.Background(), "a")
	if err != nil || row.LLMScore == nil || *row.LLMScore != 4 {
		t.Errorf("feed score changed after a dive: %+v, %v", row.LLMScore, err)
	}

	// Re-opening a stored brief serves it without another fetch or call.
	rec = post(t, w, "/items/a/brief")
	if !strings.Contains(rec.Body.String(), "Full-text 8/10") {
		t.Errorf("stored brief not served: %s", rec.Body)
	}
	if fetchCalls != 1 || genCalls != 1 {
		t.Errorf("re-open fetched/ran again: fetch=%d gen=%d", fetchCalls, genCalls)
	}

	// A page reload renders the stored state without JS.
	page := httptest.NewRecorder()
	w.Routes().ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/feed", nil))
	if !strings.Contains(page.Body.String(), "Full-text 8/10") {
		t.Errorf("feed page does not render the stored brief")
	}
}

func TestBriefRunningPollsThenTerminates(t *testing.T) {
	release := make(chan struct{})
	w := newBriefWeb(t, func(context.Context) (decisionbrief.Generator, decisionbrief.Provenance, error) {
		return briefGen(func(context.Context, string, string, json.RawMessage, int) (string, error) {
			<-release
			return briefJSON, nil
		}), decisionbrief.Provenance{}, nil
	}, briefFetcher(new(int)))
	rec := post(t, w, "/items/a/brief")
	if !strings.Contains(rec.Body.String(), `hx-trigger="every 2s`) {
		t.Errorf("running panel does not poll: %s", rec.Body)
	}
	close(release)
	body := waitBriefBody(t, w, "a", "Full-text 8/10")
	if strings.Contains(body, `hx-trigger="every 2s`) {
		t.Errorf("ready panel still polls: %s", body)
	}
}

func TestBriefDoublePostSingleGeneration(t *testing.T) {
	var genCalls int
	var mu sync.Mutex
	release := make(chan struct{})
	w := newBriefWeb(t, func(context.Context) (decisionbrief.Generator, decisionbrief.Provenance, error) {
		return briefGen(func(context.Context, string, string, json.RawMessage, int) (string, error) {
			mu.Lock()
			genCalls++
			mu.Unlock()
			<-release
			return briefJSON, nil
		}), decisionbrief.Provenance{}, nil
	}, briefFetcher(new(int)))
	post(t, w, "/items/a/brief")
	post(t, w, "/items/a/brief")
	close(release)
	waitBriefBody(t, w, "a", "Full-text 8/10")
	mu.Lock()
	defer mu.Unlock()
	if genCalls != 1 {
		t.Fatalf("model calls = %d, want 1", genCalls)
	}
}

func TestBriefRefreshReplacesStoredBrief(t *testing.T) {
	var genCalls int
	w := newBriefWeb(t, func(context.Context) (decisionbrief.Generator, decisionbrief.Provenance, error) {
		return briefGen(func(context.Context, string, string, json.RawMessage, int) (string, error) {
			genCalls++
			if genCalls == 1 {
				return briefJSON, nil
			}
			return strings.Replace(briefJSON, "A new benchmark", "A changed benchmark", 1), nil
		}), decisionbrief.Provenance{}, nil
	}, briefFetcher(new(int)))
	post(t, w, "/items/a/brief")
	waitBriefBody(t, w, "a", "Full-text 8/10")
	rec := post(t, w, "/items/a/brief/refresh")
	_ = rec
	body := waitBriefBody(t, w, "a", "A changed benchmark")
	if strings.Contains(body, "A new benchmark") {
		t.Errorf("refresh did not replace the brief: %s", body)
	}
}

func TestBriefFailureStatesRenderMessages(t *testing.T) {
	w := newBriefWeb(t, nil, nil)
	ctx := context.Background()
	codes := []string{
		decisionbrief.CodeNoLink, decisionbrief.CodeFetchBlocked,
		decisionbrief.CodeTooManyRedirects, decisionbrief.CodeTimeout,
		decisionbrief.CodeTooLarge, decisionbrief.CodeUnsupportedContentType,
		decisionbrief.CodeEncoding, decisionbrief.CodeInsufficientContent,
		decisionbrief.CodeProviderUnavailable, decisionbrief.CodeAnalysisFailed,
		decisionbrief.CodeInvalidOutput, decisionbrief.CodeStorageFailed,
	}
	for i, code := range codes {
		id := fmt.Sprintf("fail%d", i)
		if err := w.db.Record(
			ctx,
			[]feeds.Item{{ID: id, Source: "S", Title: id, Link: "https://x/" + id}},
			nil,
			time.Now(),
		); err != nil {
			t.Fatalf("Record %s: %v", id, err)
		}
		if err := w.db.SaveItemBrief(ctx, store.ItemBrief{
			ItemID: id, Status: store.ItemBriefFailed, ErrorCode: code,
		}); err != nil {
			t.Fatalf("Save failed brief: %v", err)
		}
		body := briefStatusBody(t, w, id)
		message := html.EscapeString(decisionbrief.ProductMessage(code))
		if !strings.Contains(body, message) {
			t.Errorf("%s panel missing message %q:\n%s", code, message, body)
		}
		if !strings.Contains(body, "[ retry ]") {
			t.Errorf("%s panel missing retry: %s", code, body)
		}
		if strings.Contains(body, `hx-trigger="every 2s`) {
			t.Errorf("%s panel still polls", code)
		}
	}
}

// TestStorageFailureHidesDatabaseDetail pins the user-facing contract: a stored
// storage failure renders the product message and never the raw database error,
// even if an old or externally written row carries detail.
func TestStorageFailureHidesDatabaseDetail(t *testing.T) {
	w := newBriefWeb(t, nil, nil)
	ctx := context.Background()
	if err := w.db.Record(
		ctx,
		[]feeds.Item{{ID: "storage", Source: "S", Title: "storage", Link: "https://x/storage"}},
		nil,
		time.Now(),
	); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := w.db.SaveItemBrief(ctx, store.ItemBrief{
		ItemID:      "storage",
		Status:      store.ItemBriefFailed,
		ErrorCode:   decisionbrief.CodeStorageFailed,
		ErrorDetail: "pq: password authentication failed for user rabbit",
	}); err != nil {
		t.Fatalf("Save failed brief: %v", err)
	}

	body := briefStatusBody(t, w, "storage")
	message := html.EscapeString(decisionbrief.ProductMessage(decisionbrief.CodeStorageFailed))
	if !strings.Contains(body, message) {
		t.Errorf("storage panel missing message %q:\n%s", message, body)
	}
	if strings.Contains(body, "password authentication") || strings.Contains(body, "pq:") {
		t.Errorf("storage panel exposed a database error:\n%s", body)
	}
	if !strings.Contains(body, "[ retry ]") {
		t.Errorf("storage panel missing retry: %s", body)
	}
}

func TestBriefHeuristicUnavailable(t *testing.T) {
	w := newBriefWeb(t, func(context.Context) (decisionbrief.Generator, decisionbrief.Provenance, error) {
		return nil, decisionbrief.Provenance{}, decisionbrief.ErrBriefUnavailable
	}, briefFetcher(new(int)))
	post(t, w, "/items/a/brief")
	body := waitBriefBody(t, w, "a", decisionbrief.ProductMessage(decisionbrief.CodeProviderUnavailable))
	if !strings.Contains(body, "heuristic") {
		t.Errorf("provider message = %s", body)
	}
}

func TestBriefFetchFailurePersistedAsFailed(t *testing.T) {
	fetch := func(_ context.Context, _ string, _ articlefetch.Limits) (articlefetch.Result, error) {
		return articlefetch.Result{}, articlefetch.ErrBlocked
	}
	w := newBriefWeb(t, func(context.Context) (decisionbrief.Generator, decisionbrief.Provenance, error) {
		return briefGen(func(context.Context, string, string, json.RawMessage, int) (string, error) {
			t.Errorf("model called after a fetch failure")
			return briefJSON, nil
		}), decisionbrief.Provenance{}, nil
	}, fetch)
	post(t, w, "/items/a/brief")
	waitBriefBody(t, w, "a", decisionbrief.ProductMessage(decisionbrief.CodeFetchBlocked))
	stored, err := w.db.ItemBrief(context.Background(), "a")
	if err != nil || stored.Status != store.ItemBriefFailed || stored.Score != nil {
		t.Fatalf("stored = %+v, %v", stored, err)
	}
}

func TestBriefEscapesModelOutput(t *testing.T) {
	malicious := strings.Replace(briefJSON, "It matches your interest in databases.",
		"<script>alert(1)</script> It matches your interest.", 1)
	w := newBriefWeb(t, func(context.Context) (decisionbrief.Generator, decisionbrief.Provenance, error) {
		return briefGen(func(context.Context, string, string, json.RawMessage, int) (string, error) {
			return malicious, nil
		}), decisionbrief.Provenance{}, nil
	}, briefFetcher(new(int)))
	post(t, w, "/items/a/brief")
	body := waitBriefBody(t, w, "a", "Full-text 8/10")
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Errorf("model HTML was not escaped: %s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Errorf("escaped output missing: %s", body)
	}
}

// Additional D: an older or externally written stored row may still carry
// userinfo, but the panel must never render it.
func TestBriefDataFromSanitizesFinalURL(t *testing.T) {
	d := briefDataFrom("a", store.ItemBrief{
		FinalURL: "https://storeduser:storedsecret@example.com/article",
	})
	if d.FinalURL != "https://example.com/article" {
		t.Fatalf("FinalURL = %q, want credentials removed", d.FinalURL)
	}
	if strings.Contains(d.FinalURL, "storedsecret") || strings.Contains(d.FinalURL, "storeduser") {
		t.Fatalf("panel data leaked credentials: %q", d.FinalURL)
	}
}

// Additional E: a full generation queue is a retryable 429 with a fixed
// message, not a generic 500.
func TestBriefStartErrorMapsBusyTo429(t *testing.T) {
	rec := httptest.NewRecorder()
	httpBriefStartError(rec, decisionbrief.ErrBriefBusy, "a", "brief_start")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want 1", got)
	}
	if body := strings.TrimSpace(rec.Body.String()); body != briefBusyMessage {
		t.Errorf("body = %q, want %q", body, briefBusyMessage)
	}
}

func TestBriefUnknownItem(t *testing.T) {
	w := newBriefWeb(t, nil, nil)
	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodPost, "/items/missing/brief", nil),
		httptest.NewRequest(http.MethodPost, "/items/missing/brief/refresh", nil),
		httptest.NewRequest(http.MethodGet, "/items/missing/brief/status", nil),
	} {
		rec := httptest.NewRecorder()
		w.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404", req.Method, req.URL.Path, rec.Code)
		}
	}
}

func TestBriefTruncatedIndicator(t *testing.T) {
	w := newBriefWeb(t, func(context.Context) (decisionbrief.Generator, decisionbrief.Provenance, error) {
		return briefGen(func(context.Context, string, string, json.RawMessage, int) (string, error) {
			return briefJSON, nil
		}), decisionbrief.Provenance{}, nil
	}, briefFetcher(new(int)))
	w.src = config.Static(config.Config{DecisionBrief: config.DecisionBriefConfig{MaxTextBytes: 800}})
	post(t, w, "/items/a/brief")
	body := waitBriefBody(t, w, "a", "Full-text 8/10")
	if !strings.Contains(body, "Part of the article was analyzed") {
		t.Errorf("truncation indicator missing: %s", body)
	}
}

func TestBriefFailedRefreshKeepsPreviousPanel(t *testing.T) {
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
	if !strings.Contains(body, "Last successful brief") || !strings.Contains(body, "Full-text 8/10") {
		t.Errorf("previous brief not offered: %s", body)
	}
	if !strings.Contains(body, "It matches your interest in databases.") {
		t.Errorf("previous brief content hidden: %s", body)
	}
	if strings.Contains(body, "RSS") {
		t.Errorf("previous brief invents RSS provenance: %s", body)
	}
}
