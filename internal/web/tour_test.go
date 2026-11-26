// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"bytes"
	"html"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/DanielBlei/rabbithole/internal/store"
)

func TestTourCardRendersOnLayoutPages(t *testing.T) {
	w := newTestWeb(t)
	for _, path := range []string{"/feed", "/maze", "/sources", "/settings/general"} {
		t.Run(path, func(t *testing.T) {
			_, body := getBody(t, w, path)
			for _, want := range []string{`id="tourCard"`, `data-tour-step="feed"`, `data-tour-step="sources"`, `data-tour-step="ingest"`, `data-tour-step="explain"`, `data-tour-step="demo"`, `data-tour-step="done"`, `role="dialog"`, `id="tourMask"`} {
				if !strings.Contains(body, want) {
					t.Errorf("%s: tour missing %q", path, want)
				}
			}
		})
	}

	// The gate pages use their own template set and must not carry the card.
	gated := gatedWeb(t)
	page := newBrowser(t, gated).get("/login")
	wantStatus(t, page, http.StatusOK)
	if strings.Contains(page.Body.String(), `id="tourCard"`) {
		t.Error("the login page renders the tour card")
	}
}

// A page the tour rows can point into: the path the browser would show and the
// markup the server renders for it.
type tourPage struct {
	path string
	body string
}

// The tour selects real controls by class, id and attribute. Every token of
// every selector in tour.html must exist in a page the row applies to, so a
// rename in a template breaks here and not silently in the browser.
func TestTourSelectorsExist(t *testing.T) {
	run := ingestBodyData{
		ShowBanner: true,
		Last:       &ingestRunView{Status: "ok", Counts: store.IngestCounts{Fetched: 3, NewItems: 2, Scored: 1}},
		Lines:      []ingestLogLine{{Time: "10:00:00", Level: "INF", Class: "inf", Msg: "ingest complete"}},
	}
	failed := ingestBodyData{
		Last:  &ingestRunView{Status: "error", Error: "model not found"},
		Lines: run.Lines,
	}
	render := func(data ingestBodyData) string {
		var buf bytes.Buffer
		if err := feedTmpl.ExecuteTemplate(&buf, "ingestBody", data); err != nil {
			t.Fatalf("render ingestBody: %v", err)
		}
		return buf.String()
	}
	pages := []tourPage{
		{"/feed", get(t, newTestWeb(t), "/feed")},
		{"/sources", get(t, newEmptyWeb(t), "/sources")},
		{"/ingest", get(t, newTestWeb(t), "/ingest")},
		{"/ingest", render(run)},
		{"/ingest", render(failed)},
	}

	_, feed := getBody(t, newTestWeb(t), "/feed")
	card := tourCardRe.FindString(feed)
	rows := tourRowRe.FindAllString(card, -1)
	if len(rows) == 0 {
		t.Fatal("tour has no routing rows")
	}
	for _, row := range rows {
		attrs := parseTag(row)
		for _, name := range []string{"data-ring", "data-ring-also", "data-via", "data-point"} {
			for _, sel := range splitSelectors(attrs[name]) {
				t.Run(attrs["data-path"]+" "+name+" "+sel, func(t *testing.T) {
					// The demo row and banner are made by the server's demo run, which
					// has its own tests; the card must name them literally.
					if strings.Contains(sel, "[data-demo]") {
						if !strings.Contains(row, sel) {
							t.Errorf("row does not carry %q", sel)
						}
						return
					}
					var matching []string
					for _, p := range pages {
						if regexp.MustCompile(attrs["data-path"]).MatchString(p.path) {
							matching = append(matching, p.body)
						}
					}
					if len(matching) == 0 {
						t.Fatalf("no rendered page matches data-path %q", attrs["data-path"])
					}
					for _, tok := range selectorTokens(sel) {
						found := false
						for _, body := range matching {
							if tok.in(body) {
								found = true
								break
							}
						}
						if !found {
							t.Errorf("%q: %s not present in any page for path %q", sel, tok.desc, attrs["data-path"])
						}
					}
				})
			}
		}
	}
}

// splitSelectors cuts a selector list at its top-level commas.
func splitSelectors(list string) []string {
	var out []string
	depth, start := 0, 0
	for i, c := range list {
		switch c {
		case '(', '[':
			depth++
		case ')', ']':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(list[start:i]))
				start = i + 1
			}
		}
	}
	if rest := strings.TrimSpace(list[start:]); rest != "" {
		out = append(out, rest)
	}
	return out
}

type selectorToken struct {
	desc string
	re   *regexp.Regexp
}

func (k selectorToken) in(body string) bool { return k.re.MatchString(body) }

