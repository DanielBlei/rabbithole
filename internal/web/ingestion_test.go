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
	"os"
	"regexp"
	"strings"
	"sync"
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
		"<h1><span>Ingestion</span>",
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
		`data-status="running"`, "ingb__dot--running", "Running", `class="in-n">-<`,
		`<span class="run-clock" data-elapsed-ms="`)
	mustNotContain(t, "running row", run, "in progress")
	// A run no manager is running (a stale row) has nothing to expand: no
	// toggle, and the row says so for the pointer styles.
	mustContain(t, "running row", run, `class="in-r in-r--static"`)
	mustNotContain(t, "running row", run, "in-tog__b", "aria-expanded", "every 2s", "in-d")
	mustContain(t, "finished row", ok, "in-tog__b", `aria-expanded="false"`, `hx-get="/ingestion/runs/`,
		`aria-controls="run-`, `<svg class="in-chev"`, `class="in-r"`)
	mustNotContain(t, "finished row", ok, "data-elapsed", "&#9660;")
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
		fmt.Sprintf(`<tbody id="run-%d"`, id), `class="in-d"`, `aria-expanded="true"`,
		fmt.Sprintf(`hx-get="/ingestion/runs/%d?collapse=1"`, id), "Hide details for the run from ",
		"<b>40</b> fetched, <b>12</b> new, <b>11</b> scored, <b>1</b> skipped, <b>3</b> failed",
		"Feeds in this run", "Alpha Blog", "Beta Blog", "412ms", "20s",
		"Run log", "fetch finished",
		fmt.Sprintf(`<div class="in-log" id="run-%d-log" data-ing-lines>`, id),
		`data-ing-lvl="info"`, `data-ing-lvl="dbg"`,
	)
	// A run opens with info lines and above; the debug ones wait for a request.
	mustNotContain(t, "detail", out, "scored batch", "ing-ln--dbg", "data-dbg")
	// A finished run does not poll, and has no live marker.
	mustNotContain(t, "detail", out, "No per-feed results", "No log captured", "every 2s", "hx-trigger", "ing__live")
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

	// A row left running with no manager run behind it has nothing to expand.
	live := seedIngestionRun(t, w, store.IngestTriggerManual, store.IngestStatusRunning, store.IngestCounts{}, "")
	out = get(t, w, fmt.Sprintf("/ingestion/runs/%d", live))
	mustContain(t, "running", out, "data-elapsed-ms=")
	mustNotContain(t, "running", out, "in-d", "every 2s", "in-tog__b")
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
	// The running bar polls itself once, and the live run's log once more.
	if n := strings.Count(out, `every 2s`); n != 2 {
		t.Errorf("polling triggers = %d, want 2; body=%s", n, out)
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
	wantStatus(t, b.get("/ingestion/runs/1/log?after=0", "HX-Request", "true"), http.StatusUnauthorized)
	wantStatus(t, b.get("/ingestion/live", "HX-Request", "true"), http.StatusUnauthorized)
}

// blockedRun starts a run whose feed never answers, so it stays live until the
// returned release is called, and waits until it has logged a line.
func blockedRun(t *testing.T) (w *Web, id int64, release func()) {
	t.Helper()
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-done:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)

	feed := config.Feed{Name: "Beta", URL: srv.URL + "/feed"}
	w, _ = sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{feed}})
	w.SetLibrary(library.NewLoader(library.Options{BaseURL: "off"}))
	w.ing.SetClient(srv.Client())
	if err := w.ing.Start(t.Context(), store.IngestTriggerManual); err != nil {
		t.Fatalf("Start: %v", err)
	}
	var once sync.Once
	release = func() { once.Do(func() { close(done) }) }
	t.Cleanup(func() { release(); w.ing.Shutdown(context.Background()) })

	deadline := time.Now().Add(5 * time.Second)
	for len(w.ing.Status().Lines) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the run logged nothing")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return w, w.ing.Status().RunID, release
}

