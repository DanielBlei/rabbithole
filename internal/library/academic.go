// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package library

import (
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// provider is one supported academic search shape.
type provider struct {
	path     string
	required []string // sorted
	optional []string
}

// providers maps a canonical lowercase host to its search shape.
var providers = map[string]provider{
	"arxiv.org":        {path: "/search", required: []string{"q"}},
	"api.crossref.org": {path: "/works", optional: []string{"q", "issn", "journal"}},
	"www.semanticscholar.org": {
		path:     "/search",
		required: []string{"q"},
		optional: []string{"open_access", "min_citations"},
	},
}

// academicHosts are the hosts the app routes to an academic provider (internal/ingest/academic.go).
var academicHosts = map[string]bool{
	"arxiv.org":               true,
	"www.arxiv.org":           true,
	"export.arxiv.org":        true,
	"crossref.org":            true,
	"www.crossref.org":        true,
	"api.crossref.org":        true,
	"semanticscholar.org":     true,
	"www.semanticscholar.org": true,
	"api.semanticscholar.org": true,
}

var (
	badEscape    = regexp.MustCompile(`%(?:[^0-9A-Fa-f]|[0-9A-Fa-f][^0-9A-Fa-f]|[0-9A-Fa-f]?$)`)
	issnPattern  = regexp.MustCompile(`^[0-9]{4}-[0-9]{3}[0-9Xx]$`)
	countPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)
)

// queryParams is a decoded query string.
type queryParams struct {
	keys   []string            // first-appearance order
	values map[string][]string // every value per key, in order
}

// parseQuery splits raw on "&" and decodes each pair. A pair without "=" has an empty
// value, and a pair that fails to decode is skipped.
func parseQuery(raw string) queryParams {
	params := queryParams{values: map[string][]string{}}
	for segment := range strings.SplitSeq(raw, "&") {
		if segment == "" {
			continue
		}
		name, value, _ := strings.Cut(segment, "=")
		key, err := url.QueryUnescape(name)
		if err != nil {
			continue
		}
		val, err := url.QueryUnescape(value)
		if err != nil {
			continue
		}
		if _, seen := params.values[key]; !seen {
			params.keys = append(params.keys, key)
		}
		params.values[key] = append(params.values[key], val)
	}
	return params
}

// checkAcademicURL returns one message per problem, nil when raw is a supported search URL.
func checkAcademicURL(raw string) []string {
	u, err := url.Parse(raw)
	if err != nil {
		return []string{"unparseable url: " + err.Error()}
	}
	host := strings.ToLower(u.Hostname())
	p, ok := providers[host]
	if !ok {
		hosts := slices.Sorted(maps.Keys(providers))
		return []string{fmt.Sprintf("academic host %q not in %v", host, hosts)}
	}

	var msgs []string
	if u.User != nil || u.Host != u.Hostname() {
		msgs = append(msgs, "no port or userinfo allowed")
	} else if u.Hostname() != host {
		msgs = append(msgs, "host must be lowercase")
	}
	if u.Fragment != "" {
		msgs = append(msgs, "no fragment allowed")
	}
	if u.EscapedPath() != p.path {
		msgs = append(msgs, fmt.Sprintf("path must be %s, got %q", p.path, u.EscapedPath()))
	}
	// The app's url.Query() silently drops these pairs.
	if strings.Contains(u.RawQuery, ";") {
		msgs = append(msgs, "';' in the query string, use '&' (the app drops the pair)")
	}
	if badEscape.MatchString(u.RawQuery) {
		msgs = append(msgs, "invalid percent escape in the query string")
	}

	params := parseQuery(u.RawQuery)
	allowed := slices.Concat(p.required, p.optional)
	slices.Sort(allowed)
	for _, k := range params.keys {
		vals := params.values[k]
		if len(vals) > 1 {
			msgs = append(msgs, fmt.Sprintf("param %q given %d times", k, len(vals)))
		}
		switch {
		case k == "publisher":
			msgs = append(msgs, "param 'publisher' is not allowed, filter by issn (issue #50)")
		case !slices.Contains(allowed, k):
			msgs = append(msgs, fmt.Sprintf("param %q not allowed for %s (allowed: %v)", k, host, allowed))
		case strings.TrimSpace(vals[0]) == "":
			msgs = append(msgs, fmt.Sprintf("param %q is empty", k))
		}
	}
	for _, k := range p.required {
		if _, ok := params.values[k]; !ok {
			msgs = append(msgs, fmt.Sprintf("param %q is required for %s", k, host))
		}
	}
	if qv, ok := params.values["q"]; ok && strings.ContainsFunc(qv[0], func(r rune) bool {
		return r < 32 || r == 127
	}) {
		msgs = append(msgs, "control character in q")
	}

	if host == "api.crossref.org" {
		issn, hasISSN := params.values["issn"]
		_, hasQ := params.values["q"]
		if !hasQ && !hasISSN {
			msgs = append(msgs, "crossref needs q or issn")
		}
		if hasISSN && !issnPattern.MatchString(issn[0]) {
			msgs = append(msgs, fmt.Sprintf("issn %q is not NNNN-NNNN", issn[0]))
		}
	}
	if v, ok := params.values["open_access"]; ok && v[0] != "true" && v[0] != "false" {
		msgs = append(msgs, "open_access must be true or false")
	}
	if v, ok := params.values["min_citations"]; ok && !countPattern.MatchString(v[0]) {
		msgs = append(msgs, "min_citations must be a non-negative integer, no leading zeros")
	}
	return msgs
}
