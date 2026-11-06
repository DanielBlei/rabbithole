// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package library

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/cases"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/tags"
)

// sourceRef locates a parsed source for cross-file checks.
type sourceRef struct {
	file string // file name, e.g. "rss.yaml"
	line int
	path string // "group/source name"
	src  Source
}

// labelPattern is display form: ASCII words split by single spaces, led by an uppercase letter or digit.
var labelPattern = regexp.MustCompile(`^[A-Z0-9][A-Za-z0-9]*( [A-Za-z0-9]+)*$`)

// checkSource returns one message per content problem in src, read from a file of type t.
func checkSource(t Type, src Source) []string {
	var msgs []string
	if strings.TrimSpace(src.Name) == "" {
		msgs = append(msgs, "name must be a non-empty string")
	}
	if strings.TrimSpace(src.Description) == "" {
		msgs = append(msgs, "description must be a non-empty string")
	}
	msgs = append(msgs, checkLabels(src.Labels)...)
	return append(msgs, checkURL(t, src.URL)...)
}

// checkLabels returns one message per invalid label and per repeated slug.
func checkLabels(labels []string) []string {
	if len(labels) == 0 {
		return []string{"labels must be a non-empty list"}
	}
	var msgs []string
	seen := map[string]bool{}
	for _, l := range labels {
		if !labelPattern.MatchString(l) {
			msgs = append(msgs, fmt.Sprintf("label %q must be ASCII words split by single spaces, ", l)+
				"starting with an uppercase letter or digit")
			continue
		}
		slug, err := tags.Normalize(l)
		if err != nil {
			msgs = append(msgs, fmt.Sprintf("label %q: %v", l, err))
			continue
		}
		if seen[slug] {
			msgs = append(msgs, fmt.Sprintf("duplicate label %q", l))
		}
		seen[slug] = true
	}
	return msgs
}

// checkURL returns the problems with raw, a source URL in a file of type t.
func checkURL(t Type, raw string) []string {
	if raw == "" {
		return []string{"url must be a non-empty string"}
	}
	var msgs []string
	if strings.ContainsFunc(raw, unicode.IsSpace) {
		msgs = append(msgs, "url must not contain whitespace")
	}
	if strings.ContainsFunc(raw, func(r rune) bool { return r >= 0x80 }) {
		msgs = append(msgs, "url must be ASCII, percent-encode any other character")
	}
	if !strings.HasPrefix(raw, "https://") {
		return append(msgs, "url must be https")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return append(msgs, "unparseable url: "+err.Error())
	}
	if u.Hostname() == "" {
		return append(msgs, "url must have a host")
	}
	if t == TypeAcademic {
		return append(msgs, checkAcademicURL(raw)...)
	}
	host := strings.ToLower(u.Hostname())
	if academicHosts[host] {
		msgs = append(msgs, fmt.Sprintf("academic host %s belongs in academic.yaml", host))
	}
	return msgs
}

// checkLibrary reports names and URLs that repeat across refs (the first wins) and label slugs
// spelled two ways. dropped holds the indexes of repeats, which the caller removes.
func checkLibrary(refs []sourceRef) (diags Diagnostics, dropped map[int]bool) {
	dropped = map[int]bool{}
	diag := func(ref sourceRef, msg string) {
		diags = append(diags, Diagnostic{File: ref.file, Line: ref.line, Path: ref.path, Message: msg})
	}
	names := map[string]string{} // name key to first file
	urls := map[string]string{}  // url key to first file
	type firstLabel struct{ spelling, file string }
	slugs := map[string]firstLabel{}

	for i, ref := range refs {
		nameKey := cases.Fold().String(strings.TrimSpace(ref.src.Name))
		urlKey := config.FeedURLKey(ref.src.URL)
		if first, ok := names[nameKey]; ok {
			diag(ref, fmt.Sprintf("duplicate name (also in %s)", first))
			dropped[i] = true
		}
		if first, ok := urls[urlKey]; ok {
			diag(ref, fmt.Sprintf("duplicate url (also in %s)", first))
			dropped[i] = true
		}
		if dropped[i] {
			continue
		}
		names[nameKey] = ref.file
		urls[urlKey] = ref.file

		for _, l := range ref.src.Labels {
			slug, err := tags.Normalize(l)
			if err != nil {
				continue
			}
			first, ok := slugs[slug]
			if !ok {
				slugs[slug] = firstLabel{spelling: l, file: ref.file}
				continue
			}
			if first.spelling != l {
				diag(ref, fmt.Sprintf("label %q has the same slug as %q (in %s)", l, first.spelling, first.file))
			}
		}
	}
	diags.sort()
	return diags, dropped
}
