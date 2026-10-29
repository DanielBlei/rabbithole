// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"bytes"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/store"
)

func renderItem(t *testing.T, row feedRowData) string {
	t.Helper()
	var b bytes.Buffer
	if err := feedTmpl.ExecuteTemplate(&b, "sourceItem", row); err != nil {
		t.Fatalf("render sourceItem: %v", err)
	}
	return b.String()
}

func optionValues(d listboxData) (values, labels []string) {
	for _, o := range d.Options {
		values = append(values, o.Value)
		labels = append(labels, o.Label)
	}
	return values, labels
}

func TestManageRowDataAttributes(t *testing.T) {
	last := time.Now().Add(-3 * time.Hour)
	st := scoredStat(20, 5)
	st.Weeks = [8]int{0, 0, 0, 0, 0, 0, 8, 16}
	st.Last = last
	row := statRow("Busy", st)
	wantAttrs(t, "enough", tagWith(t, renderItem(t, row), "id", "src-"+row.ID), map[string]string{
		"data-perweek": "3",
		"data-signal":  "25",
		"data-last":    strconv.FormatInt(last.Unix(), 10),
	})

	empty := statRow("Idle", store.SourceStat{})
	wantAttrs(t, "empty", tagWith(t, renderItem(t, empty), "id", "src-"+empty.ID), map[string]string{
		"data-perweek": "0",
		"data-signal":  "",
		"data-last":    "",
	})
}

func TestManageRowAttributesFromStoredItems(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{
		{Name: "Alpha", URL: "https://alpha.test/feed"},
		{Name: "Beta", URL: "https://beta.test/feed"},
	}})
	itemAdder(t, db)("Alpha")
	page := get(t, w, "/sources/manage")
	wantAttrs(t, "Alpha", tagWith(t, page, "data-name", "Alpha"), map[string]string{
		"data-perweek": "0.125", "data-signal": "", "data-last": present,
	})
	if got := tagWith(t, page, "data-name", "Alpha")["data-last"]; got == "" {
		t.Error("a source with an item has no data-last")
	}
	wantAttrs(t, "Beta", tagWith(t, page, "data-name", "Beta"), map[string]string{
		"data-perweek": "0", "data-signal": "", "data-last": "",
	})
}

func TestManageColumnCells(t *testing.T) {
	never := feedRowData{ID: "n", Name: "Never", URL: "https://n.test/feed", State: feedStateNever}
	never.applyStats(store.SourceStat{}, store.FeedHealth{}, time.Now())
	body := renderItem(t, never)
	for _, want := range []string{
		`<span class="mg-k">Stored / week</span>`, `none yet`, `<span class="mg-k">Signal</span>`, `No data yet`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("never: missing %q; body=%s", want, body)
		}
	}
	if strings.Contains(body, `class="mg-sig"`) || strings.Contains(body, `class="mg-vol"`) {
		t.Errorf("never: shows figures; body=%s", body)
	}

	thin := scoredStat(6, 3)
	thin.Unscored = 2
	thin.Weeks = [8]int{0, 0, 0, 0, 0, 0, 0, 8}
	body = renderItem(t, statRow("Thin", thin))
	for _, want := range []string{
		`Not enough data`, `title="Only 6 scored so far (needs 10), 2 unscored"`, `no items`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("not enough: missing %q; body=%s", want, body)
		}
	}
	if strings.Contains(body, `class="mg-sig"`) || strings.Contains(body, `class="tier`) {
		t.Errorf("not enough: shows a share; body=%s", body)
	}

	rich := scoredStat(20, 5)
	rich.Unscored = 4
	rich.Mid = 5
	rich.Low = 10
	rich.Weeks = [8]int{0, 0, 0, 0, 0, 0, 8, 16}
	rich.Last = time.Now().Add(-3 * time.Hour)
	body = renderItem(t, statRow("Rich", rich))
	for _, want := range []string{
		`<b>3</b><span class="mg-sub2" title="last item 3h ago">last item 3h ago</span>`,
		`<span class="mg-sig" title="5 of 20 scored are 7&#43;, 4 unscored"><b>25%</b><span class="tier"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("enough: missing %q; body=%s", want, body)
		}
	}
}

func TestManageHeaderAndEmptyCopy(t *testing.T) {
	w, _ := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}}})
	page := get(t, w, "/sources/manage")
	for _, want := range []string{
		`<span class="mg-c mg-c--vol" title="Items stored per week, 8-week average">Stored / week</span>`,
		`<span class="mg-c mg-c--sig" title="Share of scored items rated 7 or higher, last 30 days">Signal</span>`,
		`No source matches your search or tag.`,
		`data-src-clearall>Clear search and tag</button>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("missing %q; body=%s", want, page)
		}
	}
}

