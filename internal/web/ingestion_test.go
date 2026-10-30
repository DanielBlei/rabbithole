// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/httplog"
	"github.com/DanielBlei/rabbithole/internal/library"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// ingestionWeb is a test Web with no library fetch, so a page render never
// waits on the network.
func ingestionWeb(t *testing.T) *Web {
	t.Helper()
	w := newTestWeb(t)
	w.SetLibrary(library.NewLoader(library.Options{BaseURL: "off"}))
	return w
}

// seedIngestionRun records one run in the given state and returns its id. A
// running status leaves the row unfinished.
func seedIngestionRun(t *testing.T, w *Web, trigger, status string, counts store.IngestCounts, errMsg string) int64 {
	t.Helper()
	id, err := w.db.StartIngestRun(t.Context(), trigger)
	if err != nil {
		t.Fatalf("StartIngestRun: %v", err)
	}
	if status != store.IngestStatusRunning {
		if err := w.db.FinishIngestRun(t.Context(), id, status, counts, errMsg); err != nil {
			t.Fatalf("FinishIngestRun: %v", err)
		}
	}
	return id
}

// runTbody returns the <tbody> of one run from a page or a swap response.
func runTbody(t *testing.T, body string, id int64) string {
	t.Helper()
	m := regexp.MustCompile(fmt.Sprintf(`(?s)<tbody id="run-%d".*?</tbody>`, id)).FindString(body)
	if m == "" {
		t.Fatalf("no tbody for run %d; body=%s", id, body)
	}
	return m
}

func mustContain(t *testing.T, what, body string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(body, want) {
			t.Errorf("%s missing %q; body=%s", what, want, body)
		}
	}
}

func mustNotContain(t *testing.T, what, body string, nots ...string) {
	t.Helper()
	for _, not := range nots {
		if strings.Contains(body, not) {
			t.Errorf("%s has %q; body=%s", what, not, body)
		}
	}
}

func logLine(level, msg string) string {
	b, _ := json.Marshal(map[string]string{"level": level, "time": "2026-10-30T10:00:00Z", "message": msg})
	return string(b)
}

func TestIngestionPageEmpty(t *testing.T) {
	w := ingestionWeb(t)
	body := get(t, w, "/ingestion")

	mustContain(t, "page", body,
		"<title>Ingestion</title>",
		"<h1>Ingestion</h1>",
		"Ingestion fetches every enabled feed, then scores new items against your profile.",
		`class="ingb"`, `data-status="none"`, "No ingestion run yet",
		`data-testid="ingest-history"`,
		`<caption class="stp-sr">Recent ingestion runs</caption>`,
		"Recent runs",
		"A run fetches every enabled feed, then scores the new items. Each feed uses its own since and max items.",
		"No runs yet.",
		// The topbar keeps Sources lit.
		`<a class="active" href="/sources">Sources</a>`,
	)
	mustNotContain(t, "empty page", body, "Run pages", "Newer runs", "Older runs", "in-d")
}

func TestIngestionPageLiveHook(t *testing.T) {
	w := ingestionWeb(t)
	body := get(t, w, "/ingestion?page=1")
	mustContain(t, "page", body, `hx-get="/ingestion/live?page=1"`, `hx-trigger="sources-ingest-change from:body"`)
	mustNotContain(t, "page", body, "/sources/live")

	// The other sections keep their own hook.
	mustContain(t, "overview", get(t, w, "/sources"), "/sources/live?page=overview")
	mustNotContain(t, "overview", get(t, w, "/sources"), "/ingestion/live")
}

