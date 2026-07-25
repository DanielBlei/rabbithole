package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// openTestStore opens a throwaway store, closed on cleanup.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// attempt is a compact description of one recorded fetch for test tables:
// an empty errMsg means success, and ageHours places it in the past.
type attempt struct {
	errMsg   string
	items    int
	ageHours int
}

// recordAttempts writes the given attempts for one feed, oldest first.
func recordAttempts(t *testing.T, db *Store, feedID, name string, attempts []attempt) {
	t.Helper()
	base := time.Now().Truncate(time.Second)
	for i, a := range attempts {
		f := FeedFetch{
			FeedID: feedID,
			Name:   name,
			URL:    "http://" + name + ".test/feed",
			Items:  a.items,
			Error:  a.errMsg,
			At:     base.Add(-time.Duration(a.ageHours) * time.Hour),
		}
		if err := db.RecordFeedFetches(context.Background(), []FeedFetch{f}); err != nil {
			t.Fatalf("record attempt %d: %v", i, err)
		}
	}
}

// attemptsFor reads one feed's recorded attempts, newest first — via the same
// aggregate path the viewer uses, which is the only reader of this history.
func attemptsFor(t *testing.T, db *Store, feedID string) []FeedAttempt {
	t.Helper()
	health, err := db.FeedHealthByID(context.Background(), feedRecentAll)
	if err != nil {
		t.Fatalf("FeedHealthByID: %v", err)
	}
	return health[feedID].Recent
}

func mustHealth(t *testing.T, db *Store, feedID string, recent int) FeedHealth {
	t.Helper()
	health, err := db.FeedHealthByID(context.Background(), recent)
	if err != nil {
		t.Fatalf("FeedHealthByID: %v", err)
	}
	h, ok := health[feedID]
	if !ok {
		t.Fatalf("no health entry for %q", feedID)
	}
	return h
}

// Health is aggregated from the append-only history: latest attempt wins for
// status, last success is remembered independently, and the streak counts
// failures since that success.
func TestFeedHealthAggregation(t *testing.T) {
	cases := []struct {
		name string
		// attempts are given oldest-first.
		attempts       []attempt
		wantOK         bool
		wantStreak     int
		wantItems      int
		wantErr        string
		wantHasLastOK  bool
		wantFailingNow bool
	}{
		{
			name:          "single success",
			attempts:      []attempt{{items: 12, ageHours: 1}},
			wantOK:        true,
			wantStreak:    0,
			wantItems:     12,
			wantHasLastOK: true,
		},
		{
			// A feed that has never worked has no last-ok to preserve, and its
			// first failure already counts as a streak of one.
			name:           "never succeeded",
			attempts:       []attempt{{errMsg: "404 not found", ageHours: 1}},
			wantOK:         false,
			wantStreak:     1,
			wantErr:        "404 not found",
			wantHasLastOK:  false,
			wantFailingNow: true,
		},
		{
			name: "failing since a past success",
			attempts: []attempt{
				{items: 4, ageHours: 5},
				{errMsg: "timeout", ageHours: 3},
				{errMsg: "timeout", ageHours: 2},
				{errMsg: "timeout", ageHours: 1},
			},
			wantOK:         false,
			wantStreak:     3,
			wantErr:        "timeout",
			wantHasLastOK:  true, // the earlier success survives the failures
			wantFailingNow: true,
		},
		{
			// One success wipes the streak — that's what tells "flaky" apart
			// from "dead since Tuesday".
			name: "recovered after failures",
			attempts: []attempt{
				{errMsg: "timeout", ageHours: 4},
				{errMsg: "timeout", ageHours: 3},
				{items: 7, ageHours: 1},
			},
			wantOK:        true,
			wantStreak:    0,
			wantItems:     7,
			wantHasLastOK: true,
		},
		{
			name: "failed again after recovering",
			attempts: []attempt{
				{errMsg: "timeout", ageHours: 5},
				{items: 7, ageHours: 4},
				{errMsg: "500", ageHours: 1},
			},
			wantOK:         false,
			wantStreak:     1, // only the failure after the last success counts
			wantErr:        "500",
			wantHasLastOK:  true,
			wantFailingNow: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := openTestStore(t)
			recordAttempts(t, db, "feed-1", "Feed", c.attempts)

			h := mustHealth(t, db, "feed-1", len(c.attempts))
			if h.OK() != c.wantOK {
				t.Errorf("OK() = %v (status %q), want %v", h.OK(), h.Status, c.wantOK)
			}
			if h.FailStreak != c.wantStreak {
				t.Errorf("FailStreak = %d, want %d", h.FailStreak, c.wantStreak)
			}
			if h.Items != c.wantItems {
				t.Errorf("Items = %d, want %d", h.Items, c.wantItems)
			}
			if h.Error != c.wantErr {
				t.Errorf("Error = %q, want %q", h.Error, c.wantErr)
			}
			if (h.LastOK != nil) != c.wantHasLastOK {
				t.Errorf("LastOK = %v, want present=%v", h.LastOK, c.wantHasLastOK)
			}
			if _, failing := h.FailingSince(); failing != c.wantFailingNow {
				t.Errorf("FailingSince failing = %v, want %v", failing, c.wantFailingNow)
			}
			if h.LastFetch.IsZero() {
				t.Error("LastFetch should be set")
			}
			// History is append-only: every attempt is still on record.
			if len(h.Recent) != len(c.attempts) {
				t.Errorf("Recent = %d attempts, want all %d retained", len(h.Recent), len(c.attempts))
			}
		})
	}
}

