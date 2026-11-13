// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// seedCluster records a second item and clusters it with the seeded item a.
func seedCluster(t *testing.T, w *Web) {
	t.Helper()
	now := time.Now()
	if err := w.db.Record(context.Background(), []feeds.Item{
		{ID: "b", Source: "S2", Title: "B is the same development", Link: "https://x/b", Published: now},
		{ID: "c", Source: "S3", Title: "C is unrelated", Link: "https://x/c", Published: now.Add(-time.Hour)},
	}, nil, now); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := w.db.ReplaceStoryClusters(context.Background(), []store.StoryCluster{{
		ID: "story-1", RepresentativeItemID: "a", Method: "exact", Confidence: 1,
		Items: []store.StoryClusterItem{{ItemID: "a", Position: 0}, {ItemID: "b", Position: 1}},
	}}); err != nil {
		t.Fatalf("ReplaceStoryClusters: %v", err)
	}
}

func TestFeedGroupsByDefaultAndShowsRepresentativeAndMembers(t *testing.T) {
	w := newTestWeb(t)
	seedCluster(t, w)

	for _, path := range []string{"/feed", "/feed?grouped=1"} {
		grouped := get(t, w, path)
		for _, want := range []string{
			`class="story-group"`,
			"1 related item on this story",
			`href="https://x/b"`,
			"B is the same development",
		} {
			if !strings.Contains(grouped, want) {
				t.Errorf("%s missing %q", path, want)
			}
		}
		if strings.Count(grouped, "data-source=") != 2 {
			t.Errorf("%s rendered %d rows, want 2 representatives", path, strings.Count(grouped, "data-source="))
		}
	}
}

func TestFeedGroupedZeroShowsItemsIndividually(t *testing.T) {
	w := newTestWeb(t)
	seedCluster(t, w)
	body := get(t, w, "/feed?grouped=0")
	if strings.Contains(body, "story-group") {
		t.Error("grouped=0 rendered grouped markup")
	}
	if strings.Count(body, "data-source=") != 3 {
		t.Errorf("grouped=0 rendered %d rows, want all 3", strings.Count(body, "data-source="))
	}
}

func TestFeedConfiguredOffShowsIndividuallyUnlessParamOverrides(t *testing.T) {
	w := newTestWeb(t)
	off := false
	w.src = config.Static(config.Config{GroupStories: &off})
	seedCluster(t, w)

	if body := get(t, w, "/feed"); strings.Contains(body, "story-group") ||
		strings.Count(body, "data-source=") != 3 {
		t.Errorf("setting off: absent param must list individually: %s", body)
	}
	if body := get(t, w, "/feed?grouped=1"); !strings.Contains(body, `class="story-group"`) {
		t.Error("grouped=1 must group even when the setting is off")
	}
}

func TestFeedHasNoGroupToggle(t *testing.T) {
	w := newTestWeb(t)
	seedCluster(t, w)
	for _, path := range []string{"/feed", "/feed?grouped=0"} {
		if body := get(t, w, path); strings.Contains(body, "feed-view") {
			t.Errorf("%s still renders a view toggle", path)
		}
	}
}

func TestFeedGroupedWithoutClustersStillRenders(t *testing.T) {
	w := newTestWeb(t)
	body := get(t, w, "/feed?grouped=1")
	if !strings.Contains(body, "story-group--single") {
		t.Errorf("grouped feed without clusters did not fall back to single rows: %s", body)
	}
}

func TestFeedGroupedFallsBackToExactDuplicates(t *testing.T) {
	w := newTestWeb(t)
	if err := w.db.Record(context.Background(), []feeds.Item{
		{ID: "b", Source: "S1", Title: "A", Link: "https://x/b"},
	}, nil, time.Now()); err != nil {
		t.Fatalf("Record: %v", err)
	}
	body := get(t, w, "/feed?grouped=1")
	if !strings.Contains(body, "1 related item on this story") {
		t.Errorf("grouped feed did not group exact duplicates: %s", body)
	}
}