func TestIngestionRunsRender(t *testing.T) {
	w := ingestionWeb(t)
	okID := seedIngestionRun(t, w, store.IngestTriggerManual, store.IngestStatusOK,
		store.IngestCounts{Fetched: 40, NewItems: 12, Scored: 12}, "")
	errID := seedIngestionRun(t, w, store.IngestTriggerManual, store.IngestStatusError,
		store.IngestCounts{}, "scoring model unreachable")
	canID := seedIngestionRun(t, w, store.IngestTriggerManual, store.IngestStatusCancelled,
		store.IngestCounts{}, "cancelled")
	rescoreID := seedIngestionRun(t, w, store.IngestTriggerProfileRescore, store.IngestStatusOK,
		store.IngestCounts{Fetched: 30, Scored: 28, Failed: 2}, "")
	cronID := seedIngestionRun(t, w, store.IngestTriggerCron, store.IngestStatusOK,
		store.IngestCounts{NewItems: 1}, "")
	runID := seedIngestionRun(t, w, store.IngestTriggerManual, store.IngestStatusRunning, store.IngestCounts{}, "")

	body := get(t, w, "/ingestion")

	ok := runTbody(t, body, okID)
	mustContain(t, "ok run", ok,
		`data-status="ok"`, "ingb__dot--ok", ">Ok<", `data-label="New" class="in-n">12<`, "Manual", "<time datetime=")
	mustNotContain(t, "ok run", ok, "in-err")

	bad := runTbody(t, body, errID)
	mustContain(t, "error run", bad,
		`data-status="error"`, "ingb__dot--error", "Failed", `<span class="in-err">scoring model unreachable</span>`)

	can := runTbody(t, body, canID)
	mustContain(t, "cancelled run", can, `data-status="cancelled"`, "ingb__dot--cancelled", "Cancelled")
	mustNotContain(t, "cancelled run", can, "in-err", "scoring")
	if strings.Count(can, "cancelled") != 2 { // data-status and dot class only
		t.Errorf("cancelled text shown; tbody=%s", can)
	}

	res := runTbody(t, body, rescoreID)
	mustContain(t, "rescore run", res, "Profile rescore", "28 of 30 rescored")

	mustContain(t, "cron run", runTbody(t, body, cronID), "Scheduled")

	run := runTbody(t, body, runID)
	mustContain(t, "running row", run,
		`data-status="running"`, "ingb__dot--running", "Running", `class="in-n">-<`, "in progress")
	mustNotContain(t, "running row", run, "in-tog__b")
	mustContain(t, "finished row", ok, "in-tog__b", `aria-expanded="false"`, `hx-get="/ingestion/runs/`, "&#9660;")
	mustContain(t, "finished row", ok, "Show details for the run from ")
}

func TestIngestionPager(t *testing.T) {
	w := ingestionWeb(t)
	var ids []int64
	for range 12 {
		ids = append(ids, seedIngestionRun(t, w, store.IngestTriggerManual, store.IngestStatusOK,
			store.IngestCounts{NewItems: 1}, ""))
	}
	count := func(body string) int { return strings.Count(body, `<tbody id="run-`) }

	p1 := get(t, w, "/ingestion")
	if n := count(p1); n != 10 {
		t.Errorf("page 1 rows = %d, want 10", n)
	}
	mustContain(t, "page 1", p1, `aria-label="Run pages"`, `href="/ingestion?page=2"`, "Older runs", "Page 1")
	mustNotContain(t, "page 1", p1, "Newer runs")
	// Newest first: the last seeded run leads.
	newest, oldest := fmt.Sprintf(`id="run-%d"`, ids[11]), fmt.Sprintf(`id="run-%d"`, ids[0])
	if !strings.Contains(p1, newest) || strings.Contains(p1, oldest) {
		t.Errorf("page 1 holds the wrong runs")
	}

	p2 := get(t, w, "/ingestion?page=2")
	if n := count(p2); n != 2 {
		t.Errorf("page 2 rows = %d, want 2", n)
	}
	mustContain(t, "page 2", p2, `href="/ingestion"`, "Newer runs", "Page 2", fmt.Sprintf(`id="run-%d"`, ids[0]))
	mustNotContain(t, "page 2", p2, "Older runs")

	// A bad page clamps to the first.
	for _, q := range []string{"?page=0", "?page=-3", "?page=abc", "?page=", "?page=99999999999999999999"} {
		if n := count(get(t, w, "/ingestion"+q)); n != 10 {
			t.Errorf("%s rows = %d, want 10", q, n)
		}
	}
	// One past the last shows the last, on the page and on the live refresh.
	for _, path := range []string{"/ingestion?page=50", "/ingestion?page=3", "/ingestion/live?page=50"} {
		past := get(t, w, path)
		if n := count(past); n != 2 {
			t.Errorf("%s rows = %d, want 2", path, n)
		}
		mustNotContain(t, path, past, "Older runs")
	}
	mustContain(t, "past the last", get(t, w, "/ingestion?page=50"), "Page 2", "Newer runs")
	mustContain(t, "live past the last", get(t, w, "/ingestion/live?page=50"), "Page 2")
	// The live refresh stays on its page.
	if n := count(get(t, w, "/ingestion/live?page=2")); n != 2 {
		t.Errorf("live page 2 rows = %d, want 2", n)
	}

	// Ten runs or fewer is one page and no pager.
	small := ingestionWeb(t)
	seedIngestionRun(t, small, store.IngestTriggerManual, store.IngestStatusOK, store.IngestCounts{}, "")
	mustNotContain(t, "one page", get(t, small, "/ingestion"), "Run pages")
}

