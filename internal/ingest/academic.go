// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/DanielBlei/rabbithole/internal/feeds"
)

// fetchAcademic dispatches academic saved-search URLs to the scholarly API
// adapters. The URL host selects the provider and query parameters describe
// the saved search.
//
// Examples:
//
//	https://arxiv.org/search?q=agentic+coding
//	https://api.crossref.org/works?q=llm&publisher=ACM
//	https://www.semanticscholar.org/search?q=code+agents&min_citations=5
func fetchAcademic(ctx context.Context, sources []feeds.Source) []feeds.Result {
	results := make([]feeds.Result, len(sources))

	for i, src := range sources {
		start := time.Now()
		query, err := academicQueryFromSource(src)

		var items []feeds.Item
		if err == nil {
			items, err = feeds.FetchAcademic(ctx, src, query)
		}

		results[i] = feeds.Result{
			Source:  src,
			Items:   items,
			Err:     err,
			Elapsed: time.Since(start),
			At:      time.Now(),
		}
	}

	return results
}

func academicQueryFromSource(src feeds.Source) (feeds.AcademicQuery, error) {
	u, err := url.Parse(src.URL)
	if err != nil {
		return feeds.AcademicQuery{}, fmt.Errorf(
			"parse academic source %q: %w",
			src.Name,
			err,
		)
	}

	host := strings.ToLower(u.Hostname())
	params := u.Query()

	var provider string

	switch host {
	case "arxiv.org", "www.arxiv.org", "export.arxiv.org":
		provider = "arxiv"

	case "crossref.org", "www.crossref.org", "api.crossref.org":
		provider = "crossref"

	case "semanticscholar.org", "www.semanticscholar.org", "api.semanticscholar.org":
		provider = "semantic_scholar"

	default:
		return feeds.AcademicQuery{}, fmt.Errorf(
			"unsupported academic source host %q",
			host,
		)
	}

	query := firstNonEmpty(
		params.Get("q"),
		params.Get("query"),
		params.Get("search_query"),
		params.Get("query.bibliographic"),
	)

	minCitations, err := optionalNonNegativeInt(
		params.Get("min_citations"),
		"min_citations",
	)
	if err != nil {
		return feeds.AcademicQuery{}, err
	}

	return feeds.AcademicQuery{
		Provider: provider,
		Query:    query,
		Publisher: firstNonEmpty(
			params.Get("publisher"),
			params.Get("query.publisher-name"),
		),
		Journal: firstNonEmpty(
			params.Get("journal"),
			params.Get("query.container-title"),
		),
		ISSN:         params.Get("issn"),
		OpenAccess:   parseBool(params.Get("open_access")),
		MinCitations: minCitations,
	}, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}

	return ""
}

func parseBool(value string) bool {
	parsed, err := strconv.ParseBool(strings.TrimSpace(value))
	return err == nil && parsed
}

func optionalNonNegativeInt(value, name string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}

	n, err := strconv.Atoi(value)
	if err != nil || n < 0 {
		return 0, fmt.Errorf(
			"%s must be a non-negative integer",
			name,
		)
	}

	return n, nil
}
