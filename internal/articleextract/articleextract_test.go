// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package articleextract

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func longText(prefix string) string {
	var b strings.Builder
	b.WriteString("<p>")
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&b, "%s sentence number %d has enough words to look like prose. ", prefix, i)
	}
	b.WriteString("</p>")
	return b.String()
}

func TestExtractDropsChrome(t *testing.T) {
	page := `<html><head><title>Page title</title><style>.no{}</style></head><body>` +
		`<nav>NAVTEXT</nav><aside>ASIDETEXT</aside><footer>FOOTERTEXT</footer>` +
		`<form>FORMTEXT</form><button>BUTTONTEXT</button>` +
		`<article><h1>Real heading</h1>` + longText("Real") +
		`<script>SCRIPTTEXT</script><noscript>NOSCRIPTTEXT</noscript>` +
		`</article></body></html>`
	art, err := Extract(strings.NewReader(page), "text/html; charset=utf-8", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if art.Title != "Real heading" {
		t.Errorf("title = %q", art.Title)
	}
	for _, unwanted := range []string{"NAVTEXT", "ASIDETEXT", "FOOTERTEXT", "FORMTEXT",
		"BUTTONTEXT", "SCRIPTTEXT", "NOSCRIPTTEXT"} {
		if strings.Contains(art.Text, unwanted) {
			t.Errorf("text contains dropped %q: %q", unwanted, art.Text)
		}
	}
	if !strings.Contains(art.Text, "Real sentence number 0") {
		t.Errorf("missing article prose: %q", art.Text)
	}
	if !strings.Contains(art.Text, "## Real heading") {
		t.Errorf("heading not marked: %q", art.Text)
	}
}

func TestExtractRetainsStructure(t *testing.T) {
	page := `<html><body><article>` + longText("Intro") +
		`<h2>Section</h2><p>Paragraph <b>with bold</b> text.</p>` +
		`<ul><li>First point</li><li>Second point</li></ul>` +
		`<pre><code>code line one
code line two</code></pre>` +
		`<blockquote>Quoted words</blockquote></article></body></html>`
	art, err := Extract(strings.NewReader(page), "text/html", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for _, want := range []string{"## Section", "Paragraph with bold text.", "- First point",
		"- Second point", "```\ncode line one\ncode line two\n```", "> Quoted words"} {
		if !strings.Contains(art.Text, want) {
			t.Errorf("text missing %q:\n%s", want, art.Text)
		}
	}
}

func TestExtractPrefersArticleOverBody(t *testing.T) {
	page := `<html><body><div>OUTSIDETEXT</div><main><p>` + longText("Main") +
		`</p></main><article><p>` + longText("Article") + `</p></article></body></html>`
	art, err := Extract(strings.NewReader(page), "text/html", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if strings.Contains(art.Text, "OUTSIDETEXT") || strings.Contains(art.Text, "Main sentence") {
		t.Errorf("text includes content outside the article: %q", art.Text)
	}
	if !strings.Contains(art.Text, "Article sentence") {
		t.Errorf("missing article text: %q", art.Text)
	}
}

func TestExtractTitleFallsBackToTitleElement(t *testing.T) {
	page := `<html><head><title>Head &amp; title</title></head><body><article>` +
		longText("Body") + `</article></body></html>`
	art, err := Extract(strings.NewReader(page), "text/html", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if art.Title != "Head & title" {
		t.Errorf("title = %q", art.Title)
	}
}

func TestExtractDecodesLatin1(t *testing.T) {
	body := []byte("<html><body><article><p>caf\xe9 tr\xe8s bon. " +
		strings.Repeat("Enough words to pass the minimum content guard. ", 20) +
		"</p></article></body></html>")
	art, err := Extract(strings.NewReader(string(body)), "text/html; charset=iso-8859-1", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(art.Text, "café très bon") {
		t.Errorf("latin-1 not decoded: %q", art.Text)
	}
}

func TestExtractDecodesMetaCharset(t *testing.T) {
	body := []byte(`<html><head><meta charset="windows-1252"></head><body><article><p>` +
		"caf\xe9 " + strings.Repeat("Enough words to pass the minimum content guard. ", 20) +
		`</p></article></body></html>`)
	art, err := Extract(strings.NewReader(string(body)), "text/html", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(art.Text, "café") {
		t.Errorf("meta charset not decoded: %q", art.Text)
	}
}

func TestExtractRejectsInvalidDeclaredUTF8(t *testing.T) {
	body := "<html><body><article><p>bad \xff\xfe bytes " +
		strings.Repeat("Enough words to pass the minimum content guard. ", 20) +
		"</p></article></body></html>"
	_, err := Extract(strings.NewReader(body), "text/html; charset=utf-8", 0)
	if !errors.Is(err, ErrEncoding) {
		t.Fatalf("err = %v, want ErrEncoding", err)
	}
}

func TestExtractToleratesMalformedHTML(t *testing.T) {
	page := `<html><body><article><p>` + strings.Repeat("unclosed paragraph text ", 40) +
		`<div><p>Second paragraph<div>nested`
	art, err := Extract(strings.NewReader(page), "text/html", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(art.Text, "unclosed paragraph") || !strings.Contains(art.Text, "Second paragraph") {
		t.Errorf("malformed HTML lost text: %q", art.Text)
	}
}

func TestExtractBoundsOutput(t *testing.T) {
	page := `<html><body><article>` + longText("Bound") + longText("Tail") + `</article></body></html>`
	art, err := Extract(strings.NewReader(page), "text/html", 500)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(art.Text) > 500 {
		t.Errorf("text is %d bytes, want <= 500", len(art.Text))
	}
	if strings.Contains(art.Text, "Tail") {
		t.Errorf("text was not bounded: %q", art.Text)
	}
}

func TestExtractInsufficientContent(t *testing.T) {
	_, err := Extract(strings.NewReader("<html><body><p>Too short.</p></body></html>"), "text/html", 0)
	if !errors.Is(err, ErrInsufficientContent) {
		t.Fatalf("err = %v, want ErrInsufficientContent", err)
	}
}

func TestExtractDropsHidden(t *testing.T) {
	page := `<html><body><article><p hidden>HIDDENTEXT</p><div aria-hidden="true">ARIATEXT</div>` +
		longText("Visible") + `</article></body></html>`
	art, err := Extract(strings.NewReader(page), "text/html", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if strings.Contains(art.Text, "HIDDENTEXT") || strings.Contains(art.Text, "ARIATEXT") {
		t.Errorf("hidden content retained: %q", art.Text)
	}
}