// The run in flight is always open, on page load and on every refresh, with its
// live log ending in the cursor that polls for newer lines. The row itself does
// not poll. Once the run finishes the same row is the finished detail, still
// open, with no poll.
func TestIngestionLiveRunRow(t *testing.T) {
	w, id, release := blockedRun(t)
	first := firstInfoLine(t, w)
	st := w.ing.Status()

	body := get(t, w, "/ingestion")
	row := runTbody(t, body, id)
	for attr := range tagWith(t, row, "id", fmt.Sprintf("run-%d", id)) {
		if strings.HasPrefix(attr, "hx-") {
			t.Errorf("live tbody has %s: the row does not poll", attr)
		}
	}
	mustContain(t, "live row", row,
		`aria-expanded="true"`, fmt.Sprintf(`hx-get="/ingestion/runs/%d?collapse=1"`, id),
		`class="in-d"`, `ing__live`, first,
		fmt.Sprintf(`<div class="in-log" id="run-%d-log" data-ing-lines data-live>`, id),
		`<span class="run-clock" data-elapsed-ms="`, `data-ing-lvl="info"`)
	mustNotContain(t, "live row", row, "No log captured", "fetched,", "data-dbg")
	cursor := regexp.MustCompile(`<span class="in-cur"[^>]*>`).FindString(row)
	wantAttrs(t, "cursor", tagWith(t, cursor, "id", fmt.Sprintf("run-%d-cur", id)), map[string]string{
		"hx-get":     fmt.Sprintf("/ingestion/runs/%d/log?after=%d", id, st.Seq),
		"hx-trigger": "every 2s", "hx-target": "this", "hx-swap": "outerHTML",
	})

	// Opening the row again answers with the same live detail.
	mustContain(t, "reopen", get(t, w, fmt.Sprintf("/ingestion/runs/%d", id)),
		`class="in-d"`, first, fmt.Sprintf(`id="run-%d-cur"`, id))

	// A collapsed live row has no detail and nothing to poll.
	col := get(t, w, fmt.Sprintf("/ingestion/runs/%d?collapse=1", id))
	mustContain(t, "collapsed live", col, `aria-expanded="false"`, `data-elapsed-ms=`, "in-tog__b")
	mustNotContain(t, "collapsed live", col, "every 2s", "in-d")

	// The live refresh opens it too, with nothing named in open=.
	mustContain(t, "live refresh", get(t, w, "/ingestion/live"), `every 2s`, `class="in-d"`, "in-cur")

	release()
	waitIngestIdle(t, w)
	done := get(t, w, fmt.Sprintf("/ingestion/runs/%d", id))
	mustContain(t, "finished", done, `aria-expanded="true"`, `class="in-d"`, "Feeds in this run", "fetched,")
	mustNotContain(t, "finished", done, "every 2s", "hx-trigger", "ing__live", "data-elapsed", "in-cur", "data-live")
}

// A page load fetches no log for a finished run: its row is closed, with the
// toggle and no log markup, until the reader opens it.
func TestIngestionPageLoadFetchesNoFinishedLog(t *testing.T) {
	w := ingestionWeb(t)
	id := seedIngestionRun(t, w, store.IngestTriggerManual, store.IngestStatusOK, store.IngestCounts{}, "")
	if err := w.db.SaveIngestRunLog(t.Context(), id, []string{logLine("info", "stored line")}); err != nil {
		t.Fatalf("SaveIngestRunLog: %v", err)
	}
	row := runTbody(t, get(t, w, "/ingestion"), id)
	mustContain(t, "closed row", row, `aria-expanded="false"`, fmt.Sprintf(`hx-get="/ingestion/runs/%d"`, id))
	mustNotContain(t, "closed row", row, `class="in-d"`, "stored line", "in-log")
}

// firstInfoLine is the message of the live run's first info line: a run opens
// with info lines only, so a debug first line is not on the page.
func firstInfoLine(t *testing.T, w *Web) string {
	t.Helper()
	for _, raw := range w.ing.Status().Lines {
		if ll := parseIngestLogLine(raw); !ll.Debug {
			return ll.Msg
		}
	}
	t.Fatal("the run logged no info line")
	return ""
}