// FailingSince walks back to the first failure of the current run, not the
// most recent one.
func TestFeedHealthFailingSince(t *testing.T) {
	db := openTestStore(t)
	recordAttempts(t, db, "feed-1", "Feed", []attempt{
		{items: 3, ageHours: 10},
		{errMsg: "boom", ageHours: 6}, // the run starts here
		{errMsg: "boom", ageHours: 4},
		{errMsg: "boom", ageHours: 1},
	})

	h := mustHealth(t, db, "feed-1", 10)
	since, failing := h.FailingSince()
	if !failing {
		t.Fatal("expected the feed to be failing")
	}
	// Recent is newest-first; the run began with the 6h-old failure.
	oldest := h.Recent[len(h.Recent)-2].At // index 0 is newest, last is the success
	if !since.Equal(oldest) {
		t.Errorf("FailingSince = %v, want the first failure of the run at %v", since, oldest)
	}
}

func TestFeedHealthByIDMultipleFeeds(t *testing.T) {
	db := openTestStore(t)
	recordAttempts(t, db, "feed-a", "Alpha", []attempt{{items: 5, ageHours: 1}})
	recordAttempts(t, db, "feed-b", "Beta", []attempt{{errMsg: "gone", ageHours: 1}})

	health, err := db.FeedHealthByID(context.Background(), feedRecentAll)
	if err != nil {
		t.Fatalf("FeedHealthByID: %v", err)
	}
	if len(health) != 2 {
		t.Fatalf("got %d feeds, want 2", len(health))
	}
	if !health["feed-a"].OK() {
		t.Error("feed-a should be ok")
	}
	if health["feed-b"].OK() {
		t.Error("feed-b should not be ok")
	}
	// Each feed's aggregate is its own — no cross-contamination from the window.
	if health["feed-a"].FailStreak != 0 || health["feed-b"].FailStreak != 1 {
		t.Errorf("streaks = a:%d b:%d, want a:0 b:1",
			health["feed-a"].FailStreak, health["feed-b"].FailStreak)
	}
}

// recentLimit caps the strip without affecting the aggregate.
func TestFeedHealthRecentLimit(t *testing.T) {
	db := openTestStore(t)
	var attempts []attempt
	for i := 10; i > 0; i-- {
		attempts = append(attempts, attempt{items: i, ageHours: i})
	}
	recordAttempts(t, db, "feed-1", "Feed", attempts)

	cases := []struct {
		name       string
		limit      int
		wantRecent int
	}{
		{name: "no history requested", limit: 0, wantRecent: 0},
		{name: "capped below available", limit: 3, wantRecent: 3},
		{name: "above available", limit: 50, wantRecent: 10},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := mustHealth(t, db, "feed-1", c.limit)
			if len(h.Recent) != c.wantRecent {
				t.Errorf("Recent = %d, want %d", len(h.Recent), c.wantRecent)
			}
			// The aggregate is unaffected by how much history was loaded.
			if !h.OK() || h.Items != 1 {
				t.Errorf("latest attempt = %q/%d items, want ok/1", h.Status, h.Items)
			}
		})
	}
}