func TestIngestionRunDetail(t *testing.T) {
	w := ingestionWeb(t)
	id := seedIngestionRun(t, w, store.IngestTriggerManual, store.IngestStatusOK,
		store.IngestCounts{Fetched: 40, NewItems: 12, Scored: 11, Skipped: 1, Failed: 3}, "")
	at := time.Now()
	if err := w.db.RecordRunFetches(t.Context(), id, []store.FeedFetch{
		{
			FeedID: "a", Name: "Alpha Blog", URL: "https://a.test/feed", At: at,
			Items: 7, Elapsed: 412 * time.Millisecond,
		},
		{
			FeedID: "b", Name: "Beta Blog", URL: "https://b.test/feed", At: at, Elapsed: 20 * time.Second,
			Error: "Get \"https://b.test/feed\": context deadline exceeded",
		},
	}); err != nil {
		t.Fatalf("RecordRunFetches: %v", err)
	}
	lines := []string{logLine("info", "fetch finished"), logLine("debug", "scored batch")}
	if err := w.db.SaveIngestRunLog(t.Context(), id, lines); err != nil {
		t.Fatalf("SaveIngestRunLog: %v", err)
	}

	out := get(t, w, fmt.Sprintf("/ingestion/runs/%d", id))
	mustContain(t, "detail", out,
		fmt.Sprintf(`<tbody id="run-%d"`, id), `class="in-d"`, `aria-expanded="true"`, "&#9650;",
		fmt.Sprintf(`hx-get="/ingestion/runs/%d?collapse=1"`, id), "Hide details for the run from ",
		"<b>40</b> fetched, <b>12</b> new, <b>11</b> scored, <b>1</b> skipped, <b>3</b> failed",
		"Feeds in this run", "Alpha Blog", "Beta Blog", "412ms", "20s",
		"Run log", "fetch finished", "scored batch",
	)
	mustNotContain(t, "detail", out, "No per-feed results", "No log captured")
	// Failed feeds come first, with their plain-language reason under the word.
	if strings.Index(out, "Beta Blog") > strings.Index(out, "Alpha Blog") {
		t.Errorf("failed feed not first")
	}
	mustContain(t, "failed feed", out,
		`<span class="in-err" title="Get &#34;https://b.test/feed&#34;: context deadline exceeded">`)

	// Collapse gives the summary row alone, with the toggle back to open.
	col := get(t, w, fmt.Sprintf("/ingestion/runs/%d?collapse=1", id))
	mustContain(t, "collapsed", col, fmt.Sprintf(`<tbody id="run-%d"`, id), `aria-expanded="false"`)
	mustNotContain(t, "collapsed", col, "in-d", "Feeds in this run", "fetch finished", `?collapse=1`)
}