// getRec issues a GET and returns the whole response, whatever its status.
func getRec(t *testing.T, w *Web, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// The live poll returns only the lines after its cursor, with the next cursor,
// and nothing (204) when there is nothing new.
func TestIngestionLogPollAppendsNewerLines(t *testing.T) {
	w, id, _ := blockedRun(t)
	st := w.ing.Status()
	if len(st.Lines) < 2 {
		t.Skip("run logged fewer than two lines")
	}
	logPath := func(q string) string { return fmt.Sprintf("/ingestion/runs/%d/log?%s", id, q) }

	// From the start: every info line, then the cursor at the last line.
	all := getRec(t, w, logPath("after=0"))
	if all.Code != http.StatusOK {
		t.Fatalf("after=0 = %d, want 200", all.Code)
	}
	mustContain(t, "after=0", all.Body.String(), firstInfoLine(t, w),
		fmt.Sprintf(`id="run-%d-cur"`, id), `every 2s`)
	mustNotContain(t, "after=0", all.Body.String(), "<tbody", `class="in-log"`, "hx-swap-oob")

	// From the second to last: only the last line.
	prev := getRec(t, w, logPath(fmt.Sprintf("after=%d", st.Seq-1))).Body.String()
	last := parseIngestLogLine(st.Lines[len(st.Lines)-1])
	if !last.Debug {
		mustContain(t, "after=last-1", prev, last.Msg)
	}
	mustNotContain(t, "after=last-1", prev, firstInfoLine(t, w))

	// Nothing newer: no content, so nothing is swapped and the cursor stays.
	none := getRec(t, w, logPath(fmt.Sprintf("after=%d", w.ing.Status().Seq)))
	if none.Code != http.StatusNoContent || none.Body.Len() != 0 {
		t.Errorf("no new lines = %d with %q, want 204 and no body", none.Code, none.Body)
	}

	// Junk cursors read as zero rather than failing.
	if rec := getRec(t, w, logPath("after=abc")); rec.Code != http.StatusOK {
		t.Errorf("after=abc = %d, want 200", rec.Code)
	}
	if rec := getRec(t, w, "/ingestion/runs/abc/log?after=0"); rec.Code != http.StatusBadRequest {
		t.Errorf("bad id = %d, want 400", rec.Code)
	}
}

// A poll that finds its run over answers once with the finished row, retargeted
// at the run's row, and that row no longer polls.
func TestIngestionLogPollSwitchesToFinishedRow(t *testing.T) {
	w, id, release := blockedRun(t)
	cursor := w.ing.Status().Seq
	release()
	waitIngestIdle(t, w)

	rec := getRec(t, w, fmt.Sprintf("/ingestion/runs/%d/log?after=%d", id, cursor))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("HX-Retarget"); got != fmt.Sprintf("#run-%d", id) {
		t.Errorf("HX-Retarget = %q, want the run's row", got)
	}
	if got := rec.Header().Get("HX-Reswap"); got != "outerHTML" {
		t.Errorf("HX-Reswap = %q, want outerHTML", got)
	}
	out := rec.Body.String()
	mustContain(t, "switch", out, fmt.Sprintf(`<tbody id="run-%d"`, id), `aria-expanded="true"`,
		`class="in-d"`, "Feeds in this run", "fetched,")
	mustNotContain(t, "switch", out, "every 2s", "hx-trigger", "in-cur", "data-live", "ing__live")

	// Asking again keeps answering with the finished row; nothing polls on.
	again := getRec(t, w, fmt.Sprintf("/ingestion/runs/%d/log?after=%d", id, cursor))
	mustNotContain(t, "again", again.Body.String(), "every 2s", "in-cur")

	// A run that does not exist answers 200 with no body: the swap removes the
	// poller, where a 404 would leave it polling forever.
	if rec := getRec(t, w, "/ingestion/runs/9999/log?after=1"); rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Errorf("unknown run = %d with %q, want 200 and no body", rec.Code, rec.Body)
	}
}

// A log opens with info lines only; ?full=1&debug=1 returns the whole log
// element with the debug lines and marks it, and a page that picked debug asks
// for them on the row, the live refresh and the switch-over alike.
func TestIngestionLogDebugFetch(t *testing.T) {
	w := ingestionWeb(t)
	id := seedIngestionRun(t, w, store.IngestTriggerManual, store.IngestStatusOK, store.IngestCounts{}, "")
	lines := []string{
		logLine("info", "kept info"), logLine("debug", "hidden debug"),
		logLine("trace", "hidden trace"), logLine("warn", "kept warn"),
	}
	if err := w.db.SaveIngestRunLog(t.Context(), id, lines); err != nil {
		t.Fatalf("SaveIngestRunLog: %v", err)
	}
	logOf := fmt.Sprintf("/ingestion/runs/%d/log?full=1", id)

	light := get(t, w, logOf)
	mustContain(t, "info only", light, fmt.Sprintf(`<div class="in-log" id="run-%d-log" data-ing-lines>`, id),
		"kept info", "kept warn")
	mustNotContain(t, "info only", light, "hidden debug", "hidden trace", "data-dbg", "<tbody")

	full := get(t, w, logOf+"&debug=1")
	mustContain(t, "debug", full, fmt.Sprintf(`id="run-%d-log" data-ing-lines data-dbg>`, id),
		"kept info", "hidden debug", "hidden trace", "kept warn", "ing-ln--dbg")
	// Order is kept: the debug lines sit between the lines around them.
	if strings.Index(full, "kept info") > strings.Index(full, "hidden debug") ||
		strings.Index(full, "hidden trace") > strings.Index(full, "kept warn") {
		t.Errorf("debug lines are out of order")
	}

	for _, path := range []string{
		fmt.Sprintf("/ingestion/runs/%d?debug=1", id), fmt.Sprintf("/ingestion/live?debug=1&open=%d", id),
	} {
		mustContain(t, path, get(t, w, path), "hidden debug", "data-dbg")
	}
	mustNotContain(t, "live refresh", get(t, w, fmt.Sprintf("/ingestion/live?open=%d", id)), "hidden debug")

	if rec := getRec(t, w, "/ingestion/runs/9999/log?full=1"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown run = %d, want 404", rec.Code)
	}
}

