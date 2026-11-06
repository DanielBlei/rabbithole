// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"regexp"
	"strings"
	"testing"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/store"
)

func TestManageRowCarriesItsTick(t *testing.T) {
	row := statRow("Busy", store.SourceStat{})
	row.OwnTagsAttr = "ai,go"
	body := renderItem(t, row)

	wantAttrs(t, "unit", tagWith(t, body, "id", "src-"+row.ID), map[string]string{"data-own-tags": "ai,go"})
	wantAttrs(t, "tick", tagWith(t, body, "id", "pick-"+row.ID), map[string]string{
		"type": "checkbox", "value": row.ID, "aria-label": "Select Busy", "data-src-pick": present,
	})
	if !strings.Contains(body, `mg-c mg-c--chk`) {
		t.Errorf("no tick cell; body=%s", body)
	}

	gone := statRow("Gone", store.SourceStat{})
	gone.Deleted = true
	wantAttrs(
		t,
		"removed tick",
		tagWith(t, renderItem(t, gone), "id", "pick-"+gone.ID),
		map[string]string{"data-src-pick": present},
	)
}

func TestManageHeaderAndBulkBar(t *testing.T) {
	w, _ := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{
		{Name: "Alpha", URL: "https://alpha.test/feed", Tags: []string{"ai"}},
		{Name: "Beta", URL: "https://beta.test/feed"},
	}})
	page := get(t, w, "/sources/manage")

	wantAttrs(t, "select all", tagWith(t, page, "id", "pick-all"), map[string]string{
		"type": "checkbox", "aria-label": "Select all listed sources", "data-src-all": present,
	})
	if !strings.Contains(page, `<label class="mg-th-narrow" for="pick-all">Select all</label>`) {
		t.Errorf("no narrow select-all label; body=%s", page)
	}
	if n := strings.Count(page, `data-src-pick`); n != 2 {
		t.Errorf("%d row ticks, want 2", n)
	}
	// The data attribute of the tagged source lists its own tags only.
	wantAttrs(t, "own tags", tagWith(t, page, "data-name", "Alpha"), map[string]string{"data-own-tags": "ai"})
	wantAttrs(t, "no own tags", tagWith(t, page, "data-name", "Beta"), map[string]string{"data-own-tags": ""})

	wantAttrs(t, "bar", tagWith(t, page, "aria-label", "Bulk actions"), map[string]string{
		"role": "toolbar", "data-src-bulk": present, "hidden": present,
	})
	for action, label := range map[string]string{
		"enable": "Enable", "disable": "Disable", "addtag": "Add", "remove": "Remove", "restore": "Restore",
	} {
		tag := regexp.MustCompile(`<button[^<>]*hx-vals='\{"action":"` + action + `"\}'[^<>]*>`).FindString(page)
		if tag == "" {
			t.Errorf("no %s button", action)
			continue
		}
		wantAttrs(t, action, parseTag(tag), map[string]string{
			"hx-post": "/sources/bulk", "hx-swap": "none", "data-src-bulk-act": present,
		})
		if !strings.Contains(page, ">"+label+"</button>") {
			t.Errorf("no %q button", label)
		}
	}
	export := regexp.MustCompile(`<button[^<>]*\sdata-src-bulk-export[^<>]*>`).FindString(page)
	wantAttrs(t, "export", parseTag(export), map[string]string{"hx-get": "/sources/export", "hx-target": "#modalTop"})
	wantAttrs(
		t,
		"tag input",
		tagWith(t, page, "aria-label", "Tag to add"),
		map[string]string{"placeholder": "Tag name"},
	)
	for _, want := range []string{`>Add tag</button>`, `>Export selected</button>`, `>Clear</button>`, `data-src-bulk-n`} {
		if !strings.Contains(page, want) {
			t.Errorf("bulk bar lacks %q", want)
		}
	}
}
