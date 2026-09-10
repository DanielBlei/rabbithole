// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"context"
	"testing"

	"github.com/DanielBlei/rabbithole/internal/config"
)

// dispatchFetch must keep results positional with the feeds it was given, even
// when feeds of different types are interleaved and routed to different
// fetchers.
func TestDispatchFetchPreservesOrderAcrossTypes(t *testing.T) {
	active := []config.ResolvedFeed{
		{Name: "A", URL: serveRSS(t, feedRSS("Item A", "https://x.test/a")), Type: config.FeedTypeRSS},
		{Name: "B", URL: "https://blog.test/feed", Type: config.FeedTypeBlog},
		{Name: "C", URL: serveRSS(t, feedRSS("Item C", "https://x.test/c")), Type: config.FeedTypeRSS},
	}

	results := dispatchFetch(context.Background(), active)
	if len(results) != len(active) {
		t.Fatalf("results = %d, want %d", len(results), len(active))
	}
	for i, f := range active {
		if results[i].Source.Name != f.Name || results[i].Source.URL != f.URL {
			t.Errorf("results[%d] = %+v, want source %+v", i, results[i].Source, f)
		}
	}
	// The RSS feeds actually fetched; the blog feed was skipped with an error —
	// an unimplemented type must show up as failing, not as a silently healthy
	// feed stuck at zero items.
	if len(results[0].Items) == 0 {
		t.Errorf("RSS feed A returned no items")
	}
	if len(results[1].Items) != 0 || results[1].Err == nil {
		t.Errorf("blog feed B = %+v, want no items and a not-implemented error", results[1])
	}
	if len(results[2].Items) == 0 {
		t.Errorf("RSS feed C returned no items")
	}
}

// An unregistered type falls back to skipUnimplemented rather than panicking
// or dropping the feed from the results.
func TestDispatchFetchFallsBackForUnknownType(t *testing.T) {
	active := []config.ResolvedFeed{{Name: "Mystery", URL: "https://mystery.test/feed", Type: "podcast"}}
	results := dispatchFetch(context.Background(), active)
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	if results[0].Source.Name != "Mystery" || len(results[0].Items) != 0 || results[0].Err == nil {
		t.Errorf("results[0] = %+v, want an errored empty skip for Mystery", results[0])
	}
}

// Every FeedType config declares must have a registered fetcher. This is what
// makes the "unknown type" fallback in dispatchFetch actually unreachable in
// practice, rather than an untested assumption: if a new FeedType constant is
// added without wiring up its fetcher, this test catches it instead of the
// fallback silently swallowing the gap at runtime.
func TestFetchersCoverEveryFeedType(t *testing.T) {
	want := []config.FeedType{
		config.FeedTypeRSS, config.FeedTypeBlog, config.FeedTypeNews, config.FeedTypeAcademic,
	}
	if len(fetchers) != len(want) {
		t.Errorf("fetchers has %d entries, want %d (one per known FeedType)", len(fetchers), len(want))
	}
	for _, ft := range want {
		if _, ok := fetchers[ft]; !ok {
			t.Errorf("no fetcher registered for FeedType %q", ft)
		}
		if !ft.Valid() {
			t.Errorf("FeedType %q used in this test is not config.Valid() — test and enum drifted", ft)
		}
	}
}