// A live log's cursor carries its log's level, so a log that holds debug lines
// keeps getting them whatever level the page shows now; an info log stays info.
func TestIngestionLogCursorCarriesLevel(t *testing.T) {
	w, id, _ := blockedRun(t)
	cursor := func(q string) string {
		return regexp.MustCompile(`<span class="in-cur"[^>]*>`).FindString(get(t, w, q))
	}
	info := cursor(fmt.Sprintf("/ingestion/runs/%d/log?after=0", id))
	mustContain(t, "info cursor", info, fmt.Sprintf(`hx-get="/ingestion/runs/%d/log?after=`, id))
	mustNotContain(t, "info cursor", info, "debug")

	dbg := cursor(fmt.Sprintf("/ingestion/runs/%d/log?after=0&debug=1", id))
	mustContain(t, "debug cursor", dbg, `&amp;debug=1"`)
	if full := cursor(
		fmt.Sprintf("/ingestion/runs/%d/log?full=1&debug=1", id),
	); !strings.Contains(
		full,
		`&amp;debug=1"`,
	) {
		t.Errorf("full debug fetch cursor = %s, want debug=1", full)
	}
	if row := cursor(fmt.Sprintf("/ingestion/runs/%d?debug=1", id)); !strings.Contains(row, `&amp;debug=1"`) {
		t.Errorf("debug row cursor = %s, want debug=1", row)
	}
}

// An empty log says why in real text: waiting while the run has logged nothing,
// and not waiting once only debug lines exist.
func TestIngestionEmptyLogNote(t *testing.T) {
	var buf bytes.Buffer
	render := func(d ingestionDetail) string {
		buf.Reset()
		if err := sourcesTmpl.ExecuteTemplate(&buf, "ingestionLog", d); err != nil {
			t.Fatalf("render: %v", err)
		}
		return buf.String()
	}
	dbgOnly := []string{logLine("debug", "only debug")}

	var live ingestionDetail
	live.ID, live.Live = 1, true
	live.setLog(runLogSrc{}, false)
	mustContain(t, "nothing logged", render(live), "Waiting for the first log line.", "in-empty--wait")

	live.setLog(runLogSrc{Lines: dbgOnly, Seq: 1}, false)
	out := render(live)
	mustContain(t, "debug only", out, "No info lines. Pick debug for the rest.")
	mustNotContain(t, "debug only", out, "Waiting", "only debug", "in-empty--wait")

	live.setLog(runLogSrc{Lines: append(dbgOnly, logLine("info", "an info line")), Seq: 2}, false)
	mustNotContain(t, "info line", render(live), "in-empty", "Waiting")

	done := ingestionDetail{ID: 2}
	done.setLog(runLogSrc{Lines: dbgOnly}, false)
	mustContain(t, "finished debug only", render(done), "No info lines. Pick debug for the rest.")
}

