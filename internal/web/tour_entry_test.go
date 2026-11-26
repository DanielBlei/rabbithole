// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"strings"
	"testing"

	"github.com/DanielBlei/rabbithole/internal/store"
)

// The Basics group in Settings carries a row that starts the tour. It is a
// plain button outside the draft: no field name, so the save bar never sees it.
func TestSettingsBasicsHasTourRow(t *testing.T) {
	body := get(t, newTestWeb(t), "/settings/general")

	start := strings.Index(body, "data-stp-tour")
	if start < 0 {
		t.Fatal("settings general pane missing the tour row")
	}
	rest := body[start:]
	marker := strings.Index(rest, "data-tour-start")
	if marker < 0 {
		t.Fatalf("tour row has no data-tour-start button; row=%s", rest[:200])
	}
	// The button element, from its opening tag to its close.
	open := strings.LastIndex(rest[:marker], "<button")
	end := strings.Index(rest[marker:], "</button>")
	if open < 0 || end < 0 {
		t.Fatalf("tour button markup not found; row=%s", rest[:200])
	}
	button := rest[open : marker+end]

	if !strings.Contains(button, `type="button"`) {
		t.Errorf("tour button is not type=button; button=%s", button)
	}
	if strings.Contains(button, "data-fld") {
		t.Errorf("tour button joins the settings draft; button=%s", button)
	}
	if !strings.Contains(body, "Replay the six-step tour.") {
		t.Error("tour row hint does not describe the six-step tour")
	}
}

// The empty feed's sleepy bunny and the tour action appear for the first-run
// and dry states, and not for a filtered-empty feed. The block is sliced out
// first because the welcome dialog carries the same markers.
func TestFeedEmptySleepyBunny(t *testing.T) {
	feedBlock := func(t *testing.T, body string) string {
		t.Helper()
		start := strings.Index(body, `class="feed-empty"`)
		if start < 0 {
			t.Fatal("feed has no zero-state block")
		}
		rest := body[start:]
		end := strings.Index(rest, "</section>")
		if end < 0 {
			t.Fatal("zero-state block is not closed by the pane")
		}
		return rest[:end]
	}

	dryWeb := func(t *testing.T) *Web {
		t.Helper()
		w := newEmptyWeb(t)
		id, err := w.db.StartIngestRun(context.Background(), store.IngestTriggerManual)
		if err != nil {
			t.Fatalf("StartIngestRun: %v", err)
		}
		if err := w.db.FinishIngestRun(
			context.Background(),
			id,
			store.IngestStatusOK,
			store.IngestCounts{},
			"",
		); err != nil {
			t.Fatalf("FinishIngestRun: %v", err)
		}
		return w
	}

	tests := []struct {
		name string
		web  func(t *testing.T) *Web
		path string
		want bool
	}{
		{name: "never ingested", web: newEmptyWeb, path: "/feed", want: true},
		{name: "after an empty run", web: dryWeb, path: "/feed", want: true},
		{name: "filtered empty", web: newTestWeb, path: "/feed?view=1", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			block := feedBlock(t, get(t, tt.web(t), tt.path))
			for _, marker := range []string{"(u_u)", "data-tour-start"} {
				if got := strings.Contains(block, marker); got != tt.want {
					t.Errorf("zero-state contains %q = %v, want %v; block=%s", marker, got, tt.want, block)
				}
			}
		})
	}
}