var (
	selNotRe   = regexp.MustCompile(`:not\([^)]*\)`)
	selQuoteRe = regexp.MustCompile(`"[^"]*"|'[^']*'`)
	selClassRe = regexp.MustCompile(`\.([A-Za-z_][\w-]*)`)
	selIDRe    = regexp.MustCompile(`#([A-Za-z_][\w-]*)`)
	selAttrRe  = regexp.MustCompile(`\[([\w-]+)(?:([\^$*~|]?)=(?:"([^"]*)"|'([^']*)'))?\]`)
)

// selectorTokens lists what a selector needs from the markup: its classes, ids
// and attributes (with an exact or prefix value where it gives one).
func selectorTokens(sel string) []selectorToken {
	sel = selNotRe.ReplaceAllString(sel, "")
	need := func(desc, pattern string) selectorToken {
		return selectorToken{desc, regexp.MustCompile(pattern)}
	}
	var out []selectorToken
	for _, m := range selAttrRe.FindAllStringSubmatch(sel, -1) {
		name := `\s` + regexp.QuoteMeta(m[1])
		value := regexp.QuoteMeta(html.EscapeString(m[3] + m[4]))
		switch {
		case !strings.Contains(m[0], "="):
			out = append(out, need("attribute "+m[1], name+`[\s=>]`))
		case m[2] == "^":
			out = append(out, need("attribute "+m[1]+" starting "+m[3]+m[4], name+`="`+value))
		default:
			out = append(out, need("attribute "+m[1]+"="+m[3]+m[4], name+`="`+value+`"`))
		}
	}
	bare := selQuoteRe.ReplaceAllString(selAttrRe.ReplaceAllString(sel, ""), "")
	for _, m := range selClassRe.FindAllStringSubmatch(bare, -1) {
		class := regexp.QuoteMeta(m[1])
		out = append(out, need("class "+m[1], `class="(?:[^"]*\s)?`+class+`(?:\s[^"]*)?"`))
	}
	for _, m := range selIDRe.FindAllStringSubmatch(bare, -1) {
		out = append(out, need("id "+m[1], `\sid="`+regexp.QuoteMeta(m[1])+`"`))
	}
	return out
}

// The finished-run banner carries the run's counts, so scripts never parse copy.
func TestIngestDoneCarriesCounts(t *testing.T) {
	var buf bytes.Buffer
	data := ingestBodyData{
		ShowBanner: true,
		Last:       &ingestRunView{Status: "ok", Counts: store.IngestCounts{Fetched: 7, NewItems: 5, Scored: 3}},
	}
	if err := feedTmpl.ExecuteTemplate(&buf, "ingestBody", data); err != nil {
		t.Fatalf("render ingestBody: %v", err)
	}
	wantAttrs(t, "done banner", tagWith(t, buf.String(), "class", "ing__done"), map[string]string{
		"data-fetched": "7", "data-new": "5", "data-scored": "3",
	})
}

var (
	tourCardRe = regexp.MustCompile(`(?s)id="tourCard".*?</aside>`)
	tourRowRe  = regexp.MustCompile(`<span[^>]*\sdata-tour-on[^>]*>`)
	// step, number, and the section's other data-tour-* attributes (reveal,
	// advance, when) in document order
	tourSectionRe = regexp.MustCompile(
		`<section class="tour__step" data-tour-step="(\w+)" data-tour-n="(\d+)"` +
			`((?: data-tour-\w+="[^"]*")*) hidden>`,
	)
	tourWhenRe = regexp.MustCompile(`data-tour-when="([^"]*)"`)
)

