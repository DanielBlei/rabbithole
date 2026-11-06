// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/feeds"
)

var (
	statsNow  = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	statsOpts = SourceStatsOpts{High: 7, Mid: 4, ThumbUp: 10, ThumbDown: 0}
)

// seedStatItem records one item and backdates its created_at. A nil score
// records it unscored.
func seedStatItem(t *testing.T, db *Store, id, source string, created time.Time, score *int) {
	t.Helper()
	ctx := context.Background()
	it := feeds.Item{ID: id, Source: source, Title: id, Link: "https://x/" + id}
	var scored []DigestEntry
	if score != nil {
		scored = []DigestEntry{{Item: it, Score: *score, Reason: "r"}}
	}
	if err := db.Record(ctx, []feeds.Item{it}, scored, statsNow); err != nil {
		t.Fatalf("Record %s: %v", id, err)
	}
	if _, err := db.exec(ctx, "UPDATE items SET created_at = ? WHERE id = ?", sqlTime(created), id); err != nil {
		t.Fatalf("backdate %s: %v", id, err)
	}
}

func intp(n int) *int { return &n }

func statsFor(t *testing.T, db *Store) map[string]SourceStat {
	t.Helper()
	got, err := db.SourceStats(context.Background(), statsNow, statsOpts)
	if err != nil {
		t.Fatalf("SourceStats: %v", err)
	}
	return got
}

func TestSourceStatsEmptyStore(t *testing.T) {
	db := openTestStore(t)
	got := statsFor(t, db)
	if got == nil || len(got) != 0 {
		t.Errorf("got %v, want an empty map", got)
	}
}

func TestSourceStatsWeekBoundaries(t *testing.T) {
	db := openTestStore(t)
	// Postgres keeps microseconds, so the edge offset is one.
	const eps = time.Microsecond
	bound := func(k int) time.Time { return statsNow.Add(-7 * 24 * time.Hour * time.Duration(8-k)) }

	// An item exactly at b_k lands in week k; b_k - eps lands in week k-1.
	seq := 0
	add := func(at time.Time) {
		seq++
		seedStatItem(t, db, fmt.Sprintf("w%d", seq), "S", at, nil)
	}
	for k := 0; k < 8; k++ {
		add(bound(k))
		if k > 0 {
			add(bound(k).Add(-eps))
		}
	}
	add(bound(0).Add(-eps))     // older than the window: counted nowhere
	add(statsNow)               // newest week
	add(statsNow.Add(-2 * eps)) // newest week

	got := statsFor(t, db)["S"]
	// Week k holds b_k plus b_{k+1}-eps; week 7 holds b_7 plus now and now-2eps.
	want := [8]int{2, 2, 2, 2, 2, 2, 2, 3}
	if got.Weeks != want {
		t.Errorf("Weeks = %v, want %v", got.Weeks, want)
	}
	if sum := 2*7 + 3; got.PerWeek() != float64(sum)/8 {
		t.Errorf("PerWeek = %v, want %v", got.PerWeek(), float64(sum)/8)
	}
	if !got.Last.Equal(statsNow) {
		t.Errorf("Last = %v, want %v", got.Last, statsNow)
	}
}

func TestSourceStatsScoreTiers(t *testing.T) {
	db := openTestStore(t)
	at := statsNow.Add(-24 * time.Hour)
	for i, score := range []*int{intp(10), intp(7), intp(6), intp(4), intp(3), intp(0), nil, nil} {
		seedStatItem(t, db, fmt.Sprintf("t%d", i), "S", at, score)
	}
	got := statsFor(t, db)["S"]
	if got.High != 2 || got.Mid != 2 || got.Low != 2 || got.Unscored != 2 || got.Scored != 6 {
		t.Errorf("tiers high=%d mid=%d low=%d unscored=%d scored=%d, want 2 2 2 2 6",
			got.High, got.Mid, got.Low, got.Unscored, got.Scored)
	}
}

