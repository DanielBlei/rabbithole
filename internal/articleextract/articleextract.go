// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

// Package articleextract turns an article page into plain readable text. It
// parses HTML with x/net/html, never executes scripts, and emits only text.
package articleextract

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
	"golang.org/x/text/transform"
)

const (
	// DefaultMaxTextBytes bounds the extracted text when the caller gives no cap.
	DefaultMaxTextBytes = 32 << 10
	// MinContentChars is the fewest characters a usable article must yield.
	MinContentChars = 400
	// maxInputBytes bounds the raw page read before parsing. Fetching already
	// caps the body; this is a second bound for callers that stream something else.
	maxInputBytes = 8 << 20
	// maxTitleRunes caps the extracted title.
	maxTitleRunes = 500
)

var (
	// ErrEncoding marks bytes that cannot be decoded to valid UTF-8.
	ErrEncoding = errors.New("article encoding could not be handled")
	// ErrInsufficientContent marks a page that yielded too little text.
	ErrInsufficientContent = errors.New("could not extract enough article text")
)

// Article is the readable content extracted from one page. Truncated reports
// that Text stopped at the caller's byte cap.
type Article struct {
	Title     string
	Text      string
	Truncated bool
}

// Extract decodes and parses r. contentType supplies the declared charset;
// maxBytes caps the extracted plain text.
func Extract(r io.Reader, contentType string, maxBytes int) (Article, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxTextBytes
	}
	raw, err := io.ReadAll(io.LimitReader(r, maxInputBytes))
	if err != nil {
		return Article{}, fmt.Errorf("read article: %w", err)
	}
	decoded, err := decode(raw, contentType)
	if err != nil {
		return Article{}, err
	}
	doc, err := html.Parse(bytes.NewReader(decoded))
	if err != nil {
		// x/net/html is forgiving; a parse failure means the input was not HTML.
		return Article{}, fmt.Errorf("%w: %v", ErrEncoding, err)
	}

	root := contentRoot(doc)
	title := articleTitle(doc, root)
	var blocks []string
	var size int
	truncated := false
	walk(root, func(block string) bool {
		block = strings.TrimSpace(block)
		if block == "" {
			return true
		}
		if size > 0 {
			size++
		}
		blocks = append(blocks, block)
		size += len(block)
		if size >= maxBytes {
			truncated = true
			return false
		}
		return true
	})
	text := strings.TrimSpace(strings.Join(blocks, "\n\n"))
	if bounded := truncateBytes(text, maxBytes); bounded != text {
		text = bounded
		truncated = true
	}
	if utf8.RuneCountInString(text) < MinContentChars {
		return Article{Title: title, Text: text}, fmt.Errorf("%w: %d characters",
			ErrInsufficientContent, utf8.RuneCountInString(text))
	}
	return Article{Title: title, Text: text, Truncated: truncated}, nil
}

// decode converts raw to UTF-8 using the declared or detected charset. A
// declared UTF-8 page with invalid bytes is an encoding failure rather than a
// silent windows-1252 guess.
func decode(raw []byte, contentType string) ([]byte, error) {
	if declaredUTF8(contentType) && !utf8.Valid(raw) {
		return nil, fmt.Errorf("%w: invalid UTF-8 for declared charset", ErrEncoding)
	}
	enc, _, _ := charset.DetermineEncoding(raw, contentType)
	decoded, err := io.ReadAll(transform.NewReader(bytes.NewReader(raw), enc.NewDecoder()))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEncoding, err)
	}
	if !utf8.Valid(decoded) {
		return nil, fmt.Errorf("%w: decoded bytes are not UTF-8", ErrEncoding)
	}
	return decoded, nil
}

func declaredUTF8(contentType string) bool {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(params["charset"]), "utf-8")
}

// contentRoot prefers the page's main article container.
func contentRoot(doc *html.Node) *html.Node {
	for _, tag := range []string{"article", "main"} {
		if n := findElement(doc, func(n *html.Node) bool { return n.Data == tag }); n != nil {
			return n
		}
	}
	if n := findElement(doc, func(n *html.Node) bool {
		return n.Data == "div" && attr(n, "role") == "main"
	}); n != nil {
		return n
	}
	if n := findElement(doc, func(n *html.Node) bool { return n.Data == "body" }); n != nil {
		return n
	}
	return doc
}