// Members keep their own score and reason, so a group can be judged without
// expanding it into the individual view.
func TestFeedGroupedMemberShowsScoreAndReason(t *testing.T) {
	w := newTestWeb(t)
	now := time.Now()
	if err := w.db.Record(context.Background(), []feeds.Item{
		{ID: "b", Source: "S2", Title: "B is the same development", Link: "https://x/b", Published: now},
	}, nil, now); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// Score the seeded item a: the feed orders newest first, so a renders as
	// the member under representative b.
	if err := w.db.Record(context.Background(), []feeds.Item{
		{ID: "a", Source: "S1", Title: "A", Link: "https://x/a"},
	}, []store.DigestEntry{{
		Item:  feeds.Item{ID: "a", Source: "S1", Title: "A", Link: "https://x/a"},
		Score: 7, Reason: "covers the same launch from a second outlet",
	}}, now); err != nil {
		t.Fatalf("Record scored: %v", err)
	}
	if err := w.db.ReplaceStoryClusters(context.Background(), []store.StoryCluster{{
		ID: "story-1", RepresentativeItemID: "b", Method: "exact", Confidence: 1,
		Items: []store.StoryClusterItem{{ItemID: "a", Position: 0}, {ItemID: "b", Position: 1}},
	}}); err != nil {
		t.Fatalf("ReplaceStoryClusters: %v", err)
	}
	grouped := get(t, w, "/feed?grouped=1")
	for _, want := range []string{"score 7/10", "covers the same launch from a second outlet"} {
		if !strings.Contains(grouped, want) {
			t.Errorf("grouped member line missing %q: %s", want, grouped)
		}
	}
}

// Under a filter, only the visible rows form the group: a filtered-out member
// neither renders nor drags its representative out of the results.
func TestFeedGroupedUnderFilterKeepsOnlyVisibleMembers(t *testing.T) {
	w := newTestWeb(t)
	now := time.Now()
	if err := w.db.Record(context.Background(), []feeds.Item{
		{ID: "b", Source: "S2", Title: "B is the same development", Link: "https://x/b", Published: now},
	}, nil, now); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := w.db.ReplaceStoryClusters(context.Background(), []store.StoryCluster{{
		ID: "story-1", RepresentativeItemID: "a", Method: "exact", Confidence: 1,
		Items: []store.StoryClusterItem{{ItemID: "a", Position: 0}, {ItemID: "b", Position: 1}},
	}}); err != nil {
		t.Fatalf("ReplaceStoryClusters: %v", err)
	}
	narrowed := get(t, w, "/feed?grouped=1&source=S2")
	if !strings.Contains(narrowed, "story-group--single") {
		t.Errorf("filtered grouped feed did not show b as its own group: %s", narrowed)
	}
	if strings.Contains(narrowed, "related item") {
		t.Errorf("filtered grouped feed kept the group summary: %s", narrowed)
	}
	if strings.Contains(narrowed, `data-source="S1"`) {
		t.Errorf("filtered grouped feed leaked the filtered-out member: %s", narrowed)
	}
}

func TestFeedPreservesGroupedFilter(t *testing.T) {
	w := newTestWeb(t)
	seedCluster(t, w)
	for _, route := range []string{"/", "/feed"} {
		for _, grouped := range []string{"", "0", "1"} {
			t.Run(route+"grouped="+grouped, func(t *testing.T) {
				query := url.Values{}
				if grouped != "" {
					query.Set("grouped", grouped)
				}
				body := get(t, w, route+"?"+query.Encode())
				doc, err := html.Parse(strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				found := false
				var visit func(*html.Node)
				visit = func(n *html.Node) {
					if n.Type == html.ElementNode && n.Data == "input" {
						attrs := map[string]string{}
						for _, a := range n.Attr {
							attrs[a.Key] = a.Val
						}
						if attrs["name"] == "grouped" {
							found = true
							if attrs["type"] != "hidden" || attrs["value"] != grouped {
								t.Errorf("grouped input = %v", attrs)
							}
							query.Set("grouped", attrs["value"])
						}
					}
					for c := n.FirstChild; c != nil; c = c.NextSibling {
						visit(c)
					}
				}
				visit(doc)
				if found != (grouped != "") {
					t.Errorf("grouped input present = %v", found)
				}
				query.Set("sort", "oldest")
				filtered := get(t, w, "/feed?"+query.Encode())
				if grouped == "0" && strings.Contains(filtered, `class="story-group`) {
					t.Error("filter submission restored grouping")
				}
			})
		}
	}
}
