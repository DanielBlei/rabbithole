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
	"sync"
	"time"
)

var (
	crossrefRequestMu   sync.Mutex
	crossrefLastRequest time.Time
)

type crossrefResponse struct {
	Message struct {
		Items []crossrefWork `json:"items"`
	} `json:"message"`
}

type crossrefWork struct {
	DOI             string       `json:"DOI"`
	URL             string       `json:"URL"`
	Title           []string     `json:"title"`
	Abstract        string       `json:"abstract"`
	ContainerTitle  []string     `json:"container-title"`
	Publisher       string       `json:"publisher"`
	Published       crossrefDate `json:"published"`
	PublishedOnline crossrefDate `json:"published-online"`
	PublishedPrint  crossrefDate `json:"published-print"`
	Subject         []string     `json:"subject"`
}

type crossrefDate struct {
	DateParts [][]int `json:"date-parts"`
}

func (f *AcademicFetcher) fetchCrossref(
	ctx context.Context,
	src Source,
	query AcademicQuery,
) ([]Item, error) {
	base := f.CrossrefBaseURL
	if base == "" {
		base = "https://api.crossref.org/works"
	}

	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("crossref endpoint: %w", err)
	}

	q := u.Query()

	if value := strings.TrimSpace(query.Query); value != "" {
		q.Set("query.bibliographic", value)
	}
	if value := strings.TrimSpace(query.Publisher); value != "" {
		q.Set("query.publisher-name", value)
	}
	if value := strings.TrimSpace(query.Journal); value != "" {
		q.Set("query.container-title", value)
	}

	var filters []string
	if value := strings.TrimSpace(query.ISSN); value != "" {
		filters = append(filters, "issn:"+value)
	}
	if len(filters) > 0 {
		q.Set("filter", strings.Join(filters, ","))
	}

	q.Set("rows", strconv.Itoa(academicFetchLimit))
	q.Set("sort", "published")
	q.Set("order", "desc")
	q.Set(
		"select",
		"DOI,URL,title,abstract,container-title,publisher,"+
			"published,published-online,published-print,subject",
	)

	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "rabbithole/academic-reader")

	crossrefRequestMu.Lock()
	defer crossrefRequestMu.Unlock()

	var resp *http.Response

	for attempt := 0; attempt < 3; attempt++ {
		if !crossrefLastRequest.IsZero() {
			wait := 200*time.Millisecond - time.Since(crossrefLastRequest)
			if wait > 0 {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(wait):
				}
			}
		}

		resp, err = f.client().Do(req)
		crossrefLastRequest = time.Now()
		if err != nil {
			return nil, err
		}

		if resp.StatusCode != http.StatusTooManyRequests {
			break
		}

		_ = resp.Body.Close()

		delay := time.Duration(1<<attempt) * time.Second
		if seconds, parseErr := strconv.Atoi(
			resp.Header.Get("Retry-After"),
		); parseErr == nil && seconds >= 0 {
			delay = time.Duration(seconds) * time.Second
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}

	if resp == nil {
		return nil, fmt.Errorf("crossref request returned no response")
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("crossref returned %s", resp.Status)
	}

	var doc crossrefResponse
	if err := json.NewDecoder(
		io.LimitReader(resp.Body, 30<<20),
	).Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode crossref json: %w", err)
	}

	items := make([]Item, 0, len(doc.Message.Items))

	for _, work := range doc.Message.Items {
		if want := strings.TrimSpace(query.Publisher); want != "" &&
			!strings.EqualFold(want, strings.TrimSpace(work.Publisher)) {
			continue
		}

		title := firstAcademicString(work.Title)
		doi := strings.TrimSpace(work.DOI)
		link := strings.TrimSpace(work.URL)

		if doi != "" {
			link = "https://doi.org/" + doi
		}
		if link == "" || title == "" {
			continue
		}

		tags := mergeAcademicTags(src.Tags, work.Subject)

		if publisher := strings.TrimSpace(work.Publisher); publisher != "" {
			tags = mergeAcademicTags(tags, []string{publisher})
		}
		if venue := firstAcademicString(work.ContainerTitle); venue != "" {
			tags = mergeAcademicTags(tags, []string{venue})
		}

		items = append(items, Item{
			ID:        academicItemID(doi, "", link, link),
			Source:    src.Name,
			Title:     CollapseWhitespace(title),
			Link:      link,
			Summary:   cleanSummary(work.Abstract),
			Published: crossrefPublished(work),
			Tags:      tags,
		})
	}

	return items, nil
}

func firstAcademicString(values []string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func crossrefPublished(work crossrefWork) time.Time {
	for _, date := range []crossrefDate{
		work.Published,
		work.PublishedOnline,
		work.PublishedPrint,
	} {
		if parsed := date.Time(); !parsed.IsZero() {
			return parsed
		}
	}

	return time.Time{}
}

func (date crossrefDate) Time() time.Time {
	if len(date.DateParts) == 0 || len(date.DateParts[0]) == 0 {
		return time.Time{}
	}

	parts := date.DateParts[0]
	year, month, day := parts[0], 1, 1

	if len(parts) > 1 && parts[1] >= 1 && parts[1] <= 12 {
		month = parts[1]
	}
	if len(parts) > 2 && parts[2] >= 1 && parts[2] <= 31 {
		day = parts[2]
	}

	return time.Date(
		year,
		time.Month(month),
		day,
		0,
		0,
		0,
		0,
		time.UTC,
	)
}
