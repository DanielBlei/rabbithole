// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package articleextract

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func withPadding(body string) string {
	return `<html><body><article>` + body + longText("Padding") + `</article></body></html>`
}

// QA 1: max_text_bytes is a byte budget; a 400-character multibyte article is
// 1200 bytes and needs a correspondingly larger budget.
func TestExtractMultibyteContentWithinByteBudget(t *testing.T) {
	const jp = "こんにちは世界。これは日本語の記事です。" // 20 runes, 60 UTF-8 bytes
	page := `<html><body><article><p>` + strings.Repeat(jp, 20) +
		`</p></article></body></html>`
	art, err := Extract(strings.NewReader(page), "text/html; charset=utf-8", 4*MinContentChars)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if utf8.RuneCountInString(art.Text) < MinContentChars {
		t.Fatalf("text has %d runes, want at least %d",
			utf8.RuneCountInString(art.Text), MinContentChars)
	}
	if !strings.Contains(art.Text, "こんにちは世界") {
		t.Errorf("multibyte text corrupted: %q", art.Text)
	}
}

// QA 1: the byte cap may fall inside a rune; truncation must still produce
// valid UTF-8.
func TestExtractTruncationKeepsUTF8(t *testing.T) {
	const jp = "こんにちは世界。これは日本語の記事です。"
	long := strings.Repeat(jp, 40)
	for _, maxBytes := range []int{1, 2, 3, 4, 5, 1000, 1001, 1002, 1601, 1602, 1603, 2000} {
		got := truncateBytes(long, maxBytes)
		if !utf8.ValidString(got) {
			t.Fatalf("maxBytes %d: truncation produced invalid UTF-8", maxBytes)
		}
		if len(got) > maxBytes {
			t.Fatalf("maxBytes %d: text is %d bytes", maxBytes, len(got))
		}
	}
	page := `<html><body><article><p>` + long + `</p></article></body></html>`
	for _, maxBytes := range []int{1601, 1602, 1603, 2000} {
		art, err := Extract(strings.NewReader(page), "text/html; charset=utf-8", maxBytes)
		if err != nil {
			t.Fatalf("maxBytes %d: Extract: %v", maxBytes, err)
		}
		if !utf8.ValidString(art.Text) {
			t.Fatalf("maxBytes %d: extracted text is not valid UTF-8", maxBytes)
		}
		if len(art.Text) > maxBytes {
			t.Fatalf("maxBytes %d: text is %d bytes", maxBytes, len(art.Text))
		}
	}
}