func TestManageTagMenu(t *testing.T) {
	row := func(name string, deleted bool, tags ...string) feedRowData {
		return feedRowData{Name: name, Tags: tags, Deleted: deleted}
	}
	d := sourcesData{Rows: []feedRowData{
		row("A", false, "Go", "news"),
		row("B", false, "go", "Tech", "TECH"),
		row("C", false, "NEWS"),
		row("D", true, "gone", "go"),
	}}
	values, labels := optionValues(d.TagMenu())
	if want := []string{"", "Go", "news", "Tech"}; !reflect.DeepEqual(values, want) {
		t.Errorf("values = %q, want %q", values, want)
	}
	if want := []string{"All tags", "Go (2)", "news (2)", "Tech (1)"}; !reflect.DeepEqual(labels, want) {
		t.Errorf("labels = %q, want %q", labels, want)
	}
	menu := d.TagMenu()
	if menu.ID != "srcTagMenu" || menu.Name != "srctag" || menu.OOB || menu.On() {
		t.Errorf("menu = %+v", menu)
	}
	d.Counts.OOB = true
	if !d.TagMenu().OOB {
		t.Error("an out-of-band render does not mark the menu")
	}
}

func TestManageSortMenu(t *testing.T) {
	values, labels := optionValues(sourcesData{}.SortMenu())
	if want := []string{"name", "items", "signal", "last"}; !reflect.DeepEqual(values, want) {
		t.Errorf("values = %q, want %q", values, want)
	}
	if want := []string{"Name", "Stored items per week", "Signal", "Last item"}; !reflect.DeepEqual(labels, want) {
		t.Errorf("labels = %q, want %q", labels, want)
	}
}

// The button shows the bare tag name; the list keeps the count.
func TestManageTagMenuButtonText(t *testing.T) {
	d := sourcesData{Rows: []feedRowData{{Tags: []string{"AI"}}}}
	menu := d.TagMenu()
	menu.Value = "AI"
	if got := menu.Current(); got != "AI" {
		t.Errorf("button text = %q, want AI", got)
	}
	if got := menu.Options[1].Label; got != "AI (1)" {
		t.Errorf("option label = %q, want AI (1)", got)
	}
}

func TestManageMenusInPageAndMutation(t *testing.T) {
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: []config.Feed{
		{Name: "Alpha", URL: "https://alpha.test/feed", Tags: []string{"Go"}},
		{Name: "Beta", URL: "https://beta.test/feed", Tags: []string{"go", "ai"}},
	}})
	page := get(t, w, "/sources/manage")
	for _, id := range []string{"srcTagMenu", "srcSortMenu"} {
		if n := strings.Count(page, `id="`+id+`"`); n != 1 {
			t.Errorf("#%s appears %d times on the page, want 1", id, n)
		}
	}
	lacksAttr(t, "page tag menu", tagWith(t, page, "id", "srcTagMenu"), "hx-swap-oob")
	for _, want := range []string{`data-value="Go" data-label="Go"`, `data-value="ai" data-label="ai"`, `<span class="stp-menu__n">Go (2)</span>`} {
		if !strings.Contains(page, want) {
			t.Errorf("page missing %q", want)
		}
	}

	body := postForm(t, w, "/sources/feed/"+feedID(t, db, "Beta")+"/enabled", url.Values{"enabled": {"off"}})
	if n := strings.Count(body, `id="srcTagMenu"`); n != 1 {
		t.Fatalf("a mutation carried the tag menu %d times, want 1; body=%s", n, body)
	}
	wantAttrs(t, "mutation tag menu", tagWith(t, body, "id", "srcTagMenu"), map[string]string{"hx-swap-oob": "true"})
	if strings.Contains(body, `id="srcSortMenu"`) {
		t.Errorf("a mutation resent the sort menu; body=%s", body)
	}
}
