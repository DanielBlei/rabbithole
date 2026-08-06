// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package feeds

import (
	"context"
	"html"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/mmcdole/gofeed"
	"github.com/rs/zerolog"
)

const (
	fetchTimeout = 30 * time.Second
	maxParallel  = 8
	// summaryLimit caps the summary text sent to the scoring model.
	// Most RSS feeds only provide a short excerpt (300-600 chars) in
	// <description>, not the full article; the limit guards against
	// the rare feed that inlines entire posts via <content:encoded>.
	summaryLimit = 1500
)

// Source identifies a feed to fetch. Tags are the feed's configured labels,
type Source struct {
	Name string
	URL  string
	Tags []string
}

// Result is one source's fetch outcome. A failed fetch is a Result with Err
// set and no Items — failures are reported, not swallowed, so the caller can
// record feed health and still carry on with the sources that did work.
type Result struct {
	Source  Source
	Items   []Item
	Err     error
	Elapsed time.Duration
	At      time.Time // when the fetch finished
}

// FetchAll fetches every source concurrently and returns one Result per
// source, in the order the sources were given. Individual feed failures are
// logged and carried on the Result rather than failing the run.
func FetchAll(ctx context.Context, sources []Source) []Result {
	logger := zerolog.Ctx(ctx)
	sem := make(chan struct{}, maxParallel)
	// Indexed writes rather than an append under a mutex, so results come back
	// in source order and a run's logs/health rows read predictably.
	results := make([]Result, len(sources))
	var wg sync.WaitGroup
	for i, src := range sources {
		wg.Add(1)
		go func(i int, src Source) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			logger.Debug().Str("feed", src.Name).Msg("fetching feed")
			start := time.Now()
			// gofeed.Parser lazily initializes internal state on first use and
			// is not goroutine-safe, so each fetch gets its own (cheap) parser.
			items, err := fetchOne(ctx, gofeed.NewParser(), src)
			res := Result{Source: src, Items: items, Err: err, Elapsed: time.Since(start), At: time.Now()}
			if err != nil {
				logger.Warn().Str("feed", src.Name).Err(err).Msg("skipping feed")
			} else {
				logger.Debug().Str("feed", src.Name).Int("items", len(items)).
					Str("elapsed", res.Elapsed.Round(100*time.Millisecond).String()).Msg("feed fetched")
			}
			results[i] = res
		}(i, src)
	}
	wg.Wait()
	return results
}

func fetchOne(ctx context.Context, parser *gofeed.Parser, src Source) ([]Item, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	feed, err := parser.ParseURLWithContext(src.URL, ctx)
	if err != nil {
		return nil, err
	}
	items := make([]Item, 0, len(feed.Items))
	for _, it := range feed.Items {
		var published time.Time
		if it.PublishedParsed != nil {
			published = *it.PublishedParsed
		} else if it.UpdatedParsed != nil {
			published = *it.UpdatedParsed
		}
		summary := it.Description
		if summary == "" {
			summary = it.Content
		}
		items = append(items, Item{
			ID:        makeID(it.GUID, it.Link),
			Source:    src.Name,
			Title:     strings.TrimSpace(it.Title),
			Link:      it.Link,
			Summary:   cleanSummary(summary),
			Published: published,
			Tags:      src.Tags,
		})
	}
	return items, nil
}

var tagRE = regexp.MustCompile(`<[^>]*>`)

// cleanSummary strips HTML tags, unescapes entities, collapses whitespace,
// and truncates to summaryLimit so scoring prompts stay small.
func cleanSummary(s string) string {
	s = tagRE.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > summaryLimit {
		s = s[:summaryLimit] + "…"
	}
	return s
}