func TestSourceStatsCohortWindow(t *testing.T) {
	db := openTestStore(t)
	d30 := statsNow.AddDate(0, 0, -30)
	seedStatItem(t, db, "in", "S", d30, intp(9))
	seedStatItem(t, db, "out", "S", d30.Add(-time.Microsecond), intp(9))
	got := statsFor(t, db)["S"]
	if got.High != 1 || got.Scored != 1 {
		t.Errorf("high=%d scored=%d, want only the item inside 30 days", got.High, got.Scored)
	}
	sum := 0
	for _, n := range got.Weeks {
		sum += n
	}
	if sum != 2 {
		t.Errorf("weeks sum = %d, want both items counted in the 8-week volume", sum)
	}
}

func TestSourceStatsEngagement(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	at := statsNow.Add(-48 * time.Hour)
	for i := 0; i < 7; i++ {
		seedStatItem(t, db, fmt.Sprintf("e%d", i), "S", at, intp(5))
	}
	// An old item with state does not count: the cohort is the last 30 days.
	seedStatItem(t, db, "old", "S", statsNow.Add(-40*24*time.Hour), intp(5))

	read, skipped, yes := StatusRead, StatusSkipped, true
	up, down, mid := 10, 0, 5
	patches := map[string]UserPatch{
		"e0":  {Status: &read},
		"e1":  {Status: &read, UserScore: &up},
		"e2":  {Status: &skipped},
		"e3":  {Bookmarked: &yes},
		"e4":  {UserScore: &down},
		"e5":  {UserScore: &mid},
		"old": {Status: &read, Bookmarked: &yes, UserScore: &up},
	}
	for id, p := range patches {
		if err := db.UpdateUserState(ctx, id, p); err != nil {
			t.Fatalf("UpdateUserState %s: %v", id, err)
		}
	}
	got := statsFor(t, db)["S"]
	if got.Read != 2 || got.Skipped != 1 || got.Bookmarked != 1 || got.ThumbsUp != 1 || got.ThumbsDown != 1 {
		t.Errorf("read=%d skipped=%d bookmarked=%d up=%d down=%d, want 2 1 1 1 1",
			got.Read, got.Skipped, got.Bookmarked, got.ThumbsUp, got.ThumbsDown)
	}
}

func TestSourceStatsLastBeyondWindow(t *testing.T) {
	db := openTestStore(t)
	old := statsNow.Add(-100 * 24 * time.Hour)
	seedStatItem(t, db, "q1", "Quiet", old.Add(-time.Hour), nil)
	seedStatItem(t, db, "q2", "Quiet", old, nil)

	got := statsFor(t, db)
	q, ok := got["Quiet"]
	if !ok {
		t.Fatal("a source with only old items still reports its last item")
	}
	if !q.Last.Equal(old) {
		t.Errorf("Last = %v, want %v", q.Last, old)
	}
	if q.PerWeek() != 0 || q.Scored != 0 {
		t.Errorf("old items count nowhere in the window, got %+v", q)
	}
	if q.Last.Location() != time.UTC {
		t.Errorf("Last location = %v, want UTC", q.Last.Location())
	}
}

func TestSourceStatsSourcesDoNotMix(t *testing.T) {
	db := openTestStore(t)
	at := statsNow.Add(-time.Hour)
	seedStatItem(t, db, "a1", "A", at, intp(9))
	seedStatItem(t, db, "a2", "A", at, intp(9))
	seedStatItem(t, db, "b1", "B", at.Add(-time.Hour), intp(1))

	got := statsFor(t, db)
	if len(got) != 2 {
		t.Fatalf("got %d sources, want 2", len(got))
	}
	a, b := got["A"], got["B"]
	if a.Weeks[7] != 2 || a.High != 2 || a.Low != 0 || !a.Last.Equal(at) {
		t.Errorf("A = %+v", a)
	}
	if b.Weeks[7] != 1 || b.High != 0 || b.Low != 1 || !b.Last.Equal(at.Add(-time.Hour)) {
		t.Errorf("B = %+v", b)
	}
}
