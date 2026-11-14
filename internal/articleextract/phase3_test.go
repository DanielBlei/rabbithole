// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package articleextract

import (
	"errors"
	"strings"
	"testing"

	"golang.org/x/text/encoding/japanese"
)

func assertOrder(t *testing.T, text string, first, second string) {
	t.Helper()
	i := strings.Index(text, first)
	j := strings.Index(text, second)
	if i < 0 || j < 0 {
		t.Fatalf("missing %q or %q in %q", first, second, text)
	}
	if i >= j {
		t.Fatalf("%q should precede %q in %q", first, second, text)
	}
}

// Finding #1: the extractor used to read only its private 8 MiB bound, so a
// page allowed by the 16 MiB fetch cap was silently cut and the article after
// the old boundary disappeared.
func TestExtractReadsPastEightMiBInput(t *testing.T) {
	filler := strings.Repeat("f", 8<<20)
	page := "<html><body><script>" + filler + "</script><article>" +
		longText("Late") + "</article></body></html>"
	art, err := Extract(strings.NewReader(page), "text/html", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(art.Text, "Late sentence number 0") {
		t.Fatalf("article after the previous 8 MiB bound was lost: %q", art.Text)
	}
}

// Finding #1: input beyond the extractor's supported bound is rejected, never
// parsed as if the truncated prefix were the whole page.
func TestExtractRejectsOversizeInput(t *testing.T) {
	page := strings.Repeat("a", MaxInputBytes+1)
	_, err := Extract(strings.NewReader(page), "text/html", 0)
	if !errors.Is(err, ErrInputTooLarge) {
		t.Fatalf("err = %v, want ErrInputTooLarge", err)
	}
}

// Finding #2: a hidden article must not capture the root.
func TestExtractSkipsHiddenArticleRoot(t *testing.T) {
	page := `<html><body><article hidden>` + longText("HiddenRoot") +
		`</article><article>` + longText("VisibleRoot") + `</article></body></html>`
	art, err := Extract(strings.NewReader(page), "text/html", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if strings.Contains(art.Text, "HiddenRoot sentence") {
		t.Errorf("hidden article text retained: %q", art.Text)
	}
	if !strings.Contains(art.Text, "VisibleRoot sentence") {
		t.Errorf("visible article not selected: %q", art.Text)
	}
}

// Finding #2: a candidate whose ancestor is hidden is ineligible.
func TestExtractSkipsArticleInsideHiddenAncestor(t *testing.T) {
	page := `<html><body><div hidden><article>` + longText("HiddenAncestor") +
		`</article></div><article>` + longText("VisibleAncestor") + `</article></body></html>`
	art, err := Extract(strings.NewReader(page), "text/html", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if strings.Contains(art.Text, "HiddenAncestor sentence") {
		t.Errorf("article inside a hidden ancestor retained: %q", art.Text)
	}
	if !strings.Contains(art.Text, "VisibleAncestor sentence") {
		t.Errorf("visible article not selected: %q", art.Text)
	}
}

// Finding #2: template contents are not article candidates.
func TestExtractSkipsArticleInsideTemplate(t *testing.T) {
	page := `<html><body><template><article>` + longText("TemplateHidden") +
		`</article></template><article>` + longText("VisibleTemplate") + `</article></body></html>`
	art, err := Extract(strings.NewReader(page), "text/html", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if strings.Contains(art.Text, "TemplateHidden sentence") {
		t.Errorf("template article retained: %q", art.Text)
	}
	if !strings.Contains(art.Text, "VisibleTemplate sentence") {
		t.Errorf("visible article not selected: %q", art.Text)
	}
}

// Finding #2: navigation cards are not article candidates.
func TestExtractSkipsArticleInsideNavigation(t *testing.T) {
	page := `<html><body><nav><article>` + longText("NavHidden") +
		`</article></nav><article>` + longText("VisibleNav") + `</article></body></html>`
	art, err := Extract(strings.NewReader(page), "text/html", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if strings.Contains(art.Text, "NavHidden sentence") {
		t.Errorf("navigation article retained: %q", art.Text)
	}
	if !strings.Contains(art.Text, "VisibleNav sentence") {
		t.Errorf("visible article not selected: %q", art.Text)
	}
}

// Finding #2: when every candidate is hidden, selection continues to the body
// instead of stopping at the first unusable candidate.
func TestExtractFallsBackPastHiddenCandidates(t *testing.T) {
	page := `<html><body><article hidden>` + longText("HiddenFallback") +
		`</article><main aria-hidden="true">` + longText("AriaFallback") +
		`</main>` + longText("VisibleFallback") + `</body></html>`
	art, err := Extract(strings.NewReader(page), "text/html", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if strings.Contains(art.Text, "HiddenFallback sentence") ||
		strings.Contains(art.Text, "AriaFallback sentence") {
		t.Errorf("hidden candidate text retained: %q", art.Text)
	}
	if !strings.Contains(art.Text, "VisibleFallback sentence") {
		t.Errorf("body fallback not used: %q", art.Text)
	}
}

// Finding #2: a page whose only content is hidden yields insufficient content.
func TestExtractOnlyHiddenCandidates(t *testing.T) {
	page := `<html><body><article hidden>` + longText("HiddenOnly") +
		`</article></body></html>`
	art, err := Extract(strings.NewReader(page), "text/html", 0)
	if !errors.Is(err, ErrInsufficientContent) {
		t.Fatalf("err = %v, want ErrInsufficientContent", err)
	}
	if strings.Contains(art.Text, "HiddenOnly sentence") {
		t.Errorf("hidden content retained: %q", art.Text)
	}
}

// Finding #3: direct text children survive beside nested blocks, in order.
func TestExtractKeepsDirectProseAroundBlocks(t *testing.T) {
	page := `<html><body><article><div>Before prose marker.<p>Paragraph marker.</p>` +
		`After prose marker.</div>` + longText("Padding") + `</article></body></html>`
	art, err := Extract(strings.NewReader(page), "text/html", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	assertOrder(t, art.Text, "Before prose marker.", "Paragraph marker.")
	assertOrder(t, art.Text, "Paragraph marker.", "After prose marker.")
}

// Finding #3: inline markup is retained as text and blocks are not duplicated.
func TestExtractKeepsInlineMarkupWithBlocks(t *testing.T) {
	page := `<html><body><article><div>Start <b>bold</b> text.` +
		`<p>Para <a href="/x">link</a>.</p>End <em>em</em>.</div>` +
		longText("InlinePadding") + `</article></body></html>`
	art, err := Extract(strings.NewReader(page), "text/html", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for _, want := range []string{"Start bold text.", "Para link.", "End em."} {
		if !strings.Contains(art.Text, want) {
			t.Errorf("missing %q in %q", want, art.Text)
		}
	}
	assertOrder(t, art.Text, "Start bold text.", "Para link.")
	assertOrder(t, art.Text, "Para link.", "End em.")
}

// Finding #3: nested containers emit each piece once and preserve order.
func TestExtractNestedContainersNoDuplicates(t *testing.T) {
	page := `<html><body><article><section>Outer start.<div>Inner one.` +
		`<p>Deep marker.</p>Inner two.</div>Outer end.</section>` +
		longText("NestedPadding") + `</article></body></html>`
	art, err := Extract(strings.NewReader(page), "text/html", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for _, want := range []string{"Outer start.", "Inner one.", "Deep marker.", "Inner two.", "Outer end."} {
		if !strings.Contains(art.Text, want) {
			t.Errorf("missing %q in %q", want, art.Text)
		}
	}
	assertOrder(t, art.Text, "Outer start.", "Inner one.")
	assertOrder(t, art.Text, "Inner one.", "Deep marker.")
	assertOrder(t, art.Text, "Deep marker.", "Inner two.")
	assertOrder(t, art.Text, "Inner two.", "Outer end.")
	if got := strings.Count(art.Text, "Deep marker."); got != 1 {
		t.Errorf("Deep marker appears %d times, want once: %q", got, art.Text)
	}
}

// Finding #3: list items keep their marker and nested lists are emitted once.
func TestExtractListAndBlockquoteDirectProse(t *testing.T) {
	page := `<html><body><article><ul><li>Item one<ul><li>Nested item</li></ul></li></ul>` +
		`<blockquote>Quoted words</blockquote>` + longText("ListPadding") + `</article></body></html>`
	art, err := Extract(strings.NewReader(page), "text/html", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for _, want := range []string{"- Item one", "- Nested item", "> Quoted words"} {
		if !strings.Contains(art.Text, want) {
			t.Errorf("missing %q in %q", want, art.Text)
		}
	}
	if got := strings.Count(art.Text, "Item one"); got != 1 {
		t.Errorf("Item one appears %d times, want once: %q", got, art.Text)
	}
}

// Finding #2: a hidden heading cannot supply the title.
func TestExtractTitleSkipsHiddenHeading(t *testing.T) {
	page := `<html><head><title>Fallback title</title></head><body><article>` +
		`<h1 hidden>Hidden title</h1><h1>Visible title</h1>` +
		longText("TitleBody") + `</article></body></html>`
	art, err := Extract(strings.NewReader(page), "text/html", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if art.Title != "Visible title" {
		t.Errorf("title = %q, want the visible heading", art.Title)
	}
}

// Additional B: a substantive article after a short teaser wins.
func TestExtractPrefersLongArticleOverTeaser(t *testing.T) {
	teaser := `<article><p>` + strings.Repeat("Short teaser sentence. ", 5) + `</p></article>`
	page := `<html><body>` + teaser + `<article>` + longText("Substantive") +
		`</article></body></html>`
	art, err := Extract(strings.NewReader(page), "text/html", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(art.Text, "Substantive sentence number 0") {
		t.Fatalf("substantive article not selected: %q", art.Text)
	}
	if strings.Contains(art.Text, "Short teaser sentence.") {
		t.Errorf("teaser content retained: %q", art.Text)
	}
}

// Additional B: article cards before the primary article do not win.
func TestExtractPrefersPrimaryArticleOverCards(t *testing.T) {
	cards := strings.Repeat(`<article><p>Card summary.</p></article>`, 3)
	page := `<html><body>` + cards + `<article>` + longText("Primary") +
		`</article></body></html>`
	art, err := Extract(strings.NewReader(page), "text/html", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(art.Text, "Primary sentence number 0") {
		t.Fatalf("primary article not selected: %q", art.Text)
	}
	if strings.Contains(art.Text, "Card summary.") {
		t.Errorf("card content retained: %q", art.Text)
	}
}

// Additional B: an article inside main is preferred over main's other prose.
func TestExtractPrefersArticleInsideMain(t *testing.T) {
	page := `<html><body><main>` + longText("MainChrome") +
		`<article>` + longText("ArticleBody") + `</article></main></body></html>`
	art, err := Extract(strings.NewReader(page), "text/html", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(art.Text, "ArticleBody sentence number 0") {
		t.Fatalf("article inside main not selected: %q", art.Text)
	}
	if strings.Contains(art.Text, "MainChrome sentence") {
		t.Errorf("main chrome retained: %q", art.Text)
	}
}

// Additional B: main alone is used when there is no article.
func TestExtractMainWithoutArticle(t *testing.T) {
	page := `<html><body><main>` + longText("MainOnly") + `</main></body></html>`
	art, err := Extract(strings.NewReader(page), "text/html", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(art.Text, "MainOnly sentence number 0") {
		t.Fatalf("main content not selected: %q", art.Text)
	}
}

// Additional B: when every article is short, selection continues to the body.
func TestExtractOnlyShortCandidatesFallsBackToBody(t *testing.T) {
	page := `<html><body><article><p>Short card one.</p></article>` +
		`<article><p>Short card two.</p></article>` + longText("VisibleBody") +
		`</body></html>`
	art, err := Extract(strings.NewReader(page), "text/html", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(art.Text, "VisibleBody sentence number 0") {
		t.Fatalf("body fallback not used: %q", art.Text)
	}
}

// Additional B: only short candidates and no other prose cannot meet the guard.
func TestExtractOnlyShortCandidatesInsufficient(t *testing.T) {
	page := `<html><body><article><p>Only short one.</p></article>` +
		`<article><p>Only short two.</p></article></body></html>`
	_, err := Extract(strings.NewReader(page), "text/html", 0)
	if !errors.Is(err, ErrInsufficientContent) {
		t.Fatalf("err = %v, want ErrInsufficientContent", err)
	}
}

// Additional B: equal-sized candidates break ties by document order.
func TestExtractCandidateTieBreakDocumentOrder(t *testing.T) {
	one := `<article><p>` + strings.Repeat("Tie marker one. ", 35) + `</p></article>`
	two := `<article><p>` + strings.Repeat("Tie marker two. ", 35) + `</p></article>`
	page := `<html><body>` + one + two + `</body></html>`
	art, err := Extract(strings.NewReader(page), "text/html", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(art.Text, "Tie marker one.") {
		t.Fatalf("first candidate not selected: %q", art.Text)
	}
	if strings.Contains(art.Text, "Tie marker two.") {
		t.Errorf("later candidate selected: %q", art.Text)
	}
}

// Finding #4: an unsupported declared charset is rejected, not guessed around.
func TestExtractRejectsUnsupportedDeclaredCharset(t *testing.T) {
	page := `<html><body><article>` + longText("CharsetBody") + `</article></body></html>`
	for _, label := range []string{"made-up-charset", "iso-2022-kr"} {
		_, err := Extract(strings.NewReader(page), "text/html; charset="+label, 0)
		if !errors.Is(err, ErrEncoding) {
			t.Errorf("charset %q: err = %v, want ErrEncoding", label, err)
		}
	}
}

// Finding #4: a supported declared charset is honored end to end in the
// extractor (the fetcher preservation is tested in articlefetch).
func TestExtractDecodesShiftJIS(t *testing.T) {
	const jp = "こんにちは世界。これは日本語の記事です。"
	page := `<html><body><article><p>` + strings.Repeat(jp, 25) +
		`</p></article></body></html>`
	encoded, err := japanese.ShiftJIS.NewEncoder().String(page)
	if err != nil {
		t.Fatalf("encode shift_jis: %v", err)
	}
	art, err := Extract(strings.NewReader(encoded), "text/html; charset=shift_jis", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(art.Text, "こんにちは世界") {
		t.Errorf("shift_jis not decoded: %q", art.Text)
	}
}
