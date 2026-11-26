// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"bytes"
	"context"
	_ "embed"
	"time"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/rank"
)

// demoFeedXML is the built-in feed behind a demo run: one item, no network.
//
//go:embed demo_feed.xml
var demoFeedXML []byte

// demoFeed stands in for a configured feed during a demo run. It is never
// stored. Since is wide because the item is stamped with the run time.
var demoFeed = config.ResolvedFeed{
	ID:       "demo",
	Name:     "Demo · GitHub",
	Type:     config.FeedTypeRSS,
	Enabled:  true,
	Since:    24 * time.Hour,
	MaxItems: 1,
	Tags:     []string{"demo", "rabbithole"},
}

// demoReason is the recorded rationale of the demo item's fixed score.
const demoReason = "Demo item from the guided tour: scored 10 by design (no model, no keyword match)."

// fetchDemo parses the embedded document through the same item construction as
// a real fetch and stamps the item as published now.
func fetchDemo() feeds.Result {
	start := time.Now()
	src := feeds.Source{Name: demoFeed.Name, Tags: demoFeed.Tags}
	res := feeds.Result{Source: src, At: start}
	items, site, err := feeds.ParseFeed(src, bytes.NewReader(demoFeedXML))
	if err != nil {
		res.Err = err
		return res
	}
	for i := range items {
		items[i].Published = start
	}
	res.Items, res.Site = items, site
	res.Elapsed = time.Since(start)
	return res
}

// demoScorer gives every item the top score without a model or keywords. It
// serves the demo run only.
type demoScorer struct{}

func (demoScorer) Validate(context.Context) error { return nil }

func (demoScorer) Score(_ context.Context, _ string, items []feeds.Item) ([]rank.ItemScore, error) {
	out := make([]rank.ItemScore, len(items))
	for i, it := range items {
		out[i] = rank.ItemScore{ID: it.ID, Score: 10, Reason: demoReason}
	}
	return out, nil
}