func TestIngestionRunDetailEmpty(t *testing.T) {
	w := ingestionWeb(t)
	id := seedIngestionRun(t, w, store.IngestTriggerManual, store.IngestStatusOK, store.IngestCounts{}, "")
	out := get(t, w, fmt.Sprintf("/ingestion/runs/%d", id))
	mustContain(t, "detail", out, "No per-feed results were recorded for this run.", "No log captured for this run.")

	re := seedIngestionRun(t, w, store.IngestTriggerProfileRescore, store.IngestStatusOK,
		store.IngestCounts{Fetched: 9, Scored: 8, Failed: 1}, "")
	out = get(t, w, fmt.Sprintf("/ingestion/runs/%d", re))
	mustContain(t, "rescore detail", out, "<b>9</b> candidates, <b>8</b> rescored, <b>1</b> failed")
	mustNotContain(t, "rescore detail", out, "fetched,")

	// A run in progress has nothing to expand.
	live := seedIngestionRun(t, w, store.IngestTriggerManual, store.IngestStatusRunning, store.IngestCounts{}, "")
	out = get(t, w, fmt.Sprintf("/ingestion/runs/%d", live))
	mustContain(t, "running", out, "in progress")
	mustNotContain(t, "running", out, "in-d", "in-tog__b")
}

func TestIngestionRunErrors(t *testing.T) {
	w := ingestionWeb(t)
	for path, want := range map[string]int{
		"/ingestion/runs/999":  http.StatusNotFound,
		"/ingestion/runs/abc":  http.StatusBadRequest,
		"/ingestion/runs/1.5":  http.StatusBadRequest,
		"/ingestion/runs/-1":   http.StatusNotFound,
		"/ingestion/runs/9e99": http.StatusBadRequest,
	} {
		rec := httptest.NewRecorder()
		w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != want {
			t.Errorf("GET %s = %d, want %d", path, rec.Code, want)
		}
	}
}

func TestIngestionLive(t *testing.T) {
	w := ingestionWeb(t)
	id := seedIngestionRun(t, w, store.IngestTriggerManual, store.IngestStatusOK, store.IngestCounts{NewItems: 2}, "")

	body := get(t, w, "/ingestion/live")
	wantAttrs(t, "unit", tagWith(t, body, "id", "ingLive"), map[string]string{"hx-swap-oob": "true"})
	mustContain(t, "live", body,
		`id="ingRuns"`, `class="ingb"`, `data-testid="ingest-history"`, fmt.Sprintf(`<tbody id="run-%d"`, id))
	mustNotContain(t, "live", body, "<h1", "in-d")
	// The full page does not carry the OOB flag.
	mustNotContain(t, "page", get(t, w, "/ingestion"), "hx-swap-oob")

	// The route is quiet-logged: its access line drops to debug.
	var buf bytes.Buffer
	h := httplog.Middleware(zerolog.New(&buf))(w.Routes())
	for _, path := range []string{"/ingestion/live", "/ingestion"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}
	levels := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		levels[fmt.Sprint(ev["path"])] = fmt.Sprint(ev["level"])
	}
	if levels["/ingestion/live"] != "debug" || levels["/ingestion"] != "info" {
		t.Errorf("access levels = %v", levels)
	}
}

// A run started elsewhere reaches the page through the live refresh: the bar
// follows the run, and the Manage badge carries the failing count.
func TestIngestionLiveCarriesBarAndBadge(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	feed := config.Feed{Name: "Beta", URL: srv.URL + "/feed"}
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{feed}})
	w.SetLibrary(library.NewLoader(library.Options{BaseURL: "off"}))
	w.ing.SetClient(srv.Client())
	recordFetches(t, db, feed, 0, 3)
	if err := w.ing.Start(t.Context(), store.IngestTriggerManual); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { w.ing.Shutdown(context.Background()) })

	out := get(t, w, "/ingestion/live")
	unit := regexp.MustCompile(`(?s)<div id="ingLive".*?</section>\s*<section[^>]*id="ingRuns"`).FindString(out)
	if unit == "" {
		t.Fatalf("the bar is not inside the live unit ahead of the runs; body=%s", out)
	}
	mustContain(t, "unit", unit, `data-status="running"`, "Running ingestion")
	// The running bar polls itself once; the unit carries no poll of its own.
	if n := strings.Count(out, `hx-trigger="every 2s"`); n != 1 {
		t.Errorf("polling triggers = %d, want 1; body=%s", n, out)
	}
	wantAttrs(t, "badge", tagWith(t, out, "id", "srcNavBadge"), map[string]string{"hx-swap-oob": "true"})
	mustContain(t, "badge", out, `title="1 failing"`)

	// The full page holds the bar and the runs under the same unit, in place.
	mustContain(t, "page", get(t, w, "/ingestion"), `<div id="ingLive">`)
}

