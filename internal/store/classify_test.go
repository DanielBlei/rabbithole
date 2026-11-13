// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"strings"
	"sync"
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

	got, err := db.ReplaceItemTags(ctx, item.ID, 1, "",
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
	if _, err := db.ReplaceItemTags(ctx, item.ID, 1, "", []string{"k8s"}, nil); err != nil {
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
		if _, err := db.ReplaceItemTags(ctx, item.ID, 1, "",
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
	got, err := db.ReplaceItemTags(context.Background(), "missing", 1, "", []string{"go"}, nil)
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
	if _, err := db.ReplaceItemTags(ctx, item.ID, 1, "", []string{"go"}, []string{"runtime"}); err != nil {
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
	lease := 30 * time.Minute

	enqueued, err := db.EnqueueClassification(ctx, []string{item.ID}, 1, false)
	if err != nil || enqueued != 1 {
		t.Fatalf("EnqueueClassification = %d, %v", enqueued, err)
	}
	claims, err := db.ClaimClassification(ctx, 1, now, 10, 5, lease)
	if err != nil {
		t.Fatalf("ClaimClassification: %v", err)
	}
	if len(claims) != 1 || claims[0].Item.ID != item.ID || claims[0].Attempts != 0 {
		t.Fatalf("claims = %+v", claims)
	}
	if claims[0].ClaimToken == "" {
		t.Fatal("claim carried no ownership token")
	}

	if err := db.MarkClassificationFailed(
		ctx, item.ID, 1, 1, "model down", now.Add(time.Hour), claims[0].ClaimToken); err != nil {
		t.Fatalf("MarkClassificationFailed: %v", err)
	}
	claims, err = db.ClaimClassification(ctx, 1, now, 10, 5, lease)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 0 {
		t.Errorf("claimed a backed-off item: %+v", claims)
	}
	claims, err = db.ClaimClassification(ctx, 1, now.Add(2*time.Hour), 10, 5, lease)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 || claims[0].Attempts != 1 {
		t.Errorf("after backoff claims = %+v, want the item at attempts 1", claims)
	}

	if err := db.MarkClassificationFailed(
		ctx, item.ID, 1, 5, "still down", now.Add(2*time.Hour), claims[0].ClaimToken); err != nil {
		t.Fatal(err)
	}
	claims, err = db.ClaimClassification(ctx, 1, now.Add(24*time.Hour), 10, 5, lease)
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
	claims, err = db.ClaimClassification(ctx, 1, now, 10, 5, lease)
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
	if _, err := db.ReplaceItemTags(ctx, item.ID, 1, "", []string{"go"}, nil); err != nil {
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
	if _, err := db.ReplaceItemTags(ctx, fresh.ID, 1, "", []string{"go"}, nil); err != nil {
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
	claims, err := db.ClaimClassification(ctx, 1, now, 10, 5, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 || claims[0].Item.ID != old.ID {
		t.Errorf("claims = %+v, want old", claims)
	}
}

// --- Claim protocol (F5) ---

// claimOne claims a single item and returns its candidate, failing the test if
// the claim did not yield exactly one item.
func claimOne(
	t *testing.T, db *Store, version int, now time.Time, lease time.Duration,
) ClassificationCandidate {
	t.Helper()
	claims, err := db.ClaimClassification(context.Background(), version, now, 10, 5, lease)
	if err != nil {
		t.Fatalf("ClaimClassification: %v", err)
	}
	if len(claims) != 1 {
		t.Fatalf("claims = %+v, want exactly one item", claims)
	}
	return claims[0]
}

// Two workers must never hold the same item: while the first claim's lease is
// live a second claim returns nothing, and once the lease expires the item is
// reclaimable under a fresh token. That expiry path is also the whole recovery
// story for a worker that crashed mid-pass — no operator action needed.
func TestClaimExcludesActiveClaimsAndExpires(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	lease := 30 * time.Minute
	item := seedScoredItem(t, db, "a", now.Add(-time.Hour))

	first := claimOne(t, db, 1, now, lease)
	if first.ClaimToken == "" {
		t.Fatal("claim did not mint an ownership token")
	}

	second, err := db.ClaimClassification(ctx, 1, now, 10, 5, lease)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 0 {
		t.Fatalf("a live claim was stolen: %+v", second)
	}

	// Still contested just before expiry...
	late, err := db.ClaimClassification(ctx, 1, now.Add(lease-time.Second), 10, 5, lease)
	if err != nil {
		t.Fatal(err)
	}
	if len(late) != 0 {
		t.Fatalf("claim was stolen before its lease expired: %+v", late)
	}

	// ...and reclaimable after it.
	next := claimOne(t, db, 1, now.Add(lease+time.Second), lease)
	if next.ClaimToken == first.ClaimToken {
		t.Fatal("re-claim reused the previous worker's token")
	}

	state, ok, err := db.ClassificationState(ctx, item.ID)
	if err != nil || !ok {
		t.Fatalf("ClassificationState = %+v, %v, %v", state, ok, err)
	}
	if state.ClaimToken != next.ClaimToken || state.ClaimExpiresAt == nil {
		t.Fatalf("state does not reflect the live claim: %+v", state)
	}
	if state.NextAttemptAt != nil {
		t.Errorf("claiming left a backoff next_attempt_at = %v, want lease-governed recovery", state.NextAttemptAt)
	}
}

// A claim must be positive: a zero lease would let every pass steal every
// in-flight item, which is the bug the lease exists to prevent.
func TestClaimRejectsNonPositiveLease(t *testing.T) {
	db := openTestStore(t)
	now := time.Now().UTC()
	seedScoredItem(t, db, "a", now.Add(-time.Hour))
	if _, err := db.ClaimClassification(context.Background(), 1, now, 10, 5, 0); err == nil {
		t.Fatal("a zero lease was accepted")
	}
}

// A stale token must not record a failure after a newer completion: the item
// is ok, the winning worker's verdict stands, and the loser learns it lost.
func TestStaleClaimCannotRecordFailureAfterSuccess(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	lease := 30 * time.Minute
	item := seedScoredItem(t, db, "a", now.Add(-time.Hour))

	winner := claimOne(t, db, 1, now, lease)
	if _, err := db.ReplaceItemTags(
		ctx, item.ID, 1, winner.ClaimToken, []string{"rust"}, nil); err != nil {
		t.Fatalf("winner's completion: %v", err)
	}

	err := db.MarkClassificationFailed(
		ctx, item.ID, 1, 3, "late failure", now.Add(time.Hour), winner.ClaimToken)
	if !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("stale failure write error = %v, want ErrStaleClaim", err)
	}
	state, ok, err := db.ClassificationState(ctx, item.ID)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if state.Status != ClassificationOK || state.Attempts != 0 || state.LastError != "" {
		t.Fatalf("stale failure overwrote the success: %+v", state)
	}
}

// A stale token must not replace a newer worker's tags: the re-claim that
// invalidated the token owns the item now.
func TestStaleClaimCannotReplaceNewerTags(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	lease := time.Minute
	item := seedScoredItem(t, db, "a", now.Add(-time.Hour))

	slow := claimOne(t, db, 1, now, lease)
	// The slow worker's lease lapses; another worker re-claims and completes.
	fast := claimOne(t, db, 1, now.Add(2*lease), lease)
	if _, err := db.ReplaceItemTags(
		ctx, item.ID, 1, fast.ClaimToken, []string{"rust"}, nil); err != nil {
		t.Fatalf("fast worker's completion: %v", err)
	}

	if _, err := db.ReplaceItemTags(
		ctx, item.ID, 1, slow.ClaimToken, []string{"go", "stale"}, nil); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("stale tag write error = %v, want ErrStaleClaim", err)
	}
	tags, err := db.TagsForItem(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 1 || tags[0].Slug != "rust" {
		t.Fatalf("stale write replaced the newer tags: %+v", tags)
	}
	state, ok, err := db.ClassificationState(ctx, item.ID)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if state.Status != ClassificationOK || state.ClaimToken != "" {
		t.Fatalf("stale write corrupted the completion state: %+v", state)
	}
}

// Two completions racing on one item cannot interleave: the tags and the ok
// state move in the same transaction, and exactly one claim's verdict — the
// one the queue still honours — survives.
func TestConcurrentCompletionKeepsTagsAndStatusConsistent(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	lease := time.Minute
	item := seedScoredItem(t, db, "a", now.Add(-time.Hour))

	first := claimOne(t, db, 1, now, lease)
	second := claimOne(t, db, 1, now.Add(2*lease), lease)

	// The newer claim wins regardless of write order.
	if _, err := db.ReplaceItemTags(
		ctx, item.ID, 1, first.ClaimToken, []string{"go"}, []string{"stale"}); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("older claim's write error = %v, want ErrStaleClaim", err)
	}
	if _, err := db.ReplaceItemTags(
		ctx, item.ID, 1, second.ClaimToken, []string{"rust"}, []string{"fast"}); err != nil {
		t.Fatalf("current claim's write: %v", err)
	}

	tags, err := db.TagsForItem(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 2 {
		t.Fatalf("tags = %+v, want exactly the current claim's verdict", tags)
	}
	bySlug := map[string]ItemTag{}
	for _, tag := range tags {
		bySlug[tag.Slug] = tag
	}
	if bySlug["rust"].Kind != TagKindKeyword || bySlug["fast"].Kind != TagKindAssumption {
		t.Fatalf("tag kinds mixed claims: %+v", tags)
	}
	state, ok, err := db.ClassificationState(ctx, item.ID)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if state.Status != ClassificationOK || state.ClaimToken != "" || state.Attempts != 0 {
		t.Fatalf("completion state inconsistent: %+v", state)
	}
}

// --- Version-aware retry counts (F6) ---

// A version bump restores the full retry budget: the failed-at-v1 item with
// attempts exhausted and a year-long backoff is claimable at v2 with attempts
// reset to zero — and the old claim's token no longer authorizes writes.
func TestClaimVersionBumpResetsAttemptsAndOwnership(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	lease := 30 * time.Minute
	item := seedScoredItem(t, db, "a", now.Add(-time.Hour))

	first := claimOne(t, db, 1, now, lease)
	if err := db.MarkClassificationFailed(
		ctx, item.ID, 1, 5, "terminal at v1", now.Add(365*24*time.Hour), first.ClaimToken); err != nil {
		t.Fatalf("MarkClassificationFailed: %v", err)
	}
	// Within the same version the item stays parked.
	same, err := db.ClaimClassification(ctx, 1, now.Add(2*lease), 10, 5, lease)
	if err != nil {
		t.Fatal(err)
	}
	if len(same) != 0 {
		t.Fatalf("exhausted item reclaimed within its version: %+v", same)
	}

	bumped := claimOne(t, db, 2, now.Add(2*lease), lease)
	if bumped.Attempts != 0 {
		t.Fatalf("version bump kept attempts = %d, want a reset to zero", bumped.Attempts)
	}
	state, ok, err := db.ClassificationState(ctx, item.ID)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if state.ClassifierVersion != 2 || state.Status != ClassificationPending {
		t.Fatalf("after version bump state = %+v", state)
	}

	// The v1 claim cannot record a failure against the v2 queue.
	err = db.MarkClassificationFailed(
		ctx, item.ID, 1, 6, "late v1 failure", now.Add(time.Hour), first.ClaimToken)
	if !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("stale v1 failure error = %v, want ErrStaleClaim", err)
	}
}

// A manual enqueue at a new version must invalidate an in-flight claim at the
// old one: the re-classification belongs to whoever claims next, not to the
// worker whose verdict the bump just made obsolete.
func TestEnqueueVersionBumpInvalidatesOwnership(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	lease := 30 * time.Minute
	item := seedScoredItem(t, db, "a", now.Add(-time.Hour))

	claimed := claimOne(t, db, 1, now, lease)
	if _, err := db.EnqueueClassification(ctx, []string{item.ID}, 2, false); err != nil {
		t.Fatalf("EnqueueClassification: %v", err)
	}
	if _, err := db.ReplaceItemTags(
		ctx, item.ID, 2, claimed.ClaimToken, []string{"obsolete"}, nil); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("pre-bump claim still owned the item: %v", err)
	}

	// The bumped queue hands the item to the next claim with a fresh budget.
	next := claimOne(t, db, 2, now, lease)
	if next.Attempts != 0 || next.ClaimToken == claimed.ClaimToken {
		t.Fatalf("bumped claim = %+v, want a fresh token and attempts 0", next)
	}
}

// --- Label safety (A3) ---

func TestSanitizeLabelStripsControlAndFormatRunes(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"ANSI color":        {"go\x1b[31mlang", "go[31mlang"},
		"OSC title":         {"ru\x1b]0;pwned\x07st", "ru]0;pwnedst"},
		"C0 controls":       {"a\x00b\x01c\x7fd", "abcd"},
		"C1 controls":       {"a\u009bb", "ab"},
		"bidi override":     {"a\u202Eb", "ab"},
		"ZWJ preserved":     {"rust\u200dcraft", "rust\u200dcraft"},
		"ZWNJ preserved":    {"نیم\u200cفاصله", "نیم\u200cفاصله"},
		"multilingual kept": {"日本語 và Україна", "日本語 và Україна"},
		"plain ascii":       {"vLLM", "vLLM"},
	} {
		if got := sanitizeLabel(tc.in); got != tc.want {
			t.Errorf("%s: sanitizeLabel(%q) = %q, want %q", name, tc.in, got, tc.want)
		}
	}
}

// The boundary must hold end to end: a label the model laced with escape
// sequences is stored clean, and what the CLI later reads back cannot replay
// them.
func TestReplaceItemTagsSanitizesStoredLabels(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	item := seedScoredItem(t, db, "a", now.Add(-time.Hour))

	if _, err := db.ReplaceItemTags(ctx, item.ID, 1, "",
		[]string{"go\x1b[31mlang", "ru\x1b]0;pwned\x07st"}, nil); err != nil {
		t.Fatalf("ReplaceItemTags: %v", err)
	}
	tags, err := db.TagsForItem(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 2 {
		t.Fatalf("tags = %+v, want both sanitized labels kept", tags)
	}
	for _, tag := range tags {
		if tag.Label != sanitizeLabel(tag.Label) {
			t.Errorf("stored label %q still carries control characters", tag.Label)
		}
		if strings.ContainsRune(tag.Label, '\x1b') {
			t.Errorf("stored label %q still carries ESC", tag.Label)
		}
	}
}

func TestAddTagAliasSanitizesCanonicalLabel(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	if err := db.AddTagAlias(ctx, "k8s", "Kuber\x1b[31netes"); err != nil {
		t.Fatalf("AddTagAlias: %v", err)
	}
	listed, err := db.ListTags(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range listed {
		if tag.Slug == "kubernetes" && tag.Label != "Kuber[31netes" {
			t.Errorf("canonical label = %q, want the sanitized form", tag.Label)
		}
	}
}

// Several workers claiming at the same time must partition the queue: with
// live leases no item is handed out twice, and together the workers take
// everything. This is the race the claim transaction exists for, run under the
// race detector by the suite.
func TestConcurrentClaimsPartitionTheQueue(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	lease := 30 * time.Minute
	const itemCount, workerCount = 20, 4
	for i := range itemCount {
		seedScoredItem(t, db, string(rune('a'+i)), now.Add(-time.Duration(itemCount-i)*time.Hour))
	}

	var (
		mu      sync.Mutex
		claimed = map[string]int{} // item id -> times claimed
		wg      sync.WaitGroup
	)
	start := make(chan struct{})
	for range workerCount {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for {
				claims, err := db.ClaimClassification(ctx, 1, now, 10, 5, lease)
				if err != nil {
					t.Errorf("ClaimClassification: %v", err)
					return
				}
				if len(claims) == 0 {
					return
				}
				mu.Lock()
				for _, c := range claims {
					claimed[c.Item.ID]++
				}
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(claimed) != itemCount {
		t.Fatalf("workers claimed %d distinct items, want %d", len(claimed), itemCount)
	}
	for id, n := range claimed {
		if n != 1 {
			t.Errorf("item %s was claimed %d times while its lease was live", id, n)
		}
	}
}
