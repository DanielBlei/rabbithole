package feeds

import (
	"context"
	"html"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/mmcdole/gofeed"
	"github.com/rs/zerolog/log"
)

const (
	fetchTimeout = 30 * time.Second
	maxParallel  = 8
	summaryLimit = 1200 // characters of summary kept for scoring
)

// Source identifies a feed to fetch.
type Source struct {
	Name string
	URL  string
}

// FetchAll fetches every source concurrently and returns the union of items.
// Individual feed failures are logged and skipped rather than failing the run.
func FetchAll(ctx context.Context, sources []Source) []Item {
	sem := make(chan struct{}, maxParallel)
	var (
		mu  sync.Mutex
		all []Item
		wg  sync.WaitGroup
	)
	parser := gofeed.NewParser()

	for _, src := range sources {
		wg.Add(1)
		go func(src Source) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			log.Debug().Str("feed", src.Name).Msg("fetching feed")
			start := time.Now()
			items, err := fetchOne(ctx, parser, src)
			if err != nil {
				log.Warn().Str("feed", src.Name).Err(err).Msg("skipping feed")
				return
			}
			log.Debug().Str("feed", src.Name).Int("items", len(items)).
				Str("elapsed", time.Since(start).Round(100*time.Millisecond).String()).Msg("feed fetched")
			mu.Lock()
			all = append(all, items...)
			mu.Unlock()
		}(src)
	}
	wg.Wait()
	return all
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
