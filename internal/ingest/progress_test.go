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
