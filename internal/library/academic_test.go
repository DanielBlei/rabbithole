// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package library

import (
	"strings"
	"testing"
)

func TestCheckAcademicURLValid(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "arxiv speculative decoding", raw: "https://arxiv.org/search?q=speculative+decoding"},
		{name: "arxiv kv cache", raw: "https://arxiv.org/search?q=kv+cache"},
		{name: "arxiv mixture of experts", raw: "https://arxiv.org/search?q=mixture+of+experts"},
		{name: "arxiv category and abstract", raw: "https://arxiv.org/search?q=cat:cs.DC+AND+abs:llm"},
		{name: "arxiv quantization", raw: "https://arxiv.org/search?q=cat:cs.LG+AND+abs:quantization"},
		{name: "arxiv rag", raw: "https://arxiv.org/search?q=retrieval+augmented+generation"},
		{name: "arxiv coding agents", raw: "https://arxiv.org/search?q=cat:cs.SE+AND+abs:agent"},
		{name: "arxiv title", raw: "https://arxiv.org/search?q=cat:cs.CL+AND+ti:benchmark+AND+abs:llm"},
		{name: "crossref cacm issn", raw: "https://api.crossref.org/works?issn=0001-0782"},
		{name: "crossref tosem issn", raw: "https://api.crossref.org/works?issn=1049-331X"},
		{name: "crossref surveys issn", raw: "https://api.crossref.org/works?issn=0360-0300"},
		{name: "crossref query and journal", raw: "https://api.crossref.org/works?q=llm&journal=Nature"},
		{name: "crossref issn with lowercase x", raw: "https://api.crossref.org/works?issn=0360-036x"},
		{
			name: "semanticscholar filters",
			raw:  "https://www.semanticscholar.org/search?q=code+agents&open_access=true&min_citations=5",
		},
		{name: "semanticscholar zero citations", raw: "https://www.semanticscholar.org/search?q=x&min_citations=0"},
		{name: "percent escape", raw: "https://arxiv.org/search?q=a%20b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := checkAcademicURL(tt.raw); got != nil {
				t.Errorf("checkAcademicURL(%q) = %q, want nil", tt.raw, got)
			}
		})
	}
}

func TestCheckAcademicURLInvalid(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string // substring of one returned message
	}{
		{name: "rule 1 unparseable", raw: "https://arxiv.org/%zz?q=x", want: "unparseable url:"},
		{name: "rule 2 unknown host", raw: "https://example.com/search?q=x",
			want: `academic host "example.com" not in [api.crossref.org arxiv.org www.semanticscholar.org]`},
		{name: "rule 2 academic alias not a provider", raw: "https://export.arxiv.org/search?q=x",
			want: `academic host "export.arxiv.org" not in`},
		{name: "rule 3 port", raw: "https://arxiv.org:443/search?q=x", want: "no port or userinfo allowed"},
		{name: "rule 3 empty port", raw: "https://arxiv.org:/search?q=x", want: "no port or userinfo allowed"},
		{name: "rule 3 userinfo", raw: "https://user@arxiv.org/search?q=x", want: "no port or userinfo allowed"},
		{name: "rule 3 uppercase host", raw: "https://ArXiv.org/search?q=x", want: "host must be lowercase"},
		{name: "rule 4 fragment", raw: "https://arxiv.org/search?q=x#top", want: "no fragment allowed"},
		{name: "rule 5 wrong path", raw: "https://arxiv.org/x?q=x", want: `path must be /search, got "/x"`},
		{name: "rule 5 escaped path", raw: "https://arxiv.org/%73earch?q=x",
			want: `path must be /search, got "/%73earch"`},
		{name: "rule 6 semicolon", raw: "https://arxiv.org/search?q=x;y", want: "';' in the query string"},
		{name: "rule 7 bad escape letters", raw: "https://arxiv.org/search?q=%zz", want: "invalid percent escape"},
		{name: "rule 7 truncated escape", raw: "https://arxiv.org/search?q=%4", want: "invalid percent escape"},
		{name: "rule 9 repeated param", raw: "https://arxiv.org/search?q=a&q=b", want: `param "q" given 2 times`},
		{name: "rule 9 publisher", raw: "https://api.crossref.org/works?issn=0001-0782&publisher=acm",
			want: "param 'publisher' is not allowed"},
		{name: "rule 9 unknown param", raw: "https://arxiv.org/search?q=x&foo=1",
			want: `param "foo" not allowed for arxiv.org (allowed: [q])`},
		{name: "rule 9 empty param", raw: "https://arxiv.org/search?q=", want: `param "q" is empty`},
		{name: "rule 9 whitespace param", raw: "https://arxiv.org/search?q=+", want: `param "q" is empty`},
		{
			name: "rule 10 missing required q",
			raw:  "https://arxiv.org/search",
			want: `param "q" is required for arxiv.org`,
		},
		{name: "rule 11 control character", raw: "https://arxiv.org/search?q=a%01b", want: "control character in q"},
		{name: "rule 11 delete character", raw: "https://arxiv.org/search?q=a%7Fb", want: "control character in q"},
		{name: "rule 12 crossref without q or issn", raw: "https://api.crossref.org/works?journal=Nature",
			want: "crossref needs q or issn"},
		{name: "rule 12 bad issn", raw: "https://api.crossref.org/works?issn=0001-078",
			want: `issn "0001-078" is not NNNN-NNNN`},
		{name: "rule 13 bad open_access", raw: "https://www.semanticscholar.org/search?q=x&open_access=yes",
			want: "open_access must be true or false"},
		{name: "rule 14 leading zero", raw: "https://www.semanticscholar.org/search?q=x&min_citations=05",
			want: "min_citations must be a non-negative integer, no leading zeros"},
		{name: "rule 14 negative", raw: "https://www.semanticscholar.org/search?q=x&min_citations=-1",
			want: "min_citations must be a non-negative integer, no leading zeros"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := checkAcademicURL(tt.raw)
			for _, msg := range got {
				if strings.Contains(msg, tt.want) {
					return
				}
			}
			t.Errorf("checkAcademicURL(%q) = %q, want a message containing %q", tt.raw, got, tt.want)
		})
	}
}

func TestAcademicHostsCoversProviders(t *testing.T) {
	for host := range providers {
		if !academicHosts[host] {
			t.Errorf("academicHosts missing provider host %q", host)
		}
	}
}