func TestTourRoutingRows(t *testing.T) {
	_, page := getBody(t, newTestWeb(t), "/feed")
	tour := tourCardRe.FindString(page)
	if tour == "" {
		t.Fatal("no tour card on /feed")
	}

	rows := tourRowRe.FindAllString(tour, -1)
	if len(rows) == 0 {
		t.Fatal("tour has no routing rows")
	}
	for i, row := range rows {
		attrs := parseTag(row)
		if _, ok := attrs["data-path"]; !ok {
			t.Errorf("row %d has no data-path: %s", i, row)
		}
		for _, gone := range []string{"data-next", "data-avoid", "data-advance", "data-expect"} {
			if _, ok := attrs[gone]; ok {
				t.Errorf("row %d uses the removed %s: %s", i, gone, row)
			}
		}
		to, hasTo := attrs["data-to"]
		if hasTo && to != "sources" && to != "ingest" && to != "explain" && to != "demo" && to != "done" {
			t.Errorf("row %d moves to an unknown step %q: %s", i, to, row)
		}
		if _, hasVia := attrs["data-via"]; hasVia && !hasTo {
			t.Errorf("row %d has data-via without data-to: %s", i, row)
		}
	}

	if strings.Contains(tour, "data-tour-total") {
		t.Error("the total is derived from the step numbers, not stored on the card")
	}
	// Every section, in document order: the step, its number and its condition.
	want := []string{
		"feed 1 ", "sources 2 ", "ingest 3 ",
		"explain 4 runner-open",
		"demo 5 run-failed", "demo 5 demo-done", "demo 5 running", "demo 5 ",
		"done 6 demo-row", "done 6 ",
	}
	var got []string
	for _, m := range tourSectionRe.FindAllStringSubmatch(tour, -1) {
		when := ""
		if w := tourWhenRe.FindStringSubmatch(m[3]); w != nil {
			when = w[1]
		}
		got = append(got, m[1]+" "+m[2]+" "+when)
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("sections:\n got %q\nwant %q", got, want)
	}
	if n := strings.Count(tour, "data-tour-count"); n != len(want) {
		t.Errorf("%d sections show a counter, want %d", n, len(want))
	}
	for _, name := range []string{
		"data-tour-next", "data-tour-run", "data-tour-finish", "data-tour-end", "data-tour-primary",
		`hx-post="/ingest/run?scorer=demo"`, `hx-target="#ingestBody"`, `hx-swap="outerHTML"`,
		`data-ring=".sh-nav"`, `data-ring="#modal .ing__nav"`, `data-ring="#modal .ing__go"`, `data-ring=".row[data-demo]"`,
		`data-ring=".topbar__nav .navgrp"`, `data-ring="#ingChip"`, `data-ring-also="#navIngest"`,
		`data-via='.topbar__nav a[href="/feed"]'`, `data-via='.topbar__nav a[href="/sources"]'`,
		`data-tour-reveal="topnav rail"`, `data-tour-advance="runner-open"`,
	} {
		if !strings.Contains(tour, name) {
			t.Errorf("card has no %s", name)
		}
	}
	for _, gone := range []string{"scorer=keywords", "data-tour-trial", "tour__try", "tour__label"} {
		if strings.Contains(tour, gone) {
			t.Errorf("card still carries %q", gone)
		}
	}
	for _, copy := range []string{
		"Your feed", "Your sources", "Run ingestion", "Ingestion", "Try it on a demo feed", "Done",
		"Scored stories land here. To add feeds, open Sources: under Feed up top, or in the side menu.",
		"Ingestion fills your feed. Start a run from Ingestion up top, or ingest in the side menu.",
		"Running: fetch, score, store.", "The demo run failed; the runner shows why.",
		"Run the demo", "Run the demo again", "Show my feed", "Finish", "Open Library", "Open Scoring",
		"That&#39;s your first scored story.", "That&#39;s the tour.",
	} {
		if !strings.Contains(tour, copy) && !strings.Contains(tour, strings.ReplaceAll(copy, "&#39;", "'")) {
			t.Errorf("card has no copy %q", copy)
		}
	}
	// One primary per section, after End tour in tab order only because End tour
	// is moved to the left by the stylesheet.
	if n := strings.Count(tour, "data-tour-primary"); n != len(want) {
		t.Errorf("%d primaries, want one per section (%d)", n, len(want))
	}
	if strings.Count(tour, "tour__bunny") != 3 {
		t.Error("the bunny belongs on the feed step and the two Done cards only")
	}
}

// The card says whether the install is clean, so a position stored for an
// earlier database can be told apart from a live tour.
func TestTourCardFreshMarker(t *testing.T) {
	_, fresh := getBody(t, newEmptyWeb(t), "/feed")
	if !strings.Contains(tourCardRe.FindString(fresh), "data-tour-fresh") {
		t.Error("a fresh install does not mark the tour card")
	}

	dismissed := newEmptyWeb(t)
	if code := postFormCode(dismissed, "/welcome/dismissed", nil); code != http.StatusNoContent {
		t.Fatalf("POST /welcome/dismissed = %d", code)
	}
	_, after := getBody(t, dismissed, "/feed")
	if strings.Contains(tourCardRe.FindString(after), "data-tour-fresh") {
		t.Error("the tour card is marked fresh after the dismissal")
	}

	_, items := getBody(t, newTestWeb(t), "/feed")
	if strings.Contains(tourCardRe.FindString(items), "data-tour-fresh") {
		t.Error("the tour card is marked fresh with items present")
	}
}

// The tour has no keyboard shortcut of its own: Escape and Arrow Right work
// from inside the card.
func TestTourHasNoAltT(t *testing.T) {
	_, page := getBody(t, newTestWeb(t), "/feed")
	tour := tourCardRe.FindString(page)
	if tour == "" {
		t.Fatal("no tour card on /feed")
	}
	for _, gone := range []string{"Alt+T", "aria-keyshortcuts", "tourCursor"} {
		if strings.Contains(tour, gone) {
			t.Errorf("tour markup still has %q", gone)
		}
	}
	if !strings.Contains(tour, `role="dialog"`) || strings.Contains(tour, `role="region"`) {
		t.Error("the card is a dialog, not a region")
	}
}
