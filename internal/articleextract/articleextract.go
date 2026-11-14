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
	"unicode"
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
	// MaxInputBytes bounds the raw HTML the extractor accepts. It matches the
	// largest page the configurable article fetch cap permits; input beyond it
	// is rejected rather than silently truncated.
	MaxInputBytes = 16 << 20
	// maxTitleRunes caps the extracted title.
	maxTitleRunes = 500
	// maxRootCandidates bounds the candidate nesting the root scan tracks. Text
	// is counted once per active candidate, so work is O(text × depth) even with
	// thousands of sibling candidates, and only the active stack plus the two
	// best candidates are retained.
	maxRootCandidates = 32
	// maxListIndentDepth bounds nested-list indentation.
	maxListIndentDepth = 8
)

var (
	// ErrEncoding marks bytes that cannot be decoded to valid UTF-8.
	ErrEncoding = errors.New("article encoding could not be handled")
	// ErrInsufficientContent marks a page that yielded too little text.
	ErrInsufficientContent = errors.New("could not extract enough article text")
	// ErrInputTooLarge marks raw HTML beyond MaxInputBytes. It is distinct from
	// text truncation: the input is rejected, never parsed partially.
	ErrInputTooLarge = errors.New("article HTML exceeds the extractor input limit")
)

// Article is the readable content extracted from one page. Truncated reports
// that Text stopped at the caller's byte cap; it never reports raw HTML
// truncation, which is rejected with ErrInputTooLarge instead.
type Article struct {
	Title     string
	Text      string
	Truncated bool
}

// Extract decodes and parses r. contentType supplies the declared charset;
// maxBytes caps the extracted plain text. Raw HTML beyond MaxInputBytes is
// rejected, never parsed as a truncated prefix.
func Extract(r io.Reader, contentType string, maxBytes int) (Article, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxTextBytes
	}
	raw, err := io.ReadAll(io.LimitReader(r, MaxInputBytes+1))
	if err != nil {
		return Article{}, fmt.Errorf("read article: %w", err)
	}
	if len(raw) > MaxInputBytes {
		return Article{}, fmt.Errorf("%w: %d bytes", ErrInputTooLarge, len(raw))
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
		// Trim block edges but keep intentional leading list indentation.
		block = strings.TrimRight(block, " \t\r\n")
		if strings.TrimSpace(block) == "" {
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
	text := strings.TrimRight(strings.Join(blocks, "\n\n"), " \t\r\n")
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
// silent windows-1252 guess. A declared charset that HTML does not support is
// rejected instead of being ignored in favor of detection.
func decode(raw []byte, contentType string) ([]byte, error) {
	if label := declaredCharset(contentType); label != "" {
		enc, name := charset.Lookup(label)
		if enc == nil || strings.EqualFold(name, "replacement") {
			return nil, fmt.Errorf("%w: unsupported charset %q", ErrEncoding, label)
		}
		if strings.EqualFold(name, "utf-8") && !utf8.Valid(raw) {
			return nil, fmt.Errorf("%w: invalid UTF-8 for declared charset", ErrEncoding)
		}
	}
	enc, name, _ := charset.DetermineEncoding(raw, contentType)
	// The HTML "replacement" encoding decodes every byte to U+FFFD; treating it
	// as success would analyze corrupted text. It can arrive from the header or
	// a meta tag, so guard the determined encoding too.
	if enc == nil || strings.EqualFold(name, "replacement") {
		return nil, fmt.Errorf("%w: unsupported charset %q", ErrEncoding, name)
	}
	decoded, err := io.ReadAll(transform.NewReader(bytes.NewReader(raw), enc.NewDecoder()))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEncoding, err)
	}
	if !utf8.Valid(decoded) {
		return nil, fmt.Errorf("%w: decoded bytes are not UTF-8", ErrEncoding)
	}
	return decoded, nil
}

func declaredCharset(contentType string) string {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(params["charset"])
}

