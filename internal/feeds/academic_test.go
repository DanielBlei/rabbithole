// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package feeds

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAcademicFetcherRejectsUnsupportedProvider(t *testing.T) {
	f := NewAcademicFetcher()

	_, err := f.Fetch(
		context.Background(),
		Source{Name: "papers"},
		AcademicQuery{Provider: "unknown"},
	)
	if err == nil {
		t.Fatal("expected unsupported provider error")
	}
}

func TestNormalizeAcademicProvider(t *testing.T) {
	for input, want := range map[string]string{
		"arxiv":            "arxiv",
		" CrossRef ":       "crossref",
		"semantic-scholar": "semantic_scholar",
		"semanticscholar":  "semantic_scholar",
		"S2":               "semantic_scholar",
	} {
		if got := normalizeAcademicProvider(input); got != want {
			t.Errorf("normalizeAcademicProvider(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestMergeAcademicTags(t *testing.T) {
	got := mergeAcademicTags(
		[]string{"AI", " agents ", ""},
		[]string{"ai", "LLM", "Agents"},
	)

	want := []string{"AI", "agents", "LLM"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("tags = %#v, want %#v", got, want)
	}
}

func TestFetchArXiv(t *testing.T) {
	const atom = `<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <entry>
    <id>http://arxiv.org/abs/2609.12345v2</id>
    <updated>2026-09-15T12:30:00Z</updated>
    <published>2026-09-14T10:20:30Z</published>
    <title>
      Reliable   Agentic
      Coding
    </title>
    <summary>
      An abstract about reliable coding agents.
    </summary>
    <link href="https://arxiv.org/abs/2609.12345v2" rel="alternate" type="text/html"/>
    <category term="cs.AI"/>
    <category term="cs.SE"/>
  </entry>
</feed>`

	var requestSeen bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestSeen = true

		if got := r.URL.Query().Get("search_query"); got != `all:"agentic coding"` {
			t.Errorf("search_query = %q", got)
		}
		if got := r.URL.Query().Get("max_results"); got != "100" {
			t.Errorf("max_results = %q, want 100", got)
		}
		if got := r.URL.Query().Get("sortBy"); got != "submittedDate" {
			t.Errorf("sortBy = %q", got)
		}
		if got := r.URL.Query().Get("sortOrder"); got != "descending" {
			t.Errorf("sortOrder = %q", got)
		}
		if got := r.Header.Get("User-Agent"); got != "rabbithole/academic-reader" {
			t.Errorf("User-Agent = %q", got)
		}

		w.Header().Set("Content-Type", "application/atom+xml")
		_, _ = fmt.Fprint(w, atom)
	}))
	defer srv.Close()

	f := NewAcademicFetcher()
	f.ArXivBaseURL = srv.URL

	src := Source{
		Name: "arXiv AI",
		Tags: []string{"research", "CS.ai"},
	}

	items, err := f.Fetch(
		context.Background(),
		src,
		AcademicQuery{
			Provider: "arxiv",
			Query:    "agentic coding",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !requestSeen {
		t.Fatal("server did not receive request")
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}

	item := items[0]

	if item.Source != "arXiv AI" {
		t.Errorf("Source = %q", item.Source)
	}
	if item.Title != "Reliable Agentic Coding" {
		t.Errorf("Title = %q", item.Title)
	}
	if item.Link != "https://arxiv.org/abs/2609.12345v2" {
		t.Errorf("Link = %q", item.Link)
	}
	if item.Published.Format(time.RFC3339) != "2026-09-14T10:20:30Z" {
		t.Errorf("Published = %v", item.Published)
	}

	wantID := academicItemID(
		"",
		"2609.12345",
		"",
		"https://arxiv.org/abs/2609.12345v2",
	)
	if item.ID != wantID {
		t.Errorf("ID = %q, want %q", item.ID, wantID)
	}

	wantTags := []string{"research", "CS.ai", "cs.SE"}
	if strings.Join(item.Tags, "|") != strings.Join(wantTags, "|") {
		t.Errorf("Tags = %#v, want %#v", item.Tags, wantTags)
	}
}

func TestArXivRevisionProducesStableID(t *testing.T) {
	id1 := arxivID("https://arxiv.org/abs/2609.12345v1")
	id2 := arxivID("https://arxiv.org/abs/2609.12345v7")

	if id1 != "2609.12345" || id2 != id1 {
		t.Fatalf("revision IDs = %q, %q; want stable 2609.12345", id1, id2)
	}
}

func TestFetchArXivRequiresQuery(t *testing.T) {
	f := NewAcademicFetcher()

	_, err := f.fetchArXiv(
		context.Background(),
		Source{Name: "arXiv"},
		AcademicQuery{},
	)
	if err == nil {
		t.Fatal("expected missing-query error")
	}
}

func TestFetchCrossrefFiltersPublisherAndCanonicalizesDOI(t *testing.T) {
	const response = `{
	  "message": {
	    "items": [
	      {
	        "DOI": "10.1234/example",
	        "URL": "https://example.test/article",
	        "title": [" Reliable   LLM Systems "],
	        "abstract": "Useful abstract.",
	        "container-title": ["Journal of AI Systems"],
	        "publisher": "ACM",
	        "published": {"date-parts": [[2026, 9, 12]]},
	        "subject": ["Artificial Intelligence", "Software Engineering"]
	      },
	      {
	        "DOI": "10.9999/wrong",
	        "URL": "https://example.test/wrong",
	        "title": ["Wrong publisher"],
	        "publisher": "Elsevier",
	        "published": {"date-parts": [[2026, 9, 11]]}
	      }
	    ]
	  }
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()

		if got := q.Get("query.bibliographic"); got != "language models" {
			t.Errorf("query.bibliographic = %q", got)
		}
		if got := q.Get("query.publisher-name"); got != "acm" {
			t.Errorf("query.publisher-name = %q", got)
		}
		if got := q.Get("query.container-title"); got != "AI Journal" {
			t.Errorf("query.container-title = %q", got)
		}
		if got := q.Get("filter"); got != "issn:0001-0782" {
			t.Errorf("filter = %q", got)
		}
		if got := q.Get("rows"); got != "100" {
			t.Errorf("rows = %q", got)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, response)
	}))
	defer srv.Close()

	// The limiter is process-global. Reset it so this unit test does not
	// inherit timing state from another Crossref test.
	crossrefRequestMu.Lock()
	crossrefLastRequest = time.Time{}
	crossrefRequestMu.Unlock()

	f := NewAcademicFetcher()
	f.CrossrefBaseURL = srv.URL

	items, err := f.Fetch(
		context.Background(),
		Source{
			Name: "Crossref",
			Tags: []string{"papers"},
		},
		AcademicQuery{
			Provider:  "crossref",
			Query:     "language models",
			Publisher: "acm",
			Journal:   "AI Journal",
			ISSN:      "0001-0782",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1 after publisher filtering", len(items))
	}

	item := items[0]

	if item.Title != "Reliable LLM Systems" {
		t.Errorf("Title = %q", item.Title)
	}
	if item.Link != "https://doi.org/10.1234/example" {
		t.Errorf("Link = %q", item.Link)
	}
	if item.Published.Format("2006-01-02") != "2026-09-12" {
		t.Errorf("Published = %v", item.Published)
	}

	wantID := academicItemID(
		"10.1234/example",
		"",
		"",
		"https://doi.org/10.1234/example",
	)
	if item.ID != wantID {
		t.Errorf("ID = %q, want %q", item.ID, wantID)
	}

	for _, want := range []string{
		"papers",
		"Artificial Intelligence",
		"Software Engineering",
		"ACM",
		"Journal of AI Systems",
	} {
		if !containsString(item.Tags, want) {
			t.Errorf("Tags %#v missing %q", item.Tags, want)
		}
	}
}

func TestFetchCrossrefRetries429(t *testing.T) {
	var requests atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := requests.Add(1)

		if n == 1 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{
		  "message": {
		    "items": [{
		      "DOI": "10.1234/retry",
		      "title": ["Retry succeeds"],
		      "publisher": "ACM",
		      "published": {"date-parts": [[2026, 9, 1]]}
		    }]
		  }
		}`)
	}))
	defer srv.Close()

	crossrefRequestMu.Lock()
	crossrefLastRequest = time.Time{}
	crossrefRequestMu.Unlock()

	f := NewAcademicFetcher()
	f.CrossrefBaseURL = srv.URL

	items, err := f.Fetch(
		context.Background(),
		Source{Name: "Crossref"},
		AcademicQuery{
			Provider: "crossref",
			Query:    "agents",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 {
		t.Fatalf("requests = %d, want 2", requests.Load())
	}
	if len(items) != 1 || items[0].Title != "Retry succeeds" {
		t.Fatalf("items = %+v", items)
	}
}

func TestCrossrefPublishedFallbacks(t *testing.T) {
	tests := []struct {
		name string
		work crossrefWork
		want string
	}{
		{
			name: "published",
			work: crossrefWork{
				Published: crossrefDate{DateParts: [][]int{{2026, 9, 15}}},
			},
			want: "2026-09-15",
		},
		{
			name: "online",
			work: crossrefWork{
				PublishedOnline: crossrefDate{
					DateParts: [][]int{{2026, 8}},
				},
			},
			want: "2026-08-01",
		},
		{
			name: "print",
			work: crossrefWork{
				PublishedPrint: crossrefDate{
					DateParts: [][]int{{2025}},
				},
			},
			want: "2025-01-01",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := crossrefPublished(tt.work)
			if got.Format("2006-01-02") != tt.want {
				t.Errorf("date = %s, want %s", got.Format("2006-01-02"), tt.want)
			}
		})
	}
}

