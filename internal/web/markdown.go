// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"bytes"
	"html/template"
	"regexp"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

// md renders GitHub-flavoured markdown. mdPolicy sanitises the result before it
// is trusted as HTML — the why text comes from the LLM and notes are free user
// input, so both are untrusted and must be scrubbed of scripts/raw HTML.
//
// Links out of a why or a note leave for a new tab, the same as an article
// title does: the feed is a place you are reading from, and following a
// reference should not cost you your place in it. Only fully qualified ones —
// a relative link is somewhere in this app, and those stay put. bluemonday
// adds rel="noopener" alongside the target for us.
var (
	md = goldmark.New(
		goldmark.WithExtensions(extension.GFM),
	)
	mdPolicy = bluemonday.UGCPolicy().AddTargetBlankToFullyQualifiedLinks(true)

	briefingPolicy = newBriefingPolicy()
)

func newBriefingPolicy() *bluemonday.Policy {
	p := bluemonday.NewPolicy()
	p.AllowStandardURLs()
	p.AllowElements("p", "br", "hr", "h1", "h2", "h3", "h4", "h5", "h6",
		"ul", "ol", "li", "strong", "em", "b", "i", "del", "code", "pre",
		"blockquote", "table", "thead", "tbody", "tr", "th", "td")
	p.AllowAttrs("href").OnElements("a")
	p.AllowAttrs("align").Matching(regexp.MustCompile(`^(left|right|center)$`)).OnElements("th", "td")
	p.AllowAttrs("start").Matching(regexp.MustCompile(`^[0-9]+$`)).OnElements("ol")
	p.RequireNoFollowOnLinks(true)
	p.RequireNoReferrerOnLinks(true)
	p.AddTargetBlankToFullyQualifiedLinks(true)
	return p
}

// renderMarkdown turns markdown source into safe HTML for the why/note panels.
// Real paragraph breaks, lists, emphasis, links, and code render as markup;
// anything dangerous is stripped. Empty input yields empty HTML.
func renderMarkdown(src string) template.HTML {
	return renderWith(mdPolicy, src)
}

func renderBriefingMarkdown(src string) template.HTML {
	return renderWith(briefingPolicy, src)
}

func renderWith(policy *bluemonday.Policy, src string) template.HTML {
	if src == "" {
		return ""
	}
	var buf bytes.Buffer
	if err := md.Convert([]byte(src), &buf); err != nil {
		// Fall back to the sanitised raw text rather than dropping content.
		return template.HTML(policy.Sanitize(src))
	}
	return template.HTML(policy.SanitizeBytes(buf.Bytes()))
}
