// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package library

import (
	"strings"
	"testing"
)

// rssBase is a valid rss source, copied from library/rss.yaml.
func rssBase() Source {
	return Source{
		Name:        "Kubernetes blog",
		URL:         "https://kubernetes.io/feed.xml",
		Description: "Release news, SIG updates and deep dives from the Kubernetes project.",
		Labels:      []string{"Kubernetes", "Infra", "Open Source"},
	}
}

func hasMessage(msgs []string, sub string) bool {
	for _, m := range msgs {
		if strings.Contains(m, sub) {
			return true
		}
	}
	return false
}

func TestCheckSource(t *testing.T) {
	tests := []struct {
		name string
		t    Type
		mut  func(*Source)
		want string // substring of one message; empty means valid
	}{
		{name: "valid rss", t: TypeRSS, want: ""},
		{
			name: "valid academic",
			t:    TypeAcademic,
			mut: func(s *Source) {
				s.Name = "arXiv: speculative decoding"
				s.URL = "https://arxiv.org/search?q=speculative+decoding"
				s.Description = "Newest arXiv papers on speculative decoding."
				s.Labels = []string{"AI", "Inference", "Papers"}
			},
			want: "",
		},
		{name: "name blank", t: TypeRSS, mut: func(s *Source) { s.Name = "  " },
			want: "name must be a non-empty string"},
		{name: "description blank", t: TypeRSS, mut: func(s *Source) { s.Description = "" },
			want: "description must be a non-empty string"},
		{name: "labels empty", t: TypeRSS, mut: func(s *Source) { s.Labels = nil },
			want: "labels must be a non-empty list"},
		{name: "label lowercase start", t: TypeRSS, mut: func(s *Source) { s.Labels = []string{"ai"} },
			want: `label "ai" must be ASCII words`},
		{name: "label C++", t: TypeRSS, mut: func(s *Source) { s.Labels = []string{"C++"} },
			want: `label "C++" must be ASCII words`},
		{name: "label .NET", t: TypeRSS, mut: func(s *Source) { s.Labels = []string{".NET"} },
			want: `label ".NET" must be ASCII words`},
		{name: "label double space", t: TypeRSS, mut: func(s *Source) { s.Labels = []string{"Open  Source"} },
			want: `label "Open  Source" must be ASCII words`},
		{name: "label comma", t: TypeRSS, mut: func(s *Source) { s.Labels = []string{"AI,ML"} },
			want: `label "AI,ML" must be ASCII words`},
		{name: "label leading space", t: TypeRSS, mut: func(s *Source) { s.Labels = []string{" AI"} },
			want: `label " AI" must be ASCII words`},
		{name: "label trailing space", t: TypeRSS, mut: func(s *Source) { s.Labels = []string{"AI "} },
			want: `label "AI " must be ASCII words`},
		{name: "label non-ASCII", t: TypeRSS, mut: func(s *Source) { s.Labels = []string{"Ärzte"} },
			want: `label "Ärzte" must be ASCII words`},
		{name: "label duplicate slug", t: TypeRSS,
			mut:  func(s *Source) { s.Labels = []string{"Open Source", "OPEN SOURCE"} },
			want: `duplicate label "OPEN SOURCE"`},
		{name: "url empty", t: TypeRSS, mut: func(s *Source) { s.URL = "" },
			want: "url must be a non-empty string"},
		{name: "url whitespace", t: TypeRSS, mut: func(s *Source) { s.URL = "https://ex ample.com" },
			want: "url must not contain whitespace"},
		{name: "url non-ASCII", t: TypeRSS, mut: func(s *Source) { s.URL = "https://exämple.com/feed" },
			want: "url must be ASCII"},
		{name: "url http", t: TypeRSS, mut: func(s *Source) { s.URL = "http://example.com/feed" },
			want: "url must be https"},
		{name: "url no host", t: TypeRSS, mut: func(s *Source) { s.URL = "https://" },
			want: "url must have a host"},
		{name: "url unparseable", t: TypeRSS, mut: func(s *Source) { s.URL = "https://[::1" },
			want: "unparseable url"},
		{name: "academic url delegated", t: TypeAcademic,
			mut:  func(s *Source) { s.URL = "https://arxiv.org/search" },
			want: `param "q" is required`},
		{name: "academic host in rss file", t: TypeRSS,
			mut:  func(s *Source) { s.URL = "https://export.arxiv.org/rss/cs.AI" },
			want: "academic host export.arxiv.org belongs in academic.yaml"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			src := rssBase()
			if tc.mut != nil {
				tc.mut(&src)
			}
			got := checkSource(tc.t, src)
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("checkSource() = %q, want none", got)
				}
				return
			}
			if !hasMessage(got, tc.want) {
				t.Fatalf("checkSource() = %q, want a message containing %q", got, tc.want)
			}
		})
	}
}

func TestCheckSourceAcademicHostsInRSS(t *testing.T) {
	for host := range academicHosts {
		t.Run(host, func(t *testing.T) {
			src := rssBase()
			src.URL = "https://" + host + "/feed"
			got := checkSource(TypeRSS, src)
			if want := "academic host " + host + " belongs in academic.yaml"; !hasMessage(got, want) {
				t.Fatalf("checkSource() = %q, want a message containing %q", got, want)
			}
		})
	}
}

