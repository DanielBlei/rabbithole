// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"html"
	"strings"
	"testing"

	"github.com/DanielBlei/rabbithole/internal/config"
)

// hintMarkup is the "?" button and bubble the Sources pages render for key.
func hintMarkup(key string) string {
	tv := tipFor(key)
	return `<button type="button" class="hint" aria-describedby="` + tv.ID +
		`" aria-label="More about this" data-hint>?</button><span class="hint__tip" id="` + tv.ID +
		`" role="tooltip">` + html.EscapeString(tv.Text) + `</span>`
}

// A moved explanation is a bubble beside its title and appears once, so it is
// no longer visible text under the header.
func assertBubble(t *testing.T, what, body, key string) {
	t.Helper()
	if !strings.Contains(body, hintMarkup(key)) {
		t.Errorf("%s lacks the bubble %q", what, key)
	}
	if n := strings.Count(body, html.EscapeString(tipFor(key).Text)); n != 1 {
		t.Errorf("%s shows the %q text %d times, want 1", what, key, n)
	}
	if n := strings.Count(body, `id="`+tipFor(key).ID+`"`); n != 1 {
		t.Errorf("%s has the id of %q %d times, want 1", what, key, n)
	}
}

func TestSourcesOverviewMovesItsExplanationsBehindBubbles(t *testing.T) {
	w, _ := overviewSectionsWeb(t)
	body := get(t, w, "/sources")
	for _, key := range []string{"src-ingest", "src-volume", "src-signal"} {
		assertBubble(t, "overview", body, key)
	}
	for _, id := range []string{"ovSync", "ovVol", "ovSig"} {
		if !strings.Contains(body, `aria-labelledby="`+id+`"`) || !strings.Contains(body, `id="`+id+`">`) {
			t.Errorf("section %s lost its label", id)
		}
	}
	// The short Signal line stays as the key text beside its title.
	mustContain(t, "overview", body, `id="ovSig">Signal</span>`+hintMarkup("src-signal")+`</h3>
      <p>How many scored items are 7 or higher, per source.</p></div>`)
	mustNotContain(t, "overview", body, `<p class="sh-hint ov-more">Last 30 days.`)
}

func TestSourcesKeyTextsStayVisible(t *testing.T) {
	w, _ := sourcesTestWeb(t, config.FeedsDoc{})
	body := get(t, w, "/sources")
	mustContain(t, "first run", body, "Add a feed you already read, or start from the Library")
	mustNotContain(t, "first run", body, `data-hint`)
}

func TestIngestionPageBubbles(t *testing.T) {
	w := ingestionWeb(t)
	body := get(t, w, "/ingestion")
	assertBubble(t, "ingestion", body, "src-ingest")
	assertBubble(t, "ingestion", body, "src-runs")
	mustContain(
		t,
		"ingestion",
		body,
		`<h1><span>Ingestion</span><button type="button" class="hint"`,
		`id="inRuns">Recent runs</span>`,
	)
}

func TestSourcesDialogsKeepTheirIntroInABubble(t *testing.T) {
	w, _ := importTestWeb(t)
	imp := get(t, w, "/sources/import")
	assertBubble(t, "import dialog", imp, "src-import")
	mustContain(t, "import dialog", imp, `<span class="modal__title">Import sources<button`)
	mustNotContain(t, "import dialog", imp, `class="mg-imp__p"`)

	def := get(t, w, "/sources/defaults")
	assertBubble(t, "defaults dialog", def, "src-defaults")
	mustNotContain(t, "defaults dialog", def, `sdef__intro`)
}

func TestSourceTipsAreOnAPage(t *testing.T) {
	w, _ := overviewSectionsWeb(t)
	pages := strings.Join([]string{
		get(t, w, "/sources"), get(t, w, "/ingestion"),
		get(t, w, "/sources/import"), get(t, w, "/sources/defaults"),
	}, "\n")
	for key := range sourceTips {
		if _, clash := tips[key]; clash {
			t.Errorf("tip %q is in both maps", key)
		}
		if !strings.Contains(pages, `id="`+tipFor(key).ID+`" role="tooltip"`) {
			t.Errorf("source tip %q is on no Sources page", key)
		}
	}
}

// The Settings bubble keeps its wording, and the behaviour is one shared script.
func TestSettingsHintMarkupIsUnchanged(t *testing.T) {
	w, _ := configTestWeb(t)
	body := get(t, w, "/settings/general")
	tv := tipFor("feeds")
	want := `<button type="button" class="hint" aria-describedby="` + tv.ID +
		`" aria-label="More about this setting" data-hint>?</button><span class="hint__tip" id="` + tv.ID + `" role="tooltip">`
	if !strings.Contains(body, want) {
		t.Errorf("the Settings bubble markup changed")
	}
}

func TestHintScriptIsSharedAndServed(t *testing.T) {
	w, _ := configTestWeb(t)
	for _, path := range []string{"/sources", "/settings/general", "/"} {
		if !strings.Contains(get(t, w, path), `/static/js/hint.js`) {
			t.Errorf("%s does not load hint.js", path)
		}
	}
	js := get(t, w, "/static/js/hint.js")
	for _, want := range []string{"data-hint", "Escape", "is-open"} {
		if !strings.Contains(js, want) {
			t.Errorf("hint.js lacks %q", want)
		}
	}
	if strings.Contains(get(t, w, "/static/js/settings.js"), "data-hint") {
		t.Error("settings.js still carries the bubble script")
	}
}
