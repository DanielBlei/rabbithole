// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

type countingEmbedder struct {
	calls int
	vecs  [][]float64
	err   error
}

func (e *countingEmbedder) Embed(context.Context, []string) ([][]float64, error) {
	e.calls++
	if e.err != nil {
		return nil, e.err
	}
	return e.vecs, nil
}

type countingVerifier struct {
	calls int
	reply bool
	err   error
}

func (v *countingVerifier) SameDevelopment(context.Context, Item, Item) (bool, error) {
	v.calls++
	if v.err != nil {
		return false, v.err
	}
	return v.reply, nil
}

func testItem(id, source, title, link string) Item {
	return Item{
		ID: id, Source: source, Title: title, Link: link,
		Published: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		Score:     5,
	}
}

// cancellingVerifier simulates a model endpoint slow enough that the run's
// deadline expires: the first verification call both fails and cancels the
// context the build runs under.
type cancellingVerifier struct {
	cancel context.CancelFunc
	calls  int
}

func (v *cancellingVerifier) SameDevelopment(context.Context, Item, Item) (bool, error) {
	v.calls++
	v.cancel()
	return false, errors.New("verification unavailable")
}

// Items that normalize to empty values — no link, or a title with no
// alphanumerics — must not share a grouping key with every other such item.
func TestEmptyNormalizedValuesDoNotGroupUnrelatedItems(t *testing.T) {
	items := []Item{
		testItem("a", "S1", "First story about a release", ""),
		testItem("b", "S2", "Second story about a launch", ""),
		testItem("p", "S1", "!!!", "https://example.com/p"),
		testItem("q", "S1", "???", "https://example.com/q"),
	}
	clusters, _, err := Build(context.Background(), items, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 0 {
		t.Errorf("clusters = %+v, want none: empty links or punctuation-only titles grouped", clusters)
	}
}

// A deadline that expires mid-run must not discard the whole build: the exact
// groups (which need no model) survive as standalone clusters and the partial
// result is returned without an error.
func TestDeadlineKeepsExactClusters(t *testing.T) {
	items := []Item{
		testItem("a", "S1", "Big story unfolds today in city", "https://example.com/a"),
		testItem("b", "S2", "Big story unfolds today in town", "https://example.com/b"),
		testItem("c", "S1", "Unrelated exact piece", "https://example.com/same"),
		testItem("d", "S2", "Unrelated exact piece", "https://example.com/same"),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	verifier := &cancellingVerifier{cancel: cancel}
	clusters, stats, err := Build(ctx, items, Options{Verifier: verifier})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if verifier.calls != 1 {
		t.Fatalf("verifier calls = %d, want 1", verifier.calls)
	}
	if len(clusters) != 1 || clusters[0].Method != "exact" || len(clusters[0].Items) != 2 {
		t.Fatalf("clusters = %+v, want the exact pair {c,d} only", clusters)
	}
	if clusters[0].Items[0].ItemID != "c" || clusters[0].Items[1].ItemID != "d" {
		t.Errorf("cluster members = %+v, want c and d", clusters[0].Items)
	}
	if !stats.VerificationFallback {
		t.Error("stats do not report the model-comparison cutoff")
	}
}

// The representative is deterministic: the highest-scored member, newest first
// on ties, then item ID.
func TestRepresentativeIsHighestScoredMember(t *testing.T) {
	low := testItem("low", "S1", "Same development reported twice here", "https://example.com/1")
	low.Score = 2
	high := testItem("high", "S2", "Same development reported twice here", "https://example.com/2")
	high.Score = 9
	clusters, _, err := Build(context.Background(), []Item{low, high}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 1 {
		t.Fatalf("clusters = %d, want 1", len(clusters))
	}
	if clusters[0].RepresentativeItemID != "high" {
		t.Errorf("representative = %q, want high", clusters[0].RepresentativeItemID)
	}
	if clusters[0].Items[0].ItemID != "high" {
		t.Errorf("first member = %q, want high", clusters[0].Items[0].ItemID)
	}
}

func TestExactDuplicatesGroupWithoutModelCalls(t *testing.T) {
	items := []Item{
		testItem("a", "S1", "A major lab ships a new reasoning model",
			"https://www.example.com/story/?utm_source=x#top"),
		testItem("b", "S2", "A major lab ships a new reasoning model", "https://example.com/story"),
		testItem("c", "S1", "A different development entirely", "https://example.com/other"),
	}
	clusters, stats, err := Build(context.Background(), items, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 1 {
		t.Fatalf("clusters = %d, want 1: %+v", len(clusters), clusters)
	}
	if stats.Embeddings != 0 || stats.Verifications != 0 {
		t.Errorf("exact grouping made model calls: %+v", stats)
	}
	if clusters[0].Method != "exact" {
		t.Errorf("method = %q, want exact", clusters[0].Method)
	}
	if len(clusters[0].Items) != 2 {
		t.Errorf("members = %d, want 2", len(clusters[0].Items))
	}
}

func TestBlockingBoundsComparisons(t *testing.T) {
	items := make([]Item, 0, 2000)
	for i := 0; i < 2000; i++ {
		items = append(items, testItem(
			fmt.Sprintf("id-%04d", i), "S", fmt.Sprintf("unique headline %d topic%d", i, i%17),
			fmt.Sprintf("https://example.com/%d", i),
		))
	}
	const maxComparisons = 4
	_, stats, err := Build(context.Background(), items, Options{MaxComparisonsPerItem: maxComparisons})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Comparisons > len(items)*maxComparisons {
		t.Fatalf("comparisons = %d, want at most %d", stats.Comparisons,
			len(items)*maxComparisons)
	}
}

func TestConservativeFallbackKeepsDistinctStoriesApart(t *testing.T) {
	items := []Item{
		testItem("a", "S1", "OpenAI launches a new reasoning model for developers", "https://example.com/a"),
		testItem("b", "S2", "OpenAI sued by authors over training data", "https://example.com/b"),
		testItem("c", "S3", "OpenAI launches a new reasoning model for developers", "https://example.com/c"),
	}
	clusters, stats, err := Build(context.Background(), items, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Embeddings != 0 || stats.Verifications != 0 {
		t.Errorf("conservative fallback made model calls: %+v", stats)
	}
	if len(clusters) != 1 {
		t.Fatalf("clusters = %d, want only the identical headline group", len(clusters))
	}
	members := map[string]bool{}
	for _, member := range clusters[0].Items {
		members[member.ItemID] = true
	}
	if !members["a"] || !members["c"] || members["b"] {
		t.Errorf("cluster members = %v, want a and c only", members)
	}
}

func TestVerificationPreventsDistinctStoriesAndAcceptsSameStory(t *testing.T) {
	items := []Item{
		testItem("a", "S1", "Tesla recalls ten thousand vehicles over brake issue", "https://example.com/a"),
		testItem("b", "S2", "Tesla recall expands to twenty thousand cars for brakes", "https://example.com/b"),
	}
	embedder := &countingEmbedder{vecs: [][]float64{{1, 0}, {0.99, 0.01}}}
	reject := &countingVerifier{reply: false}
	clusters, stats, err := Build(context.Background(), items, Options{Embedder: embedder, Verifier: reject})
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 0 {
		t.Fatalf("rejected candidate was grouped: %+v", clusters)
	}
	if reject.calls == 0 || stats.Verifications == 0 {
		t.Fatalf("verification was not attempted: %+v", stats)
	}

	accept := &countingVerifier{reply: true}
	clusters, _, err = Build(context.Background(), items, Options{Embedder: embedder, Verifier: accept})
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 1 || clusters[0].Method != "verified" {
		t.Fatalf("accepted candidate = %+v, want one verified cluster", clusters)
	}
}

func TestEmbeddingFailureFallsBackWithoutError(t *testing.T) {
	items := []Item{
		testItem("a", "S1", "One exact headline for the clustering fallback test", "https://example.com/a"),
		testItem("b", "S2", "One exact headline for the clustering fallback test", "https://example.com/b"),
		testItem("c", "S3", "Another story entirely", "https://example.com/c"),
	}
	embedder := &countingEmbedder{err: errors.New("no embedding endpoint")}
	clusters, stats, err := Build(context.Background(), items, Options{Embedder: embedder})
	if err != nil {
		t.Fatalf("Build returned an error on embedding failure: %v", err)
	}
	if !stats.EmbeddingFallback {
		t.Error("embedding fallback was not reported")
	}
	if len(clusters) != 1 {
		t.Fatalf("clusters = %d, want the exact duplicate group", len(clusters))
	}
}

func TestBuildIsDeterministic(t *testing.T) {
	items := []Item{
		testItem("a", "S1", "Shared developing story about a launch release", "https://example.com/a"),
		testItem("b", "S2", "Shared developing story about a launch release", "https://example.com/b"),
		testItem("c", "S3", "Unrelated headline about the weather", "https://example.com/c"),
	}
	first, _, err := Build(context.Background(), items, Options{})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := Build(context.Background(), items, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != len(second) {
		t.Fatalf("cluster counts differ: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].ID != second[i].ID || first[i].RepresentativeItemID != second[i].RepresentativeItemID {
			t.Errorf("cluster %d differs: %+v vs %+v", i, first[i], second[i])
		}
	}
}