// QA 2: the global 256-candidate cutoff used to abandon the primary article
// after 266 short teasers and fall back to the whole body.
func TestExtractCandidateBudgetDoesNotHideLaterArticle(t *testing.T) {
	var teasers strings.Builder
	for i := 0; i < 266; i++ {
		fmt.Fprintf(&teasers, `<article><p>Teaser %d.</p></article>`, i)
	}
	page := `<html><body>` + teasers.String() + `<article>` + longText("Primary") +
		`</article></body></html>`
	art, err := Extract(strings.NewReader(page), "text/html", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(art.Text, "Primary sentence number 0") {
		t.Fatalf("primary article not selected: %q", art.Text)
	}
	if strings.Contains(art.Text, "Teaser 0.") {
		t.Errorf("teaser contamination: %q", art.Text)
	}
}

// QA 2: thousands of sibling candidates remain bounded and do not hide the
// primary article.
func TestExtractThousandsOfSiblingCandidates(t *testing.T) {
	var cards strings.Builder
	for i := 0; i < 2000; i++ {
		fmt.Fprintf(&cards, `<article><p>Card %d.</p></article>`, i)
	}
	page := `<html><body>` + cards.String() + `<article>` + longText("Primary") +
		`</article></body></html>`
	art, err := Extract(strings.NewReader(page), "text/html", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(art.Text, "Primary sentence number 0") {
		t.Fatalf("primary article not selected: %q", art.Text)
	}
	if strings.Contains(art.Text, "Card 0.") {
		t.Errorf("card contamination: %q", art.Text)
	}
}

// QA 2: nested candidates beyond the tracked depth still contribute text to
// the bounded ancestors.
func TestExtractDeeplyNestedCandidates(t *testing.T) {
	const depth = 40
	page := `<html><body>` + strings.Repeat(`<article>`, depth) +
		longText("Nested") + strings.Repeat(`</article>`, depth) + `</body></html>`
	art, err := Extract(strings.NewReader(page), "text/html", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(art.Text, "Nested sentence number 0") {
		t.Fatalf("deeply nested text lost: %q", art.Text)
	}
}

// QA 2: output truncation applies after selection, so primary content survives
// even when teasers precede it.
func TestExtractPrimarySurvivesOutputTruncation(t *testing.T) {
	var teasers strings.Builder
	for i := 0; i < 266; i++ {
		fmt.Fprintf(&teasers, `<article><p>Teaser %d.</p></article>`, i)
	}
	page := `<html><body>` + teasers.String() + `<article>` + longText("Primary") +
		`</article></body></html>`
	art, err := Extract(strings.NewReader(page), "text/html", 1200)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(art.Text, "Primary sentence number 0") {
		t.Fatalf("primary content lost to truncation: %q", art.Text)
	}
	if strings.Contains(art.Text, "Teaser 0.") {
		t.Errorf("teaser contamination under truncation: %q", art.Text)
	}
}

// QA 3: title metadata under a hidden head is ineligible.
func TestExtractTitleRejectsHiddenHead(t *testing.T) {
	page := `<html><head hidden><title>Hidden document title</title></head>` +
		`<body><article>` + longText("TitleBody") + `</article></body></html>`
	art, err := Extract(strings.NewReader(page), "text/html", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if art.Title == "Hidden document title" {
		t.Fatalf("hidden head title was used: %q", art.Title)
	}
}

// QA 3: aria-hidden disqualifies title metadata too.
func TestExtractTitleRejectsAriaHiddenHead(t *testing.T) {
	page := `<html><head aria-hidden="true"><title>Hidden document title</title></head>` +
		`<body><article>` + longText("TitleBody") + `</article></body></html>`
	art, err := Extract(strings.NewReader(page), "text/html", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if art.Title == "Hidden document title" {
		t.Fatalf("aria-hidden title was used: %q", art.Title)
	}
}

// QA 3: a title inside a template is not document metadata.
func TestExtractTitleRejectsTemplateTitle(t *testing.T) {
	page := `<html><body><template><title>Hidden document title</title></template>` +
		`<article>` + longText("TitleBody") + `</article></body></html>`
	art, err := Extract(strings.NewReader(page), "text/html", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if art.Title == "Hidden document title" {
		t.Fatalf("template title was used: %q", art.Title)
	}
}

// QA 3: hidden metadata must not override a visible heading outside the chosen
// root.
func TestExtractTitlePrefersVisibleHeadingOverHiddenMetadata(t *testing.T) {
	page := `<html><head hidden><title>Hidden document title</title></head><body>` +
		`<main><article>` + longText("TitleBody") + `</article></main>` +
		`<h1>Visible title</h1></body></html>`
	art, err := Extract(strings.NewReader(page), "text/html", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if art.Title != "Visible title" {
		t.Fatalf("title = %q, want the visible heading", art.Title)
	}
}

// QA 3: no eligible title and no visible heading yields an empty title.
func TestExtractTitleNoEligibleFallback(t *testing.T) {
	page := `<html><head hidden><title>Hidden document title</title></head>` +
		`<body><article>` + longText("TitleBody") + `</article></body></html>`
	art, err := Extract(strings.NewReader(page), "text/html", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if art.Title != "" {
		t.Fatalf("title = %q, want empty", art.Title)
	}
}

// QA 4: inline markup must not synthesize a space absent from the source.
func TestExtractKeepsInlineWordContiguous(t *testing.T) {
	art, err := Extract(strings.NewReader(withPadding(`<p>caf<b>é</b> words.</p>`)),
		"text/html", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(art.Text, "café words.") {
		t.Fatalf("inline word split: %q", art.Text)
	}
}

// QA 4: CJK split across inline elements stays contiguous.
func TestExtractKeepsCJKInlineContiguous(t *testing.T) {
	art, err := Extract(strings.NewReader(withPadding(`<p>日本<b>語</b>の記事。</p>`)),
		"text/html", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(art.Text, "日本語の記事。") {
		t.Fatalf("CJK text split: %q", art.Text)
	}
}

// QA 4: combining marks stay attached across inline boundaries.
func TestExtractKeepsCombiningMarks(t *testing.T) {
	art, err := Extract(strings.NewReader(withPadding(`<p>caf<b>e&#x301;</b> words.</p>`)),
		"text/html", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(art.Text, "café words.") {
		t.Fatalf("combining mark detached: %q", art.Text)
	}
}

// QA 4: source whitespace around inline elements is preserved.
func TestExtractPreservesInlineSourceWhitespace(t *testing.T) {
	art, err := Extract(strings.NewReader(withPadding(`<p><b>Hello</b> <i>world</i>.</p>`)),
		"text/html", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(art.Text, "Hello world.") {
		t.Fatalf("source whitespace lost: %q", art.Text)
	}
}

// QA 4: nested inline markup stays inline and unduplicated.
func TestExtractNestedInlineMarkup(t *testing.T) {
	art, err := Extract(strings.NewReader(withPadding(
		`<p><b>bold <i>and italic</i></b> text.</p>`)), "text/html", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(art.Text, "bold and italic text.") {
		t.Fatalf("nested inline text wrong: %q", art.Text)
	}
	if got := strings.Count(art.Text, "and italic"); got != 1 {
		t.Errorf("inline text appears %d times, want once: %q", got, art.Text)
	}
}

// QA 4: punctuation after an inline link stays attached.
func TestExtractLinkBesidePunctuation(t *testing.T) {
	art, err := Extract(strings.NewReader(withPadding(
		`<p>see <a href="/x">this</a>.</p>`)), "text/html", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(art.Text, "see this.") {
		t.Fatalf("link punctuation split: %q", art.Text)
	}
}

// QA 4: block boundaries still separate paragraphs.
func TestExtractKeepsBlockSeparation(t *testing.T) {
	art, err := Extract(strings.NewReader(withPadding(
		`<p>first block</p><p>second block</p>`)), "text/html", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(art.Text, "first block\n\nsecond block") {
		t.Fatalf("block separation lost: %q", art.Text)
	}
}

// QA 4: a source line break separates text.
func TestExtractKeepsLineBreakSeparation(t *testing.T) {
	art, err := Extract(strings.NewReader(withPadding(
		`<p>line one<br>line two</p>`)), "text/html", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(art.Text, "line one line two") {
		t.Fatalf("line break lost: %q", art.Text)
	}
}

// QA 5: nested list items are indented by depth.
func TestExtractNestedListIndentation(t *testing.T) {
	art, err := Extract(strings.NewReader(withPadding(
		`<ul><li>Parent<ul><li>Child</li></ul></li></ul>`)), "text/html", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(art.Text, "- Parent") {
		t.Fatalf("parent item missing: %q", art.Text)
	}
	if !strings.Contains(art.Text, "\n\n  - Child") {
		t.Fatalf("child item not indented: %q", art.Text)
	}
}

// QA 5: mixed list nesting indents each level deterministically.
func TestExtractMixedListIndentation(t *testing.T) {
	art, err := Extract(strings.NewReader(withPadding(
		`<ol><li>One<ul><li>Two<ol><li>Three</li></ol></li></ul></li></ol>`)),
		"text/html", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for _, want := range []string{"- One", "\n\n  - Two", "\n\n    - Three"} {
		if !strings.Contains(art.Text, want) {
			t.Errorf("missing %q in %q", want, art.Text)
		}
	}
}

// QA 5: inline text in a list item keeps the item marker.
func TestExtractListItemInlineText(t *testing.T) {
	art, err := Extract(strings.NewReader(withPadding(
		`<ul><li>Item <b>bold</b> text</li></ul>`)), "text/html", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(art.Text, "- Item bold text") {
		t.Fatalf("list item inline text wrong: %q", art.Text)
	}
}

// QA 5: a paragraph inside a list item receives the item marker.
func TestExtractListItemParagraph(t *testing.T) {
	art, err := Extract(strings.NewReader(withPadding(
		`<ul><li><p>First paragraph</p><ul><li>Child</li></ul></li></ul>`)),
		"text/html", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(art.Text, "- First paragraph") {
		t.Fatalf("list item paragraph lost its marker: %q", art.Text)
	}
	if !strings.Contains(art.Text, "\n\n  - Child") {
		t.Fatalf("nested child not indented: %q", art.Text)
	}
}

// QA 5: indentation is capped at the documented supported depth.
func TestExtractDeepListIndentationBounded(t *testing.T) {
	var lists strings.Builder
	for i := 0; i <= maxListIndentDepth+1; i++ {
		fmt.Fprintf(&lists, `<ul><li>L%d`, i)
	}
	for i := 0; i <= maxListIndentDepth+1; i++ {
		lists.WriteString(`</li></ul>`)
	}
	art, err := Extract(strings.NewReader(withPadding(lists.String())), "text/html", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	maxIndent := strings.Repeat("  ", maxListIndentDepth-1)
	if !strings.Contains(art.Text, "\n\n"+maxIndent+"- L9") {
		t.Fatalf("deepest indentation not capped at %d levels: %q",
			maxListIndentDepth, art.Text)
	}
	if strings.Contains(art.Text, maxIndent+"  - ") {
		t.Fatalf("indentation exceeded the cap: %q", art.Text)
	}
}
