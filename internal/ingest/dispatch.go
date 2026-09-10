// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/feeds"
)

// typeFetcher fetches every source of one feed type. Each config.FeedType gets
// its own file (rss.go, blog.go, news.go, academic.go) implementing this
// signature, so adding a new source kind's ingest support means adding a file
// and a registry entry here, not touching the dispatch logic itself.
type typeFetcher func(ctx context.Context, sources []feeds.Source) []feeds.Result

// fetchers maps every known feed type to its fetch implementation. RSS is the
// only one implemented today; the rest are placeholders (see skipUnimplemented)
// until their own ingest support lands. TestFetchersCoverEveryFeedType checks
// this stays in sync with config's FeedType constants.
var fetchers = map[config.FeedType]typeFetcher{
	config.FeedTypeRSS:      fetchRSS,
	config.FeedTypeBlog:     fetchBlog,
	config.FeedTypeNews:     fetchNews,
	config.FeedTypeAcademic: fetchAcademic,
}

// dispatchFetch fetches every active feed, grouping by resolved type and
// routing each group to its type's fetcher. The returned slice is positional
// with active — results[i] is active[i]'s outcome — which is what the rest of
// Run relies on to pair a feed's config with what its fetch produced.
func dispatchFetch(ctx context.Context, active []config.ResolvedFeed) []feeds.Result {
	results := make([]feeds.Result, len(active))

	byType := make(map[config.FeedType][]int, len(active))
	for i, f := range active {
		byType[f.Type] = append(byType[f.Type], i)
	}

	for t, idxs := range byType {
		sources := make([]feeds.Source, len(idxs))
		for j, idx := range idxs {
			f := active[idx]
			sources[j] = feeds.Source{Name: f.Name, URL: f.URL, Tags: f.Tags}
		}

		fetch, ok := fetchers[t]
		if !ok {
			// Only reachable if a feed's resolved type isn't in the registry —
			// config.Feed.Validate rejects unrecognized types before a feed ever
			// reaches the store, so this is a belt-and-suspenders fallback.
			fetch = func(ctx context.Context, sources []feeds.Source) []feeds.Result {
				return skipUnimplemented(ctx, sources, t)
			}
		}

		sub := fetch(ctx, sources)
		for j, idx := range idxs {
			results[idx] = sub[j]
		}
	}
	return results
}

// skipUnimplemented logs and skips every source of an unimplemented feed type.
// The Result carries an error rather than a quiet success: an unimplemented
// type never fetches anything, and a feed stuck at zero items should show up
// as failing on the Sources page (via the existing health/error surface)
// rather than reading as a healthy feed that just never publishes. rss.go's
// fetchRSS is the only fetcher that does real work; blog.go, news.go and
// academic.go each just call this with their own type.
func skipUnimplemented(ctx context.Context, sources []feeds.Source, kind config.FeedType) []feeds.Result {
	logger := zerolog.Ctx(ctx)
	now := time.Now()
	err := fmt.Errorf("feed type %q is not implemented yet", kind)
	out := make([]feeds.Result, len(sources))
	for i, src := range sources {
		logger.Warn().Str("feed", src.Name).Str("type", string(kind)).
			Msg("feed type not implemented yet; skipping")
		out[i] = feeds.Result{Source: src, Err: err, At: now}
	}
	return out
}