func TestIngestionNav(t *testing.T) {
	w := ingestionWeb(t)
	on := regexp.MustCompile(`<a class="sh-ni is-on"[^>]*>`)

	body := get(t, w, "/ingestion")
	mustContain(t, "nav", body, `<a class="sh-ni is-on" href="/ingestion" aria-current="page">Ingestion</a>`)
	if got := on.FindAllString(body, -1); len(got) != 1 {
		t.Errorf("lit nav items on /ingestion = %v, want only Ingestion", got)
	}
	mustNotContain(t, "nav", body, "preview")

	for _, path := range []string{"/sources", "/sources/manage", "/sources/library"} {
		page := get(t, w, path)
		mustContain(t, path, page, `<a class="sh-ni" href="/ingestion">Ingestion</a>`)
		mustNotContain(t, path, page, `href="/ingestion" aria-current`)
	}
	// It sits after the Library group, behind a separator.
	page := get(t, w, "/sources")
	sep := strings.Index(page, `<span class="sh-nav__sep" aria-hidden="true"></span>
    <a class="sh-ni" href="/ingestion">`)
	if lib := strings.Index(page, `id="srcLibNav"`); sep < 0 || sep < lib {
		t.Errorf("Ingestion link not behind a separator after the Library group")
	}
}

func TestIngestModalLinksAllRuns(t *testing.T) {
	w := ingestionWeb(t)
	mustContain(t, "modal", get(t, w, "/ingest"), `<a class="modal__act" href="/ingestion">▤ all runs</a>`)
}

// Everything a run recorded is untrusted: it renders as text.
func TestIngestionEscapesRecordedText(t *testing.T) {
	const evil = `<img src=x onerror=alert(1)>`
	w := ingestionWeb(t)
	id := seedIngestionRun(t, w, store.IngestTriggerManual, store.IngestStatusError, store.IngestCounts{}, evil+" run")
	if err := w.db.RecordRunFetches(t.Context(), id, []store.FeedFetch{
		{FeedID: "x", Name: evil + " feed", URL: "https://x.test/", Error: evil + " fetch", At: time.Now()},
	}); err != nil {
		t.Fatalf("RecordRunFetches: %v", err)
	}
	if err := w.db.SaveIngestRunLog(t.Context(), id, []string{
		logLine("error", evil+" log"),
		`{"level":"info","message":"kv","k":"` + evil + `"}`,
		evil + " raw",
	}); err != nil {
		t.Fatalf("SaveIngestRunLog: %v", err)
	}

	for _, path := range []string{"/ingestion", "/ingestion/live", fmt.Sprintf("/ingestion/runs/%d", id)} {
		body := get(t, w, path)
		if strings.Contains(body, "<img") {
			t.Errorf("%s: markup from a recorded string was not escaped; body=%s", path, body)
		}
		mustContain(t, path, body, "&lt;img src=x onerror=alert(1)&gt; run")
	}
	detail := get(t, w, fmt.Sprintf("/ingestion/runs/%d", id))
	mustContain(t, "detail", detail,
		"&lt;img src=x onerror=alert(1)&gt; feed",
		"&lt;img src=x onerror=alert(1)&gt; log",
		"&lt;img src=x onerror=alert(1)&gt; raw")
}

func TestIngestionGated(t *testing.T) {
	b := newBrowser(t, gatedWeb(t))
	wantRedirect(t, b.get("/ingestion"), "/login?next=%2Fingestion")
	wantRedirect(t, b.get("/ingestion/runs/1"), "/login?next=%2Fingestion%2Fruns%2F1")
	wantStatus(t, b.get("/ingestion/live", "HX-Request", "true"), http.StatusUnauthorized)
}