// Lines that fell off the ring show as a marker, in the log and in a poll.
func TestDisplayLogLinesMarksDroppedLines(t *testing.T) {
	raw := []string{logLine("info", "a"), logLine("debug", "b")}

	got := displayLogLines(raw, 0, false)
	if len(got) != 1 || got[0].Msg != "a" || got[0].Marker {
		t.Errorf("no drop, info only = %+v", got)
	}
	got = displayLogLines(raw, 12, true)
	if len(got) != 3 || !got[0].Marker || got[0].Msg != "12 earlier lines dropped" {
		t.Errorf("12 dropped = %+v", got)
	}
	if got := displayLogLines(nil, 1, false); len(got) != 1 || got[0].Msg != "1 earlier line dropped" {
		t.Errorf("1 dropped = %+v", got)
	}

	var buf bytes.Buffer
	data := ingestionLogAppend{ID: 7, Seq: 40, Logs: displayLogLines(raw, 12, false)}
	if err := sourcesTmpl.ExecuteTemplate(&buf, "ingestionLogAppend", data); err != nil {
		t.Fatalf("render: %v", err)
	}
	mustContain(t, "append", buf.String(), `<div class="ing-ln ing-ln--gap">12 earlier lines dropped</div>`,
		`hx-get="/ingestion/runs/7/log?after=40"`)
}

// The logs wear the app's thin scrollbar, and the log bar has the level pills
// alone, the page and the modal alike.
func TestIngestionLogScrollbar(t *testing.T) {
	w := ingestionWeb(t)
	id := seedIngestionRun(t, w, store.IngestTriggerManual, store.IngestStatusOK, store.IngestCounts{}, "")
	if err := w.db.SaveIngestRunLog(t.Context(), id, []string{logLine("info", "x")}); err != nil {
		t.Fatalf("SaveIngestRunLog: %v", err)
	}
	mustContain(t, "run detail", get(t, w, fmt.Sprintf("/ingestion/runs/%d", id)),
		`class="in-logbar"`, `data-ing-lvl="info"`)
	mustNotContain(t, "run detail", get(t, w, fmt.Sprintf("/ingestion/runs/%d", id)), "data-ing-jump")

	live, _, _ := blockedRun(t)
	modal := get(t, live, "/ingest/status")
	mustContain(t, "modal", modal, `class="ing__logbar"`, `data-ing-lvl="dbg"`)
	mustNotContain(t, "modal", modal, "data-ing-jump")

	css, err := os.ReadFile("static/style.css")
	if err != nil {
		t.Fatalf("read css: %v", err)
	}
	re := regexp.MustCompile(
		`(?m)^[^{\n]*\.in-log[^{\n]*\.ing__lines[^{\n]*\{[^}]*color-scheme:dark;scrollbar-width:thin;scrollbar-color:var\(--line\) transparent`,
	)
	if !re.Match(css) {
		t.Errorf("the log scrollers are not in the shared thin-scrollbar rule")
	}
}

// The bar and the modal word a live run's age, and carry it for the page clock.
func TestIngestLiveClockRendered(t *testing.T) {
	w, _, _ := blockedRun(t)

	bar := get(t, w, "/ingest/status?view=bar")
	mustContain(t, "bar", bar, `<span class="ingb__when">running <span class="run-clock" data-elapsed-ms="`)
	mustNotContain(t, "bar", bar, "started")

	modal := get(t, w, "/ingest/status")
	mustContain(t, "modal", modal, `running <span class="run-clock" data-elapsed-ms="`)
	mustNotContain(t, "modal", modal, "started")
	mustContain(t, "modal", modal, `<div class="ing__lines" data-ing-lines>`)

	// While a run is live the modal hides the previous run's counts.
	prev := store.IngestCounts{Fetched: 60, NewItems: 60}
	seedIngestionRun(t, w, store.IngestTriggerManual, store.IngestStatusOK, prev, "")
	mustContain(t, "modal", get(t, w, "/ingest/status"), `<div class="ing__counts is-idle">`)

	// A finished run's bar has no clock.
	idle := ingestionWeb(t)
	mustNotContain(t, "idle", get(t, idle, "/ingest/status?view=bar"), "data-elapsed")
}

func TestRunClock(t *testing.T) {
	start := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		after time.Duration
		ms    int64
		text  string
	}{
		{-5 * time.Second, 0, "0s"},
		{0, 0, "0s"},
		{42900 * time.Millisecond, 42900, "42s"},
		{185 * time.Second, 185000, "3m 05s"},
		{72*time.Minute + 3*time.Second, 4323000, "1h 12m"},
	} {
		ms, text := runClock(start, start.Add(c.after))
		if ms != c.ms || text != c.text {
			t.Errorf("runClock(+%v) = %d %q, want %d %q", c.after, ms, text, c.ms, c.text)
		}
	}
}