// Recent is newest-first, which the viewer relies on to build its strip.
func TestFeedHealthRecentIsNewestFirst(t *testing.T) {
	db := openTestStore(t)
	recordAttempts(t, db, "feed-1", "Feed", []attempt{
		{items: 1, ageHours: 3},
		{items: 2, ageHours: 2},
		{items: 3, ageHours: 1},
	})

	h := mustHealth(t, db, "feed-1", 10)
	if len(h.Recent) != 3 {
		t.Fatalf("Recent = %d, want 3", len(h.Recent))
	}
	for i, wantItems := range []int{3, 2, 1} {
		if h.Recent[i].Items != wantItems {
			t.Errorf("Recent[%d].Items = %d, want %d (newest first)", i, h.Recent[i].Items, wantItems)
		}
	}
}

func TestFeedAttemptsRecorded(t *testing.T) {
	db := openTestStore(t)
	recordAttempts(t, db, "feed-1", "Feed", []attempt{
		{items: 1, ageHours: 3},
		{errMsg: "boom", ageHours: 2},
		{items: 3, ageHours: 1},
	})

	got := attemptsFor(t, db, "feed-1")
	if len(got) != 3 {
		t.Fatalf("got %d attempts, want all 3 retained", len(got))
	}
	// Newest first.
	if !got[0].OK() || got[0].Items != 3 {
		t.Errorf("newest = %+v, want the 3-item success", got[0])
	}
	if got[1].OK() || got[1].Error != "boom" {
		t.Errorf("second = %+v, want the failure", got[1])
	}

	// An unknown feed has no entry at all, and that is not an error.
	if n := len(attemptsFor(t, db, "nope")); n != 0 {
		t.Errorf("unknown feed returned %d attempts, want 0", n)
	}
}

// Append-only needs a ceiling; pruning is per-feed so a busy feed can't crowd
// out a quiet one's history.
func TestPruneFeedFetches(t *testing.T) {
	db := openTestStore(t)
	var busy []attempt
	for i := 20; i > 0; i-- {
		busy = append(busy, attempt{items: i, ageHours: i})
	}
	recordAttempts(t, db, "feed-busy", "Busy", busy)
	recordAttempts(t, db, "feed-quiet", "Quiet", []attempt{{items: 1, ageHours: 1}})

	if err := db.PruneFeedFetches(context.Background(), 5); err != nil {
		t.Fatalf("PruneFeedFetches: %v", err)
	}

	cases := []struct {
		feedID string
		want   int
	}{
		{feedID: "feed-busy", want: 5},  // trimmed to the cap
		{feedID: "feed-quiet", want: 1}, // untouched, well under it
	}
	for _, c := range cases {
		t.Run(c.feedID, func(t *testing.T) {
			if got := attemptsFor(t, db, c.feedID); len(got) != c.want {
				t.Errorf("history = %d rows, want %d", len(got), c.want)
			}
		})
	}

	// The newest rows are the ones kept.
	got := attemptsFor(t, db, "feed-busy")
	if got[0].Items != 1 {
		t.Errorf("newest kept row = %d items, want the most recent (1)", got[0].Items)
	}
}

func TestRecordFeedFetchesEmptyIsNoop(t *testing.T) {
	db := openTestStore(t)
	if err := db.RecordFeedFetches(context.Background(), nil); err != nil {
		t.Fatalf("RecordFeedFetches(nil): %v", err)
	}
	health, err := db.FeedHealthByID(context.Background(), feedRecentAll)
	if err != nil {
		t.Fatalf("FeedHealthByID: %v", err)
	}
	if len(health) != 0 {
		t.Errorf("got %d entries, want none", len(health))
	}
}

// A batch is one transaction: several feeds' outcomes land together.
func TestRecordFeedFetchesBatch(t *testing.T) {
	db := openTestStore(t)
	now := time.Now()
	err := db.RecordFeedFetches(context.Background(), []FeedFetch{
		{FeedID: "a", Name: "Alpha", Items: 3, Elapsed: 250 * time.Millisecond, At: now},
		{FeedID: "b", Name: "Beta", Error: "dial tcp", At: now},
	})
	if err != nil {
		t.Fatalf("RecordFeedFetches: %v", err)
	}
	health, err := db.FeedHealthByID(context.Background(), feedRecentAll)
	if err != nil {
		t.Fatalf("FeedHealthByID: %v", err)
	}
	if len(health) != 2 {
		t.Fatalf("got %d entries, want 2", len(health))
	}
	if health["a"].Elapsed != 250*time.Millisecond {
		t.Errorf("Elapsed = %v, want 250ms", health["a"].Elapsed)
	}
	if health["b"].Error != "dial tcp" {
		t.Errorf("Error = %q, want the fetch error", health["b"].Error)
	}
}

// feedRecentAll asks for more history than any test writes.
const feedRecentAll = 100