func articleTitle(doc, root *html.Node) string {
	if h1 := findElement(root, func(n *html.Node) bool { return n.Data == "h1" }); h1 != nil {
		if text := collapse(gatherText(h1)); text != "" {
			return truncateRunes(text, maxTitleRunes)
		}
	}
	if title := findElement(doc, func(n *html.Node) bool { return n.Data == "title" }); title != nil {
		return truncateRunes(collapse(gatherText(title)), maxTitleRunes)
	}
	return ""
}

// walk emits blocks in document order. The callback returns false to stop.
func walk(n *html.Node, emit func(string) bool) bool {
	if n.Type == html.ElementNode {
		if skipElement(n) {
			return true
		}
		switch n.Data {
		case "h1", "h2", "h3", "h4", "h5", "h6":
			return emit("## " + collapse(gatherText(n)))
		case "p":
			return emit(collapse(gatherText(n)))
		case "li":
			return emit("- " + collapse(gatherText(n)))
		case "pre":
			return emit("```\n" + strings.TrimSpace(gatherRawText(n)) + "\n```")
		case "blockquote":
			return emit(quote(gatherText(n)))
		}
		// Many pages use unstyled divs for prose. Treat a container with no
		// block-level descendants as one paragraph, but recurse otherwise so
		// its real paragraphs are not duplicated.
		if (n.Data == "div" || n.Data == "section") && !hasBlockDescendant(n) {
			return emit(collapse(gatherText(n)))
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if !walk(c, emit) {
			return false
		}
	}
	return true
}

var skipTags = map[string]bool{
	"script": true, "style": true, "noscript": true, "template": true,
	"iframe": true, "svg": true, "canvas": true, "form": true,
	"button": true, "input": true, "select": true, "nav": true,
	"aside": true, "footer": true, "head": true,
}

func skipElement(n *html.Node) bool {
	if skipTags[n.Data] {
		return true
	}
	if _, ok := attrValue(n, "hidden"); ok {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(attr(n, "aria-hidden")), "true") {
		return true
	}
	return false
}

var blockTags = map[string]bool{
	"p": true, "div": true, "section": true, "article": true, "main": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"li": true, "ul": true, "ol": true, "pre": true, "blockquote": true,
	"table": true, "thead": true, "tbody": true, "tr": true, "td": true,
	"figure": true, "figcaption": true,
}

func hasBlockDescendant(n *html.Node) bool {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode {
			if blockTags[c.Data] {
				return true
			}
			if hasBlockDescendant(c) {
				return true
			}
		}
	}
	return false
}

func gatherText(n *html.Node) string {
	var b strings.Builder
	var rec func(*html.Node)
	rec = func(n *html.Node) {
		if n.Type == html.ElementNode && skipElement(n) {
			return
		}
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
			b.WriteByte(' ')
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			rec(c)
		}
	}
	rec(n)
	return b.String()
}

func gatherRawText(n *html.Node) string {
	var b strings.Builder
	var rec func(*html.Node)
	rec = func(n *html.Node) {
		if n.Type == html.ElementNode && skipElement(n) {
			return
		}
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			rec(c)
		}
	}
	rec(n)
	return b.String()
}

func quote(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for i, line := range lines {
		line = collapse(line)
		if line == "" {
			lines[i] = ">"
			continue
		}
		lines[i] = "> " + line
	}
	return strings.Join(lines, "\n")
}

func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

func findElement(n *html.Node, match func(*html.Node) bool) *html.Node {
	if n.Type == html.ElementNode && match(n) {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := findElement(c, match); found != nil {
			return found
		}
	}
	return nil
}

func attr(n *html.Node, name string) string {
	v, _ := attrValue(n, name)
	return v
}

func attrValue(n *html.Node, name string) (string, bool) {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, name) {
			return a.Val, true
		}
	}
	return "", false
}

func truncateBytes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return strings.TrimRightFunc(s[:end], func(r rune) bool { return r == ' ' || r == '\n' || r == '\t' })
}

func truncateRunes(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes])
}
