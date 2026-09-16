// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package feeds

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type arxivFeed struct {
	Entries []arxivEntry `xml:"entry"`
}

type arxivEntry struct {
	ID         string          `xml:"id"`
	Title      string          `xml:"title"`
	Summary    string          `xml:"summary"`
	Published  string          `xml:"published"`
	Updated    string          `xml:"updated"`
	Links      []arxivLink     `xml:"link"`
	Categories []arxivCategory `xml:"category"`
}

type arxivLink struct {
	Href  string `xml:"href,attr"`
	Rel   string `xml:"rel,attr"`
	Type  string `xml:"type,attr"`
	Title string `xml:"title,attr"`
}

type arxivCategory struct {
	Term string `xml:"term,attr"`
}

func (f *AcademicFetcher) fetchArXiv(
	ctx context.Context,
	src Source,
	query AcademicQuery,
) ([]Item, error) {
	base := f.ArXivBaseURL
	if base == "" {
		base = "https://export.arxiv.org/api/query"
	}

	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("arxiv endpoint: %w", err)
	}

	q := u.Query()
	search := strings.TrimSpace(query.Query)
	if search == "" {
		return nil, fmt.Errorf("arxiv query is required")
	}

	// Plain text searches all fields. Explicit arXiv query syntax such as
	// ti:, au:, cat:, AND and OR is passed through unchanged.
	if !strings.Contains(search, ":") {
		search = "all:" + strconv.Quote(search)
	}

	q.Set("search_query", search)
	q.Set("start", "0")
	q.Set("max_results", strconv.Itoa(academicFetchLimit))
	q.Set("sortBy", "submittedDate")
	q.Set("sortOrder", "descending")
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
		return nil, fmt.Errorf("arxiv returned %s", resp.Status)
	}

	var doc arxivFeed
	if err := xml.NewDecoder(
		io.LimitReader(resp.Body, 20<<20),
	).Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode arxiv atom: %w", err)
	}

	items := make([]Item, 0, len(doc.Entries))
	for _, entry := range doc.Entries {
		id := arxivID(entry.ID)
		link := arxivLandingLink(entry.Links)

		if link == "" {
			link = strings.TrimSpace(entry.ID)
		}
		if link == "" {
			continue
		}

		published := parseAcademicTime(entry.Published)
		if published.IsZero() {
			published = parseAcademicTime(entry.Updated)
		}

		categories := make([]string, 0, len(entry.Categories))
		for _, category := range entry.Categories {
			if term := strings.TrimSpace(category.Term); term != "" {
				categories = append(categories, term)
			}
		}

		items = append(items, Item{
			ID:        academicItemID("", id, "", link),
			Source:    src.Name,
			Title:     CollapseWhitespace(entry.Title),
			Link:      link,
			Summary:   cleanSummary(entry.Summary),
			Published: published,
			Tags:      mergeAcademicTags(src.Tags, categories),
		})
	}

	return items, nil
}

func arxivID(raw string) string {
	raw = strings.TrimSpace(raw)

	if i := strings.LastIndex(raw, "/abs/"); i >= 0 {
		raw = raw[i+5:]
	} else if i := strings.LastIndex(raw, "/"); i >= 0 {
		raw = raw[i+1:]
	}

	// Strip revision suffixes such as v1/v2 while preserving old-style IDs.
	if i := strings.LastIndex(raw, "v"); i > 0 {
		if _, err := strconv.Atoi(raw[i+1:]); err == nil {
			raw = raw[:i]
		}
	}

	return raw
}

func arxivLandingLink(links []arxivLink) string {
	for _, link := range links {
		href := strings.TrimSpace(link.Href)
		if href == "" {
			continue
		}

		if link.Rel == "alternate" || link.Rel == "" {
			return href
		}
	}

	return ""
}

func parseAcademicTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}

	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02",
	} {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed
		}
	}

	return time.Time{}
}