func TestFetchSemanticScholarFiltersAndNormalizes(t *testing.T) {
	const response = `{
	  "data": [
	    {
	      "paperId": "s2-good",
	      "url": "https://www.semanticscholar.org/paper/s2-good",
	      "title": " Reliable   Coding Agents ",
	      "abstract": "An abstract.",
	      "publicationDate": "2026-09-10",
	      "year": 2026,
	      "citationCount": 12,
	      "externalIds": {
	        "DOI": "10.1234/example",
	        "ArXiv": "2609.12345"
	      },
	      "openAccessPdf": {
	        "url": "https://example.test/paper.pdf"
	      },
	      "fieldsOfStudy": ["Computer Science"],
	      "s2FieldsOfStudy": [
	        {"category": "Artificial Intelligence"}
	      ]
	    },
	    {
	      "paperId": "s2-closed",
	      "title": "Closed paper",
	      "citationCount": 100,
	      "externalIds": {},
	      "openAccessPdf": null
	    },
	    {
	      "paperId": "s2-low-citations",
	      "title": "Low citation paper",
	      "citationCount": 2,
	      "externalIds": {},
	      "openAccessPdf": {
	        "url": "https://example.test/low.pdf"
	      }
	    }
	  ]
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()

		if got := q.Get("query"); got != "coding agents" {
			t.Errorf("query = %q", got)
		}
		if got := q.Get("limit"); got != "100" {
			t.Errorf("limit = %q", got)
		}
		if got := q.Get("openAccessPdf"); got != "true" {
			t.Errorf("openAccessPdf = %q", got)
		}
		if got := q.Get("minCitationCount"); got != "5" {
			t.Errorf("minCitationCount = %q", got)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, response)
	}))
	defer srv.Close()

	f := NewAcademicFetcher()
	f.SemanticScholarBaseURL = srv.URL

	items, err := f.Fetch(
		context.Background(),
		Source{
			Name: "Semantic Scholar",
			Tags: []string{"research"},
		},
		AcademicQuery{
			Provider:     "semantic_scholar",
			Query:        "coding agents",
			OpenAccess:   true,
			MinCitations: 5,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}

	item := items[0]

	if item.Title != "Reliable Coding Agents" {
		t.Errorf("Title = %q", item.Title)
	}
	if item.Link != "https://doi.org/10.1234/example" {
		t.Errorf("Link = %q", item.Link)
	}
	if item.Published.Format("2006-01-02") != "2026-09-10" {
		t.Errorf("Published = %v", item.Published)
	}

	for _, want := range []string{
		"research",
		"Computer Science",
		"Artificial Intelligence",
	} {
		if !containsString(item.Tags, want) {
			t.Errorf("Tags %#v missing %q", item.Tags, want)
		}
	}
}

func TestSemanticScholarArXivFallback(t *testing.T) {
	item, ok := semanticPaperItem(
		Source{Name: "S2"},
		semanticPaper{
			PaperID: "paper-1",
			Title:   "Paper",
			ExternalIDs: semanticExternalIDs{
				ArXiv: "2609.54321",
			},
			Year: 2026,
		},
	)
	if !ok {
		t.Fatal("paper rejected")
	}

	if item.Link != "https://arxiv.org/abs/2609.54321" {
		t.Errorf("Link = %q", item.Link)
	}
	if item.Published.Format("2006-01-02") != "2026-01-01" {
		t.Errorf("Published = %v", item.Published)
	}
}

func TestDOIIdentityAcrossProviders(t *testing.T) {
	doi := "10.1234/example"
	link := "https://doi.org/" + doi

	crossrefID := academicItemID(doi, "", link, link)

	s2, ok := semanticPaperItem(
		Source{Name: "S2"},
		semanticPaper{
			PaperID: "provider-specific-id",
			Title:   "Same paper",
			ExternalIDs: semanticExternalIDs{
				DOI: doi,
			},
		},
	)
	if !ok {
		t.Fatal("Semantic Scholar paper rejected")
	}

	if crossrefID != s2.ID {
		t.Fatalf(
			"same DOI produced different IDs: crossref=%q semantic=%q",
			crossrefID,
			s2.ID,
		)
	}
}

func TestAcademicProviderHTTPAndDecodeErrors(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		setURL   func(*AcademicFetcher, string)
		body     string
		status   int
	}{
		{
			name:     "arxiv status",
			provider: "arxiv",
			setURL: func(f *AcademicFetcher, u string) {
				f.ArXivBaseURL = u
			},
			status: http.StatusBadGateway,
		},
		{
			name:     "crossref invalid json",
			provider: "crossref",
			setURL: func(f *AcademicFetcher, u string) {
				f.CrossrefBaseURL = u
			},
			status: http.StatusOK,
			body:   "{",
		},
		{
			name:     "semantic invalid json",
			provider: "semantic_scholar",
			setURL: func(f *AcademicFetcher, u string) {
				f.SemanticScholarBaseURL = u
			},
			status: http.StatusOK,
			body:   "{",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(tt.status)
					_, _ = fmt.Fprint(w, tt.body)
				},
			))
			defer srv.Close()

			if tt.provider == "crossref" {
				crossrefRequestMu.Lock()
				crossrefLastRequest = time.Time{}
				crossrefRequestMu.Unlock()
			}

			f := NewAcademicFetcher()
			tt.setURL(f, srv.URL)

			_, err := f.Fetch(
				context.Background(),
				Source{Name: "test"},
				AcademicQuery{
					Provider: tt.provider,
					Query:    "agents",
				},
			)
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestAcademicItemIDNormalizesDOICase(t *testing.T) {
	upper := academicItemID(
		"10.1234/ABC.Def",
		"",
		"",
		"https://doi.org/10.1234/ABC.Def",
	)
	lower := academicItemID(
		"10.1234/abc.def",
		"",
		"",
		"https://doi.org/10.1234/abc.def",
	)

	if upper != lower {
		t.Fatalf("DOI case changed identity: %q != %q", upper, lower)
	}
}

func TestAcademicItemIDMatchesArXivAcrossProviders(t *testing.T) {
	arxiv := academicItemID(
		"",
		"https://arxiv.org/abs/2609.54321v3",
		"",
		"https://arxiv.org/abs/2609.54321v3",
	)

	s2 := academicItemID(
		"",
		"2609.54321",
		"semantic-provider-id",
		"https://arxiv.org/abs/2609.54321",
	)

	if arxiv != s2 {
		t.Fatalf("same arXiv paper produced different IDs: %q != %q", arxiv, s2)
	}
}
