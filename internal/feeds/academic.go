// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package feeds

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
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
		Client:                 NewClient(""),
		ArXivBaseURL:           "https://export.arxiv.org/api/query",
		CrossrefBaseURL:        "https://api.crossref.org/works",
		SemanticScholarBaseURL: "https://api.semanticscholar.org/graph/v1/paper/search/bulk",
	}
}

// FetchAcademic fetches one academic saved search through client and
// normalizes its results into the same Item model used by RSS ingest. A nil
// client means NewClient.
func FetchAcademic(
	ctx context.Context,
	client *http.Client,
	src Source,
	query AcademicQuery,
) ([]Item, error) {
	f := NewAcademicFetcher()
	f.Client = client
	if client == nil {
		f.Client = NewClient("")
	}
	return f.Fetch(ctx, src, query)
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

// Academic APIs answer 429 (rate limited) or 503 when they are busy. A request
// gets academicAttempts tries in all, waiting academicBackoff, then double that,
// capped at academicMaxWait. A Retry-After header in seconds replaces the wait.
const (
	academicAttempts = 3
	academicBackoff  = time.Second
	academicMaxWait  = 30 * time.Second
)

// doWithBackoff sends a GET that carries no body and retries it while the
// provider is busy. The last response is returned as is, for the caller to report.
func doWithBackoff(ctx context.Context, client *http.Client, req *http.Request) (*http.Response, error) {
	wait := academicBackoff
	for attempt := 1; ; attempt++ {
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		busy := resp.StatusCode == http.StatusTooManyRequests ||
			resp.StatusCode == http.StatusServiceUnavailable
		if !busy || attempt == academicAttempts {
			return resp, nil
		}

		delay := wait
		if seconds, perr := strconv.Atoi(resp.Header.Get("Retry-After")); perr == nil && seconds >= 0 {
			delay = time.Duration(seconds) * time.Second
		}
		delay = min(delay, academicMaxWait)
		_ = resp.Body.Close()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		wait *= 2
	}
}

func (f *AcademicFetcher) client() *http.Client {
	if f.Client != nil {
		return f.Client
	}
	return NewClient("")
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
