// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"strings"
	"testing"
)

func TestFeedBulkBar(t *testing.T) {
	w := newTestWeb(t)
	page := get(t, w, "/feed")

	if !strings.Contains(page, `class="row__check"`) {
		t.Errorf("feed page missing row__check; body=%s", page)
	}
	if !strings.Contains(page, `data-bulk-check`) {
		t.Errorf("feed page missing data-bulk-check; body=%s", page)
	}
	wantAttrs(t, "select-all", tagWith(t, page, "aria-label", "Select all visible rows"), map[string]string{
		"type": "checkbox", "data-bulk-select-all": present,
	})
	if !strings.Contains(page, `data-bulk-selectbar`) {
		t.Errorf("feed page missing data-bulk-selectbar; body=%s", page)
	}
	wantAttrs(t, "bulk bar", tagWith(t, page, "aria-label", "Bulk actions"), map[string]string{
		"role": "toolbar", "data-bulk-bar": present, "hidden": present,
	})
	for _, action := range []string{"bookmark", "seen", "hide", "unhide"} {
		tag := tagWith(t, page, "data-bulk-action", action)
		if tag == nil {
			t.Errorf("no bulk button for %q", action)
			continue
		}
		wantAttrs(t, action, tag, map[string]string{
			"data-bulk-action": action,
		})
	}
	if !strings.Contains(page, `data-bulk-status`) {
		t.Errorf("feed page missing data-bulk-status; body=%s", page)
	}
	wantAttrs(t, "bulk status", tagWith(t, page, "role", "status"), map[string]string{
		"data-bulk-status": present, "aria-live": "polite",
	})
	if !strings.Contains(page, `data-bulk-count`) {
		t.Errorf("feed page missing data-bulk-count; body=%s", page)
	}
}

func TestFeedBulkBarHiddenWhenEmpty(t *testing.T) {
	w := newTestWeb(t)
	page := get(t, w, "/feed")
	if !strings.Contains(page, `data-bulk-bar`) {
		t.Fatalf("no bulk bar; body=%s", page)
	}
	barTag := tagWith(t, page, "aria-label", "Bulk actions")
	if _, ok := barTag["hidden"]; !ok {
		t.Errorf("bulk bar should be hidden initially; attrs=%v", barTag)
	}
}

func TestFeedRowCarriesBulkTick(t *testing.T) {
	w := newTestWeb(t)
	page := get(t, w, "/feed")
	if !strings.Contains(page, `data-bulk-check`) {
		t.Fatalf("no bulk tick; body=%s", page)
	}
	if n := strings.Count(page, `data-bulk-check`); n != 1 {
		t.Errorf("%d bulk ticks, want 1 (one seeded item)", n)
	}
	if !strings.Contains(page, `data-id="a"`) {
		t.Errorf("row missing data-id; body=%s", page)
	}
	if !strings.Contains(page, `aria-label="Select A"`) {
		t.Errorf("row checkbox missing aria-label; body=%s", page)
	}
}
