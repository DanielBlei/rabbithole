// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"testing"

	"github.com/DanielBlei/rabbithole/internal/feeds"
)

func TestAcademicQueryFromSource(t *testing.T) {
	tests := []struct {
		name       string
		url        string
		provider   string
		query      string
		publisher  string
		journal    string
		issn       string
		openAccess bool
		citations  int
		maxItems   int
	}{
		{
			name:     "arxiv",
			url:      "https://arxiv.org/search?q=agentic+coding",
			provider: "arxiv",
			query:    "agentic coding",
			maxItems: 25,
		},
		{
			name: "crossref",
			url: "https://api.crossref.org/works?" +
				"query.bibliographic=language+models&" +
				"publisher=ACM&journal=Communications&issn=0001-0782",
			provider:  "crossref",
			query:     "language models",
			publisher: "ACM",
			journal:   "Communications",
			issn:      "0001-0782",
		},
		{
			name: "semantic scholar",
			url: "https://www.semanticscholar.org/search?" +
				"q=code+agents&open_access=true&min_citations=5",
			provider:   "semantic_scholar",
			query:      "code agents",
			openAccess: true,
			citations:  5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := academicQueryFromSource(feeds.Source{
				Name: "papers",
				URL:  tt.url,
			})
			if err != nil {
				t.Fatal(err)
			}

			if got.Provider != tt.provider {
				t.Errorf(
					"Provider = %q, want %q",
					got.Provider,
					tt.provider,
				)
			}
			if got.Query != tt.query {
				t.Errorf("Query = %q, want %q", got.Query, tt.query)
			}
			if got.Publisher != tt.publisher {
				t.Errorf(
					"Publisher = %q, want %q",
					got.Publisher,
					tt.publisher,
				)
			}
			if got.Journal != tt.journal {
				t.Errorf(
					"Journal = %q, want %q",
					got.Journal,
					tt.journal,
				)
			}
			if got.ISSN != tt.issn {
				t.Errorf("ISSN = %q, want %q", got.ISSN, tt.issn)
			}
			if got.OpenAccess != tt.openAccess {
				t.Errorf(
					"OpenAccess = %v, want %v",
					got.OpenAccess,
					tt.openAccess,
				)
			}
			if got.MinCitations != tt.citations {
				t.Errorf(
					"MinCitations = %d, want %d",
					got.MinCitations,
					tt.citations,
				)
			}
		})
	}
}

func TestAcademicQueryFromSourceRejectsUnknownHost(t *testing.T) {
	_, err := academicQueryFromSource(feeds.Source{
		Name: "unknown",
		URL:  "https://example.com/search?q=paper",
	})

	if err == nil {
		t.Fatal("expected unsupported host error")
	}
}

func TestAcademicQueryFromSourceRejectsInvalidInteger(t *testing.T) {
	_, err := academicQueryFromSource(feeds.Source{
		Name: "papers",
		URL: "https://www.semanticscholar.org/search?" +
			"q=agents&min_citations=nope",
	})

	if err == nil {
		t.Fatal("expected invalid min_citations error")
	}
}
