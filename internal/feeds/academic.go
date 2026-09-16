// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package feeds

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// AcademicQuery describes a saved scholarly search without expanding the
// general Source model with provider-specific fields.
type AcademicQuery struct {
	Provider     string
	Query        string
	Publisher    string
	Journal      string
	ISSN         string
	OpenAccess   bool
	MinCitations int
}

// AcademicFetcher owns the HTTP client and provider endpoints. Endpoint fields
// make provider behavior deterministic under httptest.
type AcademicFetcher struct {
	Client                 *http.Client
	ArXivBaseURL           string
	CrossrefBaseURL        string
	SemanticScholarBaseURL string
}

// NewAcademicFetcher returns a fetcher configured for the public scholarly APIs.
func NewAcademicFetcher() *AcademicFetcher {
	return &AcademicFetcher{
		Client:                 &http.Client{},
		ArXivBaseURL:           "https://export.arxiv.org/api/query",
		CrossrefBaseURL:        "https://api.crossref.org/works",
		SemanticScholarBaseURL: "https://api.semanticscholar.org/graph/v1/paper/search/bulk",
	}
}

// FetchAcademic fetches one academic saved search and normalizes its results
// into the same Item model used by RSS ingest.
func FetchAcademic(
	ctx context.Context,
	src Source,
	query AcademicQuery,
) ([]Item, error) {
	return NewAcademicFetcher().Fetch(ctx, src, query)
}

// Fetch executes one academic provider query.
func (f *AcademicFetcher) Fetch(
	ctx context.Context,
	src Source,
	query AcademicQuery,
) ([]Item, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	switch normalizeAcademicProvider(query.Provider) {
	case "arxiv":
		return f.fetchArXiv(ctx, src, query)
	case "crossref":
		return f.fetchCrossref(ctx, src, query)
	case "semantic_scholar":
		return f.fetchSemanticScholar(ctx, src, query)
	default:
		return nil, fmt.Errorf(
			"unsupported academic provider %q",
			query.Provider,
		)
	}
}

func (f *AcademicFetcher) client() *http.Client {
	if f.Client != nil {
		return f.Client
	}
	return http.DefaultClient
}

func normalizeAcademicProvider(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "semantic-scholar", "semanticscholar", "s2":
		return "semantic_scholar"
	default:
		return strings.ToLower(strings.TrimSpace(provider))
	}
}

const academicFetchLimit = 100

func mergeAcademicTags(base, extra []string) []string {
	seen := make(map[string]struct{}, len(base)+len(extra))
	out := make([]string, 0, len(base)+len(extra))

	for _, group := range [][]string{base, extra} {
		for _, tag := range group {
			tag = strings.TrimSpace(tag)
			if tag == "" {
				continue
			}

			key := strings.ToLower(tag)
			if _, ok := seen[key]; ok {
				continue
			}

			seen[key] = struct{}{}
			out = append(out, tag)
		}
	}

	return out
}

func academicItemID(doi, arxiv, providerID, link string) string {
	if doi = strings.TrimSpace(doi); doi != "" {
		return makeID("doi:"+strings.ToLower(doi), link)
	}

	if arxiv = arxivID(arxiv); arxiv != "" {
		return makeID("arxiv:"+strings.ToLower(arxiv), link)
	}

	return makeID(strings.TrimSpace(providerID), link)
}