func TestCheckLibrary(t *testing.T) {
	ref := func(file string, line int, name, url string, labels ...string) sourceRef {
		return sourceRef{
			file: file,
			line: line,
			path: "blogs/" + name,
			src:  Source{Name: name, URL: url, Description: "d", Labels: labels},
		}
	}

	t.Run("duplicate name across files", func(t *testing.T) {
		refs := []sourceRef{
			ref("rss.yaml", 5, "Go blog", "https://go.dev/blog/feed.atom", "Go"),
			ref("academic.yaml", 9, " go BLOG ", "https://go.dev/other", "Go"),
		}
		diags, dropped := checkLibrary(refs)
		if len(diags) != 1 || diags[0].Message != "duplicate name (also in rss.yaml)" {
			t.Fatalf("diags = %+v, want one duplicate name finding", diags)
		}
		if !dropped[1] || len(dropped) != 1 {
			t.Fatalf("dropped = %v, want {1: true}", dropped)
		}
	})

	t.Run("duplicate name by full case folding", func(t *testing.T) {
		refs := []sourceRef{
			ref("rss.yaml", 5, "Stra\u00dfe Blog", "https://a.example/one", "Go"),
			ref("academic.yaml", 9, "Strasse Blog", "https://b.example/two", "Go"),
		}
		diags, dropped := checkLibrary(refs)
		if len(diags) != 1 || diags[0].Message != "duplicate name (also in rss.yaml)" {
			t.Fatalf("diags = %+v, want one duplicate name finding", diags)
		}
		if !dropped[1] || len(dropped) != 1 {
			t.Fatalf("dropped = %v, want {1: true}", dropped)
		}
	})

	t.Run("duplicate url differing in host case and trailing slash", func(t *testing.T) {
		refs := []sourceRef{
			ref("rss.yaml", 5, "First", "https://Example.com/feed", "Go"),
			ref("rss.yaml", 9, "Second", "https://example.com/feed/", "Go"),
		}
		diags, dropped := checkLibrary(refs)
		if len(diags) != 1 || diags[0].Message != "duplicate url (also in rss.yaml)" {
			t.Fatalf("diags = %+v, want one duplicate url finding", diags)
		}
		if !dropped[1] || len(dropped) != 1 {
			t.Fatalf("dropped = %v, want {1: true}", dropped)
		}
	})

	t.Run("url differing only in path case is not a duplicate", func(t *testing.T) {
		refs := []sourceRef{
			ref("rss.yaml", 5, "First", "https://example.com/Feed", "Go"),
			ref("rss.yaml", 9, "Second", "https://example.com/feed", "Go"),
		}
		diags, dropped := checkLibrary(refs)
		if len(diags) != 0 || len(dropped) != 0 {
			t.Fatalf("diags = %+v, dropped = %v, want neither", diags, dropped)
		}
	})

	t.Run("name-dropped url not recorded", func(t *testing.T) {
		refs := []sourceRef{
			ref("rss.yaml", 5, "X", "https://a.example/one", "Go"),
			ref("rss.yaml", 9, "X", "https://b.example/two", "Go"),
			ref("rss.yaml", 13, "Y", "https://b.example/two", "Go"),
		}
		diags, dropped := checkLibrary(refs)
		if len(diags) != 1 || diags[0].Message != "duplicate name (also in rss.yaml)" {
			t.Fatalf("diags = %+v, want only the name repeat", diags)
		}
		if !dropped[1] || len(dropped) != 1 {
			t.Fatalf("dropped = %v, want {1: true}", dropped)
		}
	})

	t.Run("label slug spelled two ways", func(t *testing.T) {
		refs := []sourceRef{
			ref("rss.yaml", 5, "First", "https://a.example/one", "Open Source"),
			ref("academic.yaml", 9, "Second", "https://b.example/two", "Open source"),
		}
		diags, dropped := checkLibrary(refs)
		want := `label "Open source" has the same slug as "Open Source" (in rss.yaml)`
		if len(diags) != 1 || diags[0].Message != want {
			t.Fatalf("diags = %+v, want %q", diags, want)
		}
		if dropped == nil || len(dropped) != 0 {
			t.Fatalf("dropped = %v, want empty non-nil map", dropped)
		}
	})

	t.Run("clean refs", func(t *testing.T) {
		refs := []sourceRef{
			ref("rss.yaml", 5, "First", "https://a.example/one", "Open Source"),
			ref("rss.yaml", 9, "Second", "https://b.example/two", "Open Source"),
		}
		diags, dropped := checkLibrary(refs)
		if len(diags) != 0 {
			t.Fatalf("diags = %+v, want none", diags)
		}
		if dropped == nil || len(dropped) != 0 {
			t.Fatalf("dropped = %v, want empty non-nil map", dropped)
		}
	})

	t.Run("diagnostic location", func(t *testing.T) {
		refs := []sourceRef{
			ref("rss.yaml", 5, "Go blog", "https://go.dev/blog/feed.atom", "Go"),
			ref("academic.yaml", 12, "go blog", "https://go.dev/other", "Go"),
		}
		diags, _ := checkLibrary(refs)
		if len(diags) != 1 {
			t.Fatalf("diags = %+v, want one finding", diags)
		}
		d := diags[0]
		if d.File != "academic.yaml" || d.Line != 12 || d.Path != "blogs/go blog" {
			t.Fatalf("diag location = %s:%d %q, want academic.yaml:12 blogs/go blog", d.File, d.Line, d.Path)
		}
	})
}