// open= keeps the named rows open across the run-edge refresh; junk and ids
// past one page of runs are ignored.
func TestIngestionLiveKeepsOpenRows(t *testing.T) {
	w := ingestionWeb(t)
	var ids []int64
	for range 3 {
		ids = append(ids, seedIngestionRun(t, w, store.IngestTriggerManual, store.IngestStatusOK,
			store.IngestCounts{NewItems: 1}, ""))
	}
	if err := w.db.SaveIngestRunLog(t.Context(), ids[0], []string{logLine("info", "kept log")}); err != nil {
		t.Fatalf("SaveIngestRunLog: %v", err)
	}

	open := func(q string) map[int64]bool {
		body := get(t, w, "/ingestion/live"+q)
		got := map[int64]bool{}
		for _, id := range ids {
			got[id] = strings.Contains(runTbody(t, body, id), `class="in-d"`)
		}
		return got
	}
	if got := open(""); got[ids[0]] || got[ids[1]] || got[ids[2]] {
		t.Errorf("no open= opened rows: %v", got)
	}
	got := open(fmt.Sprintf("?open=%d,%d", ids[0], ids[2]))
	if !got[ids[0]] || got[ids[1]] || !got[ids[2]] {
		t.Errorf("open=%d,%d opened %v", ids[0], ids[2], got)
	}
	mustContain(t, "open row", get(t, w, fmt.Sprintf("/ingestion/live?open=%d", ids[0])),
		"kept log", `aria-expanded="true"`, fmt.Sprintf(`hx-get="/ingestion/runs/%d?collapse=1"`, ids[0]))

	// Junk is skipped without failing the refresh, and the good id still opens.
	got = open(fmt.Sprintf("?open=abc,,-1,0,1.5,99999999999999999999,%d,%d", ids[1], 1<<40))
	if got[ids[0]] || !got[ids[1]] || got[ids[2]] {
		t.Errorf("junk open= opened %v", got)
	}

	// Past one page of ids the rest are dropped.
	var long []string
	for i := range ingestHistoryLimit {
		long = append(long, fmt.Sprint(1000+i))
	}
	got = open("?open=" + strings.Join(long, ",") + fmt.Sprintf(",%d", ids[0]))
	if got[ids[0]] {
		t.Errorf("an id past the cap was opened: %v", got)
	}
}

func TestIngestionPageHoldsLevelState(t *testing.T) {
	body := get(t, ingestionWeb(t), "/ingestion")
	// The wrapper that holds the picked level sits outside the swapped unit.
	page, live := strings.Index(body, `id="ingPage"`), strings.Index(body, `id="ingLive"`)
	if page < 0 || live < page {
		t.Errorf("level host is not outside the live unit (ingPage at %d, ingLive at %d)", page, live)
	}
}

// A run that ended in the store while the manager still holds it (its log not
// stored yet) opens with the manager's log, finished and not polling.
func TestIngestionFinishedRowReadsManagerLog(t *testing.T) {
	w, id, _ := blockedRun(t)
	first := firstInfoLine(t, w)
	counts := store.IngestCounts{NewItems: 2}
	if err := w.db.FinishIngestRun(t.Context(), id, store.IngestStatusOK, counts, ""); err != nil {
		t.Fatalf("FinishIngestRun: %v", err)
	}

	for _, path := range []string{fmt.Sprintf("/ingestion/runs/%d", id), "/ingestion/live?open=" + fmt.Sprint(id)} {
		out := get(t, w, path)
		row := runTbody(t, out, id)
		mustContain(t, path, row, `data-status="ok"`, `aria-expanded="true"`, `class="in-d"`, first, "<b>2</b> new")
		mustNotContain(t, path, row, "every 2s", "ing__live", "data-elapsed-ms")
	}
}

// A live row sits on the manager's start, the clock the bar uses.
func TestIngestionLiveRowUsesManagerStart(t *testing.T) {
	w, id, _ := blockedRun(t)
	ms := func(body string) int64 {
		m := regexp.MustCompile(`data-elapsed-ms="(\d+)"`).FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("no clock; body=%s", body)
		}
		var n int64
		_, _ = fmt.Sscan(m[1], &n)
		return n
	}
	row, bar := ms(runTbody(t, get(t, w, "/ingestion"), id)), ms(get(t, w, "/ingest/status?view=bar"))
	if d := row - bar; d < -1500 || d > 1500 {
		t.Errorf("row clock %dms and bar clock %dms disagree", row, bar)
	}
}
