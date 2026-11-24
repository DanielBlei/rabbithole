// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/DanielBlei/rabbithole/internal/config"
)

// runLines runs one ingest at info level and returns its decoded log lines.
func runLines(t *testing.T, cfg *testCfg) []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	ctx := zerolog.New(&buf).Level(zerolog.InfoLevel).WithContext(t.Context())
	db := openStore(t, cfg)
	if _, err := Run(ctx, cfg.Config, testProfile("vllm inference"), db, time.Now(),
		Options{Record: true, Client: loopbackClient}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var lines []map[string]any
	for _, raw := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("decode %q: %v", raw, err)
		}
		lines = append(lines, m)
	}
	return lines
}

func linesWith(lines []map[string]any, msg string) []map[string]any {
	var out []map[string]any
	for _, l := range lines {
		if l["message"] == msg {
			out = append(out, l)
		}
	}
	return out
}

// Scoring prints a per-feed completion count at info, ending at of=total, and
// the heuristic scorer logs no model connect line.
func TestRunLogsScoringProgressNoHeuristicConnect(t *testing.T) {
	cfg := testConfig(t,
		config.Feed{Name: "A", URL: serveRSS(t, multiItemRSS(t, "a", 5))},
		config.Feed{Name: "B", URL: serveRSS(t, multiItemRSS(t, "b", 3))},
	)
	lines := runLines(t, cfg)

	wantOf := map[string]int{"A": 3, "B": 2} // batch size 2
	lastDone := map[string]int{}
	progress := linesWith(lines, "scoring progress")
	if len(progress) != 5 {
		t.Fatalf("scoring progress lines = %d, want 5: %v", len(progress), progress)
	}
	for _, l := range progress {
		feed, _ := l["feed"].(string)
		done, of := int(l["done"].(float64)), int(l["of"].(float64))
		if l["level"] != "info" {
			t.Errorf("level = %v, want info", l["level"])
		}
		if of != wantOf[feed] {
			t.Errorf("feed %s of = %d, want %d", feed, of, wantOf[feed])
		}
		if done != lastDone[feed]+1 {
			t.Errorf("feed %s done = %d after %d, want a rising count", feed, done, lastDone[feed])
		}
		if _, ok := l["elapsed"].(string); !ok {
			t.Errorf("feed %s line has no elapsed: %v", feed, l)
		}
		lastDone[feed] = done
	}
	for feed, of := range wantOf {
		if lastDone[feed] != of {
			t.Errorf("feed %s ended at done=%d, want %d", feed, lastDone[feed], of)
		}
	}

	// The heuristic scorer has no model to connect to.
	if connect := linesWith(lines, "connecting to model"); len(connect) != 0 {
		t.Errorf("heuristic run logged a connect line: %v", connect)
	}
	if got := linesWith(lines, "ingesting feeds"); len(got) != 1 || int(got[0]["feeds"].(float64)) != 2 {
		t.Errorf("ingesting feeds = %v, want one line with feeds=2", got)
	}
}

// The run-wide scoring progress counts batches across every feed: its "of"
// is the sum of the per-feed batch totals and its "done" rises monotonically
// from 1 to that total, while the per-feed lines keep their own counts.
func TestRunLogsRunWideScoringProgress(t *testing.T) {
	cfg := testConfig(t,
		config.Feed{Name: "A", URL: serveRSS(t, multiItemRSS(t, "a", 5))},
		config.Feed{Name: "B", URL: serveRSS(t, multiItemRSS(t, "b", 3))},
	)
	lines := runLines(t, cfg)

	run := linesWith(lines, "run scoring progress")
	wantTotal := 5 // 3 batches (5 items) + 2 batches (3 items), batch size 2
	if len(run) != wantTotal {
		t.Fatalf("run scoring progress lines = %d, want %d: %v", len(run), wantTotal, run)
	}
	for i, l := range run {
		if l["level"] != "info" {
			t.Errorf("level = %v, want info", l["level"])
		}
		done, of := int(l["done"].(float64)), int(l["of"].(float64))
		if of != wantTotal {
			t.Errorf("run line %d of = %d, want %d", i, of, wantTotal)
		}
		if done != i+1 {
			t.Errorf("run line %d done = %d, want %d (monotonic from 1)", i, done, i+1)
		}
		if _, ok := l["elapsed"].(string); !ok {
			t.Errorf("run line %d has no elapsed: %v", i, l)
		}
	}
}

