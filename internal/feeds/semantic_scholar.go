// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package feeds

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type semanticSearchResponse struct {
	Data []semanticPaper `json:"data"`
}

type semanticPaper struct {
	PaperID         string              `json:"paperId"`
	URL             string              `json:"url"`
	Title           string              `json:"title"`
	Abstract        string              `json:"abstract"`
	PublicationDate string              `json:"publicationDate"`
	Year            int                 `json:"year"`
	CitationCount   int                 `json:"citationCount"`
	ExternalIDs     semanticExternalIDs `json:"externalIds"`
	OpenAccessPDF   semanticPDF         `json:"openAccessPdf"`
	FieldsOfStudy   []string            `json:"fieldsOfStudy"`
	S2Fields        []semanticField     `json:"s2FieldsOfStudy"`
}

type semanticExternalIDs struct {
	DOI   string `json:"DOI"`
	ArXiv string `json:"ArXiv"`
}

type semanticPDF struct {
	URL string `json:"url"`
}

type semanticField struct {
	Category string `json:"category"`
}

func (f *AcademicFetcher) fetchSemanticScholar(
	ctx context.Context,
	src Source,
	query AcademicQuery,
) ([]Item, error) {
	base := f.SemanticScholarBaseURL
	if base == "" {
		base = "https://api.semanticscholar.org/graph/v1/paper/search/bulk"
	}

	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("semantic scholar endpoint: %w", err)
	}

	search := strings.TrimSpace(query.Query)
	if search == "" {
		return nil, fmt.Errorf("semantic scholar query is required")
	}

	q := u.Query()
	q.Set("query", search)
	q.Set("limit", strconv.Itoa(min(academicFetchLimit, 100)))
	q.Set(
		"fields",
		"title,abstract,url,year,publicationDate,citationCount,"+
			"externalIds,openAccessPdf,fieldsOfStudy,s2FieldsOfStudy",
	)
	q.Set("sort", "publicationDate:desc")

	if query.OpenAccess {
		q.Set("openAccessPdf", "true")
	}
	if query.MinCitations > 0 {
		q.Set("minCitationCount", strconv.Itoa(query.MinCitations))
	}

	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "rabbithole/academic-reader")

	resp, err := f.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf(
			"semantic scholar returned %s",
			resp.Status,
		)
	}

	var doc semanticSearchResponse
	if err := json.NewDecoder(
		io.LimitReader(resp.Body, 30<<20),
	).Decode(&doc); err != nil {
		return nil, fmt.Errorf(
			"decode semantic scholar json: %w",
			err,
		)
	}

	items := make([]Item, 0, len(doc.Data))
	for _, paper := range doc.Data {
		if item, ok := semanticPaperItem(src, paper); ok {
			if query.OpenAccess &&
				strings.TrimSpace(paper.OpenAccessPDF.URL) == "" {
				continue
			}
			if query.MinCitations > 0 &&
				paper.CitationCount < query.MinCitations {
				continue
			}

			items = append(items, item)
		}
	}

	return items, nil
}

func semanticPaperItem(src Source, paper semanticPaper) (Item, bool) {
	title := strings.TrimSpace(paper.Title)
	id := strings.TrimSpace(paper.PaperID)

	if title == "" || id == "" {
		return Item{}, false
	}

	doi := strings.TrimSpace(paper.ExternalIDs.DOI)
	link := strings.TrimSpace(paper.URL)

	if doi != "" {
		link = "https://doi.org/" + doi
	} else if arxiv := strings.TrimSpace(paper.ExternalIDs.ArXiv); arxiv != "" {
		link = "https://arxiv.org/abs/" + arxiv
	}
	if link == "" {
		link = "https://www.semanticscholar.org/paper/" + id
	}

	published := parseAcademicTime(paper.PublicationDate)
	if published.IsZero() && paper.Year > 0 {
		published = time.Date(
			paper.Year,
			1,
			1,
			0,
			0,
			0,
			0,
			time.UTC,
		)
	}

	tags := append([]string{}, paper.FieldsOfStudy...)
	for _, field := range paper.S2Fields {
		tags = append(tags, field.Category)
	}

	return Item{
		ID:        academicItemID(doi, paper.ExternalIDs.ArXiv, id, link),
		Source:    src.Name,
		Title:     CollapseWhitespace(title),
		Link:      link,
		Summary:   cleanSummary(paper.Abstract),
		Published: published,
		Tags:      mergeAcademicTags(src.Tags, tags),
	}, true
}