// contentRoot prefers a substantive visible article, then a substantive main,
// and otherwise falls back to the body. Candidates inside skipped or hidden
// containers are never visited, so they cannot capture the root.
func contentRoot(doc *html.Node) *html.Node {
	var bestArticle, bestMain *rootCandidate
	stack := make([]*rootCandidate, 0, 8)
	order := 0

	var visit func(*html.Node)
	visit = func(n *html.Node) {
		if n.Type == html.ElementNode && skipElement(n) {
			return
		}
		var cand *rootCandidate
		if n.Type == html.ElementNode && len(stack) < maxRootCandidates {
			switch {
			case n.Data == "article":
				cand = &rootCandidate{node: n, rank: 0, order: order}
			case n.Data == "main":
				cand = &rootCandidate{node: n, rank: 1, order: order}
			case n.Data == "div" && strings.EqualFold(strings.TrimSpace(attr(n, "role")), "main"):
				cand = &rootCandidate{node: n, rank: 2, order: order}
			}
			if cand != nil {
				order++
				stack = append(stack, cand)
			}
		}
		if n.Type == html.TextNode {
			count := contentRuneCount(n.Data)
			for _, c := range stack {
				c.runes += count
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			visit(c)
		}
		if cand != nil {
			stack = stack[:len(stack)-1]
			if cand.rank == 0 {
				bestArticle = betterRoot(bestArticle, cand)
			} else {
				bestMain = betterRoot(bestMain, cand)
			}
		}
	}
	visit(doc)

	// A candidate must clear the same minimum the extractor enforces, so a
	// short teaser or card does not shadow substantive content elsewhere.
	if bestArticle != nil && bestArticle.runes >= MinContentChars {
		return bestArticle.node
	}
	if bestMain != nil && bestMain.runes >= MinContentChars {
		return bestMain.node
	}
	if body := findElement(doc, func(n *html.Node) bool { return n.Data == "body" }); body != nil &&
		!skipElement(body) {
		return body
	}
	return doc
}

type rootCandidate struct {
	node  *html.Node
	rank  int // article 0, main 1, div[role=main] 2
	order int
	runes int // visible non-whitespace runes
}

// contentRuneCount measures actual content, so indentation and formatting
// whitespace cannot make an empty container look substantive.
func contentRuneCount(s string) int {
	n := 0
	for _, r := range s {
		if !unicode.IsSpace(r) {
			n++
		}
	}
	return n
}

// betterRoot ranks by visible rune count, then tag specificity, then document
// order. The result is deterministic for equal candidates.
func betterRoot(best, cand *rootCandidate) *rootCandidate {
	if best == nil {
		return cand
	}
	switch {
	case cand.runes > best.runes:
		return cand
	case cand.runes < best.runes:
		return best
	case cand.rank < best.rank:
		return cand
	case cand.rank > best.rank:
		return best
	case cand.order < best.order:
		return cand
	default:
		return best
	}
}

func articleTitle(doc, root *html.Node) string {
	if title := headingTitle(root); title != "" {
		return title
	}
	if title := findTitleElement(doc); title != nil {
		if text := truncateRunes(collapse(gatherText(title)), maxTitleRunes); text != "" {
			return text
		}
	}
	// A hidden metadata title must not beat a visible heading outside the
	// chosen root; this is the last eligible fallback.
	return headingTitle(doc)
}

func headingTitle(root *html.Node) string {
	h1 := findVisibleElement(root, func(n *html.Node) bool { return n.Data == "h1" })
	if h1 == nil {
		return ""
	}
	return truncateRunes(collapse(gatherText(h1)), maxTitleRunes)
}

// findTitleElement accepts a title under a normal head, unlike the body-content
// skip rule which rejects head entirely. Hidden, aria-hidden, template and
// other skipped ancestors still disqualify the title.
func findTitleElement(n *html.Node) *html.Node {
	if n.Type == html.ElementNode {
		if hiddenElement(n) {
			return nil
		}
		if n.Data == "title" {
			return n
		}
		if n.Data != "head" && skipTags[n.Data] {
			return nil
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := findTitleElement(c); found != nil {
			return found
		}
	}
	return nil
}

// walk emits blocks in document order. The callback returns false to stop.
func walk(n *html.Node, emit func(string) bool) bool {
	return walkMixed(n, emit, "", 0)
}

// walkMixed emits a container's direct prose and its nested blocks in document
// order. Inline markup stays text; block children are emitted once by their own
// handler. firstPrefix marks the first paragraph block, such as a list item's
// indented "- ". listDepth is the number of enclosing lists.
func walkMixed(n *html.Node, emit func(string) bool, firstPrefix string, listDepth int) bool {
	var prose strings.Builder
	pending := firstPrefix
	emitBlock := func(block string) bool {
		if pending != "" {
			block = pending + block
			pending = ""
		}
		return emit(block)
	}
	flush := func() bool {
		block := collapse(prose.String())
		prose.Reset()
		if block == "" {
			return true
		}
		return emitBlock(block)
	}

	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.TextNode {
			prose.WriteString(c.Data)
			continue
		}
		if c.Type != html.ElementNode || skipElement(c) {
			continue
		}
		switch c.Data {
		case "br":
			prose.WriteString(" ")
		case "h1", "h2", "h3", "h4", "h5", "h6":
			if !flush() {
				return false
			}
			if !emit("## " + collapse(gatherText(c))) {
				return false
			}
		case "p":
			if !flush() {
				return false
			}
			if !emitBlock(collapse(gatherText(c))) {
				return false
			}
		case "pre":
			if !flush() {
				return false
			}
			if !emit("```\n" + strings.TrimSpace(gatherRawText(c)) + "\n```") {
				return false
			}
		case "blockquote":
			if !flush() {
				return false
			}
			if !emit(quote(gatherText(c))) {
				return false
			}
		case "li":
			if !flush() {
				return false
			}
			indent := strings.Repeat("  ", max(listDepth-1, 0))
			if !walkMixed(c, emit, indent+"- ", listDepth) {
				return false
			}
		default:
			if blockTags[c.Data] {
				if !flush() {
					return false
				}
				depth := listDepth
				if c.Data == "ul" || c.Data == "ol" {
					depth = min(listDepth+1, maxListIndentDepth)
				}
				if !walkMixed(c, emit, "", depth) {
					return false
				}
				continue
			}
			prose.WriteString(gatherText(c))
		}
	}
	return flush()
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
	return hiddenElement(n)
}

func hiddenElement(n *html.Node) bool {
	if _, ok := attrValue(n, "hidden"); ok {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(attr(n, "aria-hidden")), "true")
}

var blockTags = map[string]bool{
	"p": true, "div": true, "section": true, "article": true, "main": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"li": true, "ul": true, "ol": true, "pre": true, "blockquote": true,
	"table": true, "thead": true, "tbody": true, "tr": true, "td": true,
	"figure": true, "figcaption": true,
}

func gatherText(n *html.Node) string {
	var b strings.Builder
	var rec func(*html.Node)
	rec = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if skipElement(n) {
				return
			}
			if n.Data == "br" {
				b.WriteByte(' ')
				return
			}
		}
		if n.Type == html.TextNode {
			// Inline markup splits prose into adjacent text nodes. Concatenate
			// them exactly: HTML does not imply whitespace at a tag boundary.
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

func gatherRawText(n *html.Node) string {
	var b strings.Builder
	var rec func(*html.Node)
	rec = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if skipElement(n) {
				return
			}
			if n.Data == "br" {
				b.WriteByte('\n')
				return
			}
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

// findVisibleElement is findElement that prunes skipped and hidden subtrees, so
// a hidden heading cannot supply the title.
func findVisibleElement(n *html.Node, match func(*html.Node) bool) *html.Node {
	if n.Type == html.ElementNode && skipElement(n) {
		return nil
	}
	if n.Type == html.ElementNode && match(n) {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := findVisibleElement(c, match); found != nil {
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