// Every enabled feed gets exactly one accounting record — including feeds
// that return no items, feeds whose fetch fails, and feeds with nothing new
// after dedup — with the funnel counts that distinguish raw fetches from
// filtered candidates from scoring input.
func TestRunLogsFeedAccountingForEveryEnabledFeed(t *testing.T) {
	cfg := testConfig(t,
		config.Feed{Name: "Full", URL: serveRSS(t, multiItemRSS(t, "full", 4))},
		config.Feed{Name: "Empty", URL: serveRSS(t, multiItemRSS(t, "empty", 0))},
		config.Feed{Name: "Dead", URL: "http://127.0.0.1:1/feed"},
	)
	lines := runLines(t, cfg)

	accounts := linesWith(lines, "feed accounting")
	if len(accounts) != 3 {
		t.Fatalf("feed accounting lines = %d, want 3: %v", len(accounts), accounts)
	}
	byFeed := map[string]map[string]any{}
	for _, l := range accounts {
		feed, _ := l["feed"].(string)
		if _, dup := byFeed[feed]; dup {
			t.Fatalf("feed %s logged more than one accounting record: %v", feed, accounts)
		}
		byFeed[feed] = l
	}
	full := byFeed["Full"]
	if int(full["raw"].(float64)) != 4 || int(full["filtered"].(float64)) != 4 ||
		int(full["new"].(float64)) != 4 || int(full["skipped"].(float64)) != 0 {
		t.Errorf("Full accounting = %v, want raw=filtered=new=4 skipped=0", full)
	}
	if full["fetch_failed"] != false {
		t.Errorf("Full accounting fetch_failed = %v, want false", full["fetch_failed"])
	}
	empty := byFeed["Empty"]
	if int(empty["raw"].(float64)) != 0 || int(empty["new"].(float64)) != 0 {
		t.Errorf("Empty accounting = %v, want raw=0 new=0", empty)
	}
	if empty["fetch_failed"] != false {
		t.Errorf("Empty accounting fetch_failed = %v, want false", empty["fetch_failed"])
	}
	dead := byFeed["Dead"]
	if int(dead["raw"].(float64)) != 0 || int(dead["new"].(float64)) != 0 {
		t.Errorf("Dead accounting = %v, want raw=0 new=0", dead)
	}
	if dead["fetch_failed"] != true {
		t.Errorf("Dead accounting fetch_failed = %v, want true", dead["fetch_failed"])
	}
}

// The accounting record must never carry the raw transport error: fetch
// failures embed the full request URL, credentials in query parameters
// included. A feed URL with a token exercises the actual failed-fetch path
// (connection refused), and the accounting record may report the failure
// only as a boolean.
func TestRunFeedAccountingOmitsFetchErrorDetails(t *testing.T) {
	cfg := testConfig(t,
		config.Feed{Name: "Leaky", URL: "http://127.0.0.1:1/feed?token=SECRET"},
	)
	lines := runLines(t, cfg)

	accounts := linesWith(lines, "feed accounting")
	if len(accounts) != 1 {
		t.Fatalf("feed accounting lines = %d, want 1: %v", len(accounts), accounts)
	}
	a := accounts[0]
	if a["feed"] != "Leaky" || a["fetch_failed"] != true {
		t.Errorf("accounting = %v, want feed=Leaky fetch_failed=true", a)
	}
	for _, field := range []string{"fetch_error", "error", "err", "url"} {
		if _, present := a[field]; present {
			t.Errorf("accounting carries %q field: %v", field, a)
		}
	}
	// The decisive check: the accounting record's JSON must not contain the
	// credential, in any field, however the error text happens to render.
	for _, l := range accounts {
		encoded, err := json.Marshal(l)
		if err != nil {
			t.Fatalf("re-encode accounting line: %v", err)
		}
		if bytes.Contains(encoded, []byte("SECRET")) {
			t.Errorf("accounting record leaks the URL credential: %s", encoded)
		}
	}
}

// A second run over the same feeds accounts them with new=0: every item is
// already scored, so the feeds contribute candidates to no batch and the
// run reports no scoring progress at all.
func TestRunLogsFeedAccountingForAlreadyScoredFeeds(t *testing.T) {
	cfg := testConfig(t,
		config.Feed{Name: "A", URL: serveRSS(t, multiItemRSS(t, "again", 3))},
	)
	runLines(t, cfg) // first run scores everything
	lines := runLines(t, cfg)

	accounts := linesWith(lines, "feed accounting")
	if len(accounts) != 1 {
		t.Fatalf("feed accounting lines = %d, want 1: %v", len(accounts), accounts)
	}
	a := accounts[0]
	if int(a["raw"].(float64)) != 3 || int(a["filtered"].(float64)) != 3 ||
		int(a["new"].(float64)) != 0 || int(a["skipped"].(float64)) != 3 {
		t.Errorf("accounting = %v, want raw=filtered=3 new=0 skipped=3", a)
	}
	if got := linesWith(lines, "run scoring progress"); len(got) != 0 {
		t.Errorf("run scoring progress lines = %d, want 0 with no candidates: %v", len(got), got)
	}
	if got := linesWith(lines, "scoring progress"); len(got) != 0 {
		t.Errorf("scoring progress lines = %d, want 0 with no candidates: %v", len(got), got)
	}
}

// The effective inference settings are logged once per run with every field
// the run will actually use.
func TestRunLogsInferenceSettingsOnce(t *testing.T) {
	cfg := testConfig(t,
		config.Feed{Name: "A", URL: serveRSS(t, multiItemRSS(t, "settings", 2))},
	)
	lines := runLines(t, cfg)

	settings := linesWith(lines, "inference settings")
	if len(settings) != 1 {
		t.Fatalf("inference settings lines = %d, want 1: %v", len(settings), settings)
	}
	s := settings[0]
	if s["level"] != "info" {
		t.Errorf("level = %v, want info", s["level"])
	}
	want := map[string]any{
		"provider":         "heuristic",
		"model":            "test-model",
		"batch_size":       float64(2),
		"max_parallel":     float64(2),
		"think":            false,
		"adaptive_ranking": true, // unset means on
	}
	for k, v := range want {
		if s[k] != v {
			t.Errorf("inference settings %s = %v, want %v", k, s[k], v)
		}
	}
}
