// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/feeds"
)

func seedScoredItem(t *testing.T, db *Store, id string, published time.Time) feeds.Item {
	t.Helper()
	item := feeds.Item{
		ID: id, Source: "S", Title: "Title " + id, Link: "https://x/" + id,
		Summary: "summary", Published: published, Tags: []string{"feed-tag"},
	}
	if err := db.Record(context.Background(), []feeds.Item{item}, []DigestEntry{{
		Item: item, Score: 8, Reason: "relevant",
	}}, published); err != nil {
		t.Fatalf("Record(%s): %v", id, err)
	}
	return item
}

func TestReplaceItemTagsRoundTripAndAliases(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	item := seedScoredItem(t, db, "a", now.Add(-time.Hour))

	got, err := db.ReplaceItemTags(ctx, item.ID, 1,
		[]string{"vLLM", "paged attention", "vllm"},
		[]string{"inference", "vllm"})
	if err != nil {
		t.Fatalf("ReplaceItemTags: %v", err)
	}
	if got.Keywords != 2 || got.Assumptions != 1 {
		t.Fatalf("replaced = %+v, want 2 keywords and 1 assumption", got)
	}

	tags, err := db.TagsForItem(ctx, item.ID)
	if err != nil {
		t.Fatalf("TagsForItem: %v", err)
	}
	if len(tags) != 3 {
		t.Fatalf("tags = %+v, want 3", tags)
	}
	bySlug := map[string]ItemTag{}
	for _, tag := range tags {
		bySlug[tag.Slug] = tag
	}
	if bySlug["vllm"].Label != "vLLM" {
		t.Errorf("label = %q, want the first-seen display form", bySlug["vllm"].Label)
	}
	if bySlug["vllm"].Kind != TagKindKeyword {
		t.Errorf("vllm kind = %q, want keyword (keyword wins over assumption)", bySlug["vllm"].Kind)
	}
	if bySlug["paged-attention"].Kind != TagKindKeyword {
		t.Errorf("paged-attention kind = %q", bySlug["paged-attention"].Kind)
	}
	if bySlug["inference"].Kind != TagKindAssumption {
		t.Errorf("inference kind = %q", bySlug["inference"].Kind)
	}

	if err := db.AddTagAlias(ctx, "k8s", "Kubernetes"); err != nil {
		t.Fatalf("AddTagAlias: %v", err)
	}
	if _, err := db.ReplaceItemTags(ctx, item.ID, 1, []string{"k8s"}, nil); err != nil {
		t.Fatalf("ReplaceItemTags(alias): %v", err)
	}
	tags, err = db.TagsForItem(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 1 || tags[0].Slug != "kubernetes" || tags[0].Label != "Kubernetes" {
		t.Fatalf("alias did not resolve to the canonical tag: %+v", tags)
	}

	items, err := db.ItemsForTag(ctx, "kubernetes", []string{TagKindKeyword}, Page{})
	if err != nil {
		t.Fatalf("ItemsForTag: %v", err)
	}
	if len(items) != 1 || items[0].ID != item.ID {
		t.Fatalf("ItemsForTag = %+v, want item a", items)
	}
	if len(items[0].Tags) != 1 || items[0].Tags[0] != "feed-tag" {
		t.Errorf("feed tags changed: %+v", items[0].Tags)
	}

	top, err := db.TopTags(ctx, now.Add(-24*time.Hour), nil, 10)
	if err != nil {
		t.Fatalf("TopTags: %v", err)
	}
	if len(top) != 1 || top[0].Slug != "kubernetes" || top[0].Count != 1 {
		t.Fatalf("TopTags = %+v", top)
	}

	listed, err := db.ListTags(ctx)
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	var found bool
	for _, tag := range listed {
		if tag.Slug == "kubernetes" {
			found = true
			if len(tag.Aliases) != 1 || tag.Aliases[0] != "k8s" {
				t.Errorf("aliases = %v", tag.Aliases)
			}
		}
	}
	if !found {
		t.Error("kubernetes missing from ListTags")
	}

	state, ok, err := db.ClassificationState(ctx, item.ID)
	if err != nil || !ok {
		t.Fatalf("ClassificationState = %+v, %v, %v", state, ok, err)
	}
	if state.Status != ClassificationOK || state.ClassifierVersion != 1 || state.Attempts != 0 {
		t.Errorf("state = %+v", state)
	}
}

func TestReplaceItemTagsIsIdempotent(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	item := seedScoredItem(t, db, "a", time.Now().UTC())

	for range 2 {
		if _, err := db.ReplaceItemTags(ctx, item.ID, 1,
			[]string{"Go", "go"}, []string{"runtime"}); err != nil {
			t.Fatalf("ReplaceItemTags: %v", err)
		}
	}
	tags, err := db.TagsForItem(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 2 {
		t.Fatalf("replayed tags = %+v, want 2", tags)
	}
	var tagRows int
	if err := db.queryRow(ctx, "SELECT COUNT(*) FROM tags").Scan(&tagRows); err != nil {
		t.Fatal(err)
	}
	if tagRows != 2 {
		t.Errorf("tags rows = %d, want 2", tagRows)
	}
}

func TestReplaceItemTagsIgnoresMissingItem(t *testing.T) {
	db := openTestStore(t)
	got, err := db.ReplaceItemTags(context.Background(), "missing", 1, []string{"go"}, nil)
	if err != nil {
		t.Fatalf("ReplaceItemTags: %v", err)
	}
	if got != (TagWriteResult{}) {
		t.Errorf("result = %+v, want zero", got)
	}
}

func TestReplaceItemTagsCascadesFromItemDelete(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	item := seedScoredItem(t, db, "a", time.Now().UTC())
	if _, err := db.ReplaceItemTags(ctx, item.ID, 1, []string{"go"}, []string{"runtime"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.exec(ctx, "DELETE FROM items WHERE id = ?", item.ID); err != nil {
		t.Fatal(err)
	}
	tags, err := db.TagsForItem(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 0 {
		t.Errorf("cascade left item tags: %+v", tags)
	}
	_, ok, err := db.ClassificationState(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("cascade left the classification row")
	}
}

func TestAddTagAliasRejectsSelfAndInvalidLabels(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	if err := db.AddTagAlias(ctx, "K8s", "k8s"); !errors.Is(err, ErrInvalidTag) {
		t.Errorf("self alias error = %v, want ErrInvalidTag", err)
	}
	if err := db.AddTagAlias(ctx, "++", "kubernetes"); !errors.Is(err, ErrInvalidTag) {
		t.Errorf("invalid alias error = %v, want ErrInvalidTag", err)
	}
}

func TestClassificationQueueLifecycle(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	item := seedScoredItem(t, db, "a", now.Add(-time.Hour))

	enqueued, err := db.EnqueueClassification(ctx, []string{item.ID}, 1, false)
	if err != nil || enqueued != 1 {
		t.Fatalf("EnqueueClassification = %d, %v", enqueued, err)
	}
	claims, err := db.ClaimClassification(ctx, 1, now, 10, 5)
	if err != nil {
		t.Fatalf("ClaimClassification: %v", err)
	}
	if len(claims) != 1 || claims[0].Item.ID != item.ID || claims[0].Attempts != 0 {
		t.Fatalf("claims = %+v", claims)
	}

	if err := db.MarkClassificationFailed(ctx, item.ID, 1, 1, "model down", now.Add(time.Hour)); err != nil {
		t.Fatalf("MarkClassificationFailed: %v", err)
	}
	claims, err = db.ClaimClassification(ctx, 1, now, 10, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 0 {
		t.Errorf("claimed a backed-off item: %+v", claims)
	}
	claims, err = db.ClaimClassification(ctx, 1, now.Add(2*time.Hour), 10, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 || claims[0].Attempts != 1 {
		t.Errorf("after backoff claims = %+v, want the item at attempts 1", claims)
	}

	if err := db.MarkClassificationFailed(ctx, item.ID, 1, 5, "still down", now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	claims, err = db.ClaimClassification(ctx, 1, now.Add(24*time.Hour), 10, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 0 {
		t.Errorf("exhausted item reclaimed: %+v", claims)
	}

	enqueued, err = db.EnqueueBackfill(ctx, time.Time{}, 0, 1)
	if err != nil || enqueued != 1 {
		t.Fatalf("manual backfill = %d, %v, want the exhausted item reset", enqueued, err)
	}
	claims, err = db.ClaimClassification(ctx, 1, now, 10, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 || claims[0].Attempts != 0 {
		t.Errorf("after manual retry claims = %+v", claims)
	}
}

func TestEnqueueLeavesCurrentVersionOKAlone(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	item := seedScoredItem(t, db, "a", time.Now().UTC())
	if _, err := db.ReplaceItemTags(ctx, item.ID, 1, []string{"go"}, nil); err != nil {
		t.Fatal(err)
	}

	enqueued, err := db.EnqueueClassification(ctx, []string{item.ID}, 1, false)
	if err != nil {
		t.Fatalf("EnqueueClassification: %v", err)
	}
	if enqueued != 0 {
		t.Errorf("enqueued %d rows for an item already ok at the current version", enqueued)
	}
	state, ok, err := db.ClassificationState(ctx, item.ID)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if state.Status != ClassificationOK {
		t.Errorf("status = %q, want ok", state.Status)
	}

	enqueued, err = db.EnqueueClassification(ctx, []string{item.ID}, 2, false)
	if err != nil || enqueued != 1 {
		t.Fatalf("version bump enqueue = %d, %v", enqueued, err)
	}
	state, _, err = db.ClassificationState(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != ClassificationPending || state.ClassifierVersion != 2 {
		t.Errorf("after version bump state = %+v", state)
	}
}

func TestEnqueueBackfillSkipsClassifiedItems(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	fresh := seedScoredItem(t, db, "fresh", now.Add(-time.Hour))
	old := seedScoredItem(t, db, "old", now.Add(-40*24*time.Hour))
	if _, err := db.ReplaceItemTags(ctx, fresh.ID, 1, []string{"go"}, nil); err != nil {
		t.Fatal(err)
	}

	enqueued, err := db.EnqueueBackfill(ctx, now.Add(-7*24*time.Hour), 0, 1)
	if err != nil {
		t.Fatalf("EnqueueBackfill: %v", err)
	}
	if enqueued != 0 {
		t.Errorf("enqueued %d, want 0 (fresh already ok, old outside window)", enqueued)
	}

	enqueued, err = db.EnqueueBackfill(ctx, time.Time{}, 0, 1)
	if err != nil {
		t.Fatalf("EnqueueBackfill(all): %v", err)
	}
	if enqueued != 1 {
		t.Errorf("enqueued %d, want the old item only", enqueued)
	}
	claims, err := db.ClaimClassification(ctx, 1, now, 10, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 || claims[0].Item.ID != old.ID {
		t.Errorf("claims = %+v, want old", claims)
	}
}
