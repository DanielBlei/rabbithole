// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/config"
)

func feedCount(t *testing.T, db *Store) int {
	t.Helper()
	var n int
	if err := db.queryRow(t.Context(), `SELECT COUNT(*) FROM feeds`).Scan(&n); err != nil {
		t.Fatalf("count feeds: %v", err)
	}
	return n
}

func TestPlanImportClassifiesEveryStatus(t *testing.T) {
	db := openTestStore(t)
	insertRawFeed(t, db, "Live", "https://live.test/feed")
	goneID := insertRawFeed(t, db, "Gone", "https://gone.test/feed")
	if err := db.SoftDeleteFeed(t.Context(), goneID); err != nil {
		t.Fatalf("SoftDeleteFeed: %v", err)
	}
	insertRawFeed(t, db, "Held", "https://held.test/feed")

	doc := config.FeedsDoc{Feeds: []config.Feed{
		{Name: "Fresh", URL: "https://fresh.test/feed"},
		{Name: "Whatever", URL: "https://Live.test/feed/"},
		{Name: "Gone", URL: "https://gone.test/feed"},
		{Name: "Held", URL: "https://other.test/feed"},
		{Name: "Bad scheme", URL: "ftp://bad.test/feed"},
	}}
	plan, err := db.PlanImport(t.Context(), doc)
	if err != nil {
		t.Fatalf("PlanImport: %v", err)
	}
	want := []PlanStatus{PlanNew, PlanSubscribed, PlanRemoved, PlanConflict, PlanInvalid}
	if len(plan) != len(want) {
		t.Fatalf("plan = %+v, want %d rows", plan, len(want))
	}
	for i, w := range want {
		if plan[i].Status != w {
			t.Errorf("row %d (%s) status = %q, want %q", i, doc.Feeds[i].Name, plan[i].Status, w)
		}
		if plan[i].Reason == "" && w != PlanNew {
			t.Errorf("row %d has no reason", i)
		}
	}
	if plan[1].ExistingName != "Live" || plan[1].Reason != "Already in your sources as Live" {
		t.Errorf("subscribed row = %+v, want the stored name", plan[1])
	}
	if plan[0].Key != config.FeedURLKey("https://fresh.test/feed") {
		t.Errorf("Key = %q, want the url key", plan[0].Key)
	}
}

func TestPlanImportConflicts(t *testing.T) {
	db := openTestStore(t)
	goneID := insertRawFeed(t, db, "Gone", "https://gone.test/feed")
	insertRawFeed(t, db, "Holder", "https://holder.test/feed")
	if err := db.SoftDeleteFeed(t.Context(), goneID); err != nil {
		t.Fatalf("SoftDeleteFeed: %v", err)
	}

	plan, err := db.PlanImport(t.Context(), config.FeedsDoc{Feeds: []config.Feed{
		// A removed row whose file name belongs to another row.
		{Name: "Holder", URL: "https://gone.test/feed"},
		{Name: "Dup", URL: "https://dup.test/feed"},
		// Same key as the previous entry.
		{Name: "Dup elsewhere", URL: "https://DUP.test/feed/"},
		// Same name as an earlier new entry.
		{Name: "Dup", URL: "https://dup2.test/feed"},
	}})
	if err != nil {
		t.Fatalf("PlanImport: %v", err)
	}
	want := []struct {
		status PlanStatus
		reason string
	}{
		{PlanConflict, planReasonNameTaken},
		{PlanNew, ""},
		{PlanConflict, planReasonDuplicate},
		{PlanConflict, planReasonDuplicate},
	}
	for i, w := range want {
		if plan[i].Status != w.status || plan[i].Reason != w.reason {
			t.Errorf("row %d = %q %q, want %q %q", i, plan[i].Status, plan[i].Reason, w.status, w.reason)
		}
	}
}

func TestPlanImportWritesNothing(t *testing.T) {
	db := openTestStore(t)
	insertRawFeed(t, db, "Live", "https://live.test/feed")
	before := feedCount(t, db)
	if _, err := db.PlanImport(t.Context(), config.FeedsDoc{Feeds: []config.Feed{
		{Name: "A", URL: "https://a.test/feed"},
		{Name: "B", URL: "https://b.test/feed"},
	}}); err != nil {
		t.Fatalf("PlanImport: %v", err)
	}
	if got := feedCount(t, db); got != before {
		t.Errorf("feeds = %d after a plan, want %d", got, before)
	}
}

func TestApplyImportAddsAndRestores(t *testing.T) {
	db := openTestStore(t)
	goneID := insertRawFeed(t, db, "Gone", "https://gone.test/feed")
	if err := db.RecordFeedFetches(t.Context(), []FeedFetch{{
		FeedID: goneID, Name: "Gone", URL: "https://gone.test/feed", Items: 3, At: time.Now(),
	}}); err != nil {
		t.Fatalf("RecordFeedFetches: %v", err)
	}
	if err := db.SoftDeleteFeed(t.Context(), goneID); err != nil {
		t.Fatalf("SoftDeleteFeed: %v", err)
	}

	doc := config.FeedsDoc{Feeds: []config.Feed{
		{Name: "One", URL: "https://one.test/feed"},
		{Name: "Two", URL: "https://two.test/feed", MaxItems: feedPtrInt(7)},
		{Name: "Three", URL: "https://three.test/feed"},
		{Name: "Gone again", URL: "https://gone.test/feed", Tags: []string{"x"}},
	}}
	pick := map[string]bool{
		config.FeedURLKey("https://one.test/feed"):  true,
		config.FeedURLKey("https://two.test/feed"):  true,
		config.FeedURLKey("https://gone.test/feed"): true,
	}
	res, err := db.ApplyImport(t.Context(), doc, pick)
	if err != nil {
		t.Fatalf("ApplyImport: %v", err)
	}
	if res != (ApplyResult{Added: 2, Restored: 1}) {
		t.Errorf("result = %+v, want 2 added, 1 restored", res)
	}

	live := liveFeeds(t, db)
	if len(live) != 3 {
		t.Fatalf("live = %+v, want One, Two and the restored feed", live)
	}
	restored := feedNamed(t, live, "Gone again")
	if restored.ID != goneID {
		t.Errorf("restored id = %q, want %q", restored.ID, goneID)
	}
	if len(restored.Tags) != 1 || restored.Tags[0] != "x" {
		t.Errorf("restored tags = %v, want the file's values", restored.Tags)
	}
	if got := feedNamed(t, live, "Two"); got.MaxItems == nil || *got.MaxItems != 7 {
		t.Errorf("Two = %+v, want max_items 7", got)
	}

	var fetches int
	if err := db.queryRow(t.Context(), `SELECT COUNT(*) FROM feed_fetches WHERE feed_id = ?`, goneID).
		Scan(&fetches); err != nil {
		t.Fatalf("count fetches: %v", err)
	}
	if fetches != 1 {
		t.Errorf("fetch rows for the restored feed = %d, want its history kept", fetches)
	}
}

func TestApplyImportSkipsStalePicks(t *testing.T) {
	db := openTestStore(t)
	insertRawFeed(t, db, "Live", "https://live.test/feed")
	doc := config.FeedsDoc{Feeds: []config.Feed{
		{Name: "Live", URL: "https://live.test/feed"},
		{Name: "Fresh", URL: "https://fresh.test/feed"},
	}}
	res, err := db.ApplyImport(t.Context(), doc, map[string]bool{
		config.FeedURLKey("https://live.test/feed"):  true, // subscribed since the preview
		config.FeedURLKey("https://fresh.test/feed"): true,
		"https://unknown.test/feed":                  true, // not in the file at all
		config.FeedURLKey("https://off.test/feed"):   false,
	})
	if err != nil {
		t.Fatalf("ApplyImport: %v", err)
	}
	if res != (ApplyResult{Added: 1, Skipped: 2}) {
		t.Errorf("result = %+v, want 1 added, 2 skipped", res)
	}
}

func TestApplyImportNothingPickedWritesNothing(t *testing.T) {
	db := openTestStore(t)
	doc := config.FeedsDoc{Feeds: []config.Feed{{Name: "Fresh", URL: "https://fresh.test/feed"}}}
	for name, pick := range map[string]map[string]bool{
		"empty":   {},
		"nil":     nil,
		"unknown": {"https://nope.test/feed": true},
	} {
		if _, err := db.ApplyImport(t.Context(), doc, pick); !errors.Is(err, ErrNothingToImport) {
			t.Errorf("%s: err = %v, want ErrNothingToImport", name, err)
		}
	}
	if n := feedCount(t, db); n != 0 {
		t.Errorf("feeds = %d, want none written", n)
	}
}

// A trigger rejects the second picked row at insert, after the first row went
// in, so the plan sees nothing to object to. The first row's insert must roll
// back with it. SQLite and Postgres spell triggers differently.
func TestApplyImportRollsBackOnFailure(t *testing.T) {
	db := openTestStore(t)
	ctx := t.Context()
	ddl, drop := []string{
		`CREATE TRIGGER fail_boom BEFORE INSERT ON feeds WHEN NEW.name = 'Boom'
			BEGIN SELECT RAISE(ABORT, 'boom'); END`,
	}, []string{`DROP TRIGGER fail_boom`}
	if onPostgres() {
		ddl = []string{
			`CREATE FUNCTION fail_boom() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'boom'; END $$ LANGUAGE plpgsql`,
			`CREATE TRIGGER fail_boom BEFORE INSERT ON feeds FOR EACH ROW WHEN (NEW.name = 'Boom')
				EXECUTE FUNCTION fail_boom()`,
		}
		drop = []string{`DROP TRIGGER fail_boom ON feeds`, `DROP FUNCTION fail_boom()`}
	}
	for _, q := range ddl {
		if _, err := db.exec(ctx, q); err != nil {
			t.Fatalf("create trigger: %v", err)
		}
	}
	t.Cleanup(func() {
		for _, q := range drop {
			_, _ = db.exec(context.Background(), q)
		}
	})
	before := feedCount(t, db)

	doc := config.FeedsDoc{Feeds: []config.Feed{
		{Name: "First", URL: "https://first.test/feed"},
		{Name: "Boom", URL: "https://boom.test/feed"},
	}}
	_, err := db.ApplyImport(ctx, doc, map[string]bool{
		config.FeedURLKey("https://first.test/feed"): true,
		config.FeedURLKey("https://boom.test/feed"):  true,
	})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want the trigger failure", err)
	}
	if got := feedCount(t, db); got != before {
		t.Errorf("feeds = %d after a failed import, want %d (first row rolled back)", got, before)
	}
}

// A source whose address was edited keeps the id of the old address, so a file
// that lists the old address would insert onto a taken primary key.
func TestImportPlansIDSquatterAsConflict(t *testing.T) {
	db := openTestStore(t)
	ctx := t.Context()
	const old = "https://old.test/feed"
	id, err := db.AddFeed(ctx, config.Feed{Name: "Edited", URL: old})
	if err != nil {
		t.Fatalf("AddFeed: %v", err)
	}
	if err := db.UpdateFeed(ctx, id, config.Feed{Name: "Edited", URL: "https://new.test/feed"}); err != nil {
		t.Fatalf("UpdateFeed: %v", err)
	}

	doc := config.FeedsDoc{Feeds: []config.Feed{
		{Name: "Old again", URL: old},
		{Name: "Fresh", URL: "https://fresh.test/feed"},
	}}
	plan, err := db.PlanImport(ctx, doc)
	if err != nil {
		t.Fatalf("PlanImport: %v", err)
	}
	if plan[0].Status != PlanConflict || plan[0].Reason != planReasonIDTaken {
		t.Errorf("row 0 = %q %q, want the id conflict", plan[0].Status, plan[0].Reason)
	}
	if plan[1].Status != PlanNew {
		t.Errorf("row 1 = %q, want new", plan[1].Status)
	}

	res, err := db.ApplyImport(ctx, doc, map[string]bool{config.FeedURLKey(old): true})
	if !errors.Is(err, ErrNothingToImport) || res.Skipped != 1 {
		t.Errorf("apply = %+v, %v, want the squatter skipped", res, err)
	}
	if n := feedCount(t, db); n != 1 {
		t.Errorf("feeds = %d, want only the edited source", n)
	}
}

func TestPlanImportNameHeldByDeletedRow(t *testing.T) {
	db := openTestStore(t)
	ctx := t.Context()
	goneID := insertRawFeed(t, db, "Gone", "https://gone.test/feed")
	if err := db.SoftDeleteFeed(ctx, goneID); err != nil {
		t.Fatalf("SoftDeleteFeed: %v", err)
	}
	plan, err := db.PlanImport(ctx, config.FeedsDoc{Feeds: []config.Feed{
		{Name: "Gone", URL: "https://other.test/feed"},
	}})
	if err != nil {
		t.Fatalf("PlanImport: %v", err)
	}
	if plan[0].Status != PlanConflict || plan[0].Reason != planReasonNameGone {
		t.Errorf("row = %q %q, want the removed-name conflict", plan[0].Status, plan[0].Reason)
	}
}

// Deleted twins of one key: the one carrying the file's name is restored, else
// the lowest id, whatever order the scan runs in.
func TestPlanImportPicksDeletedTwin(t *testing.T) {
	db := openTestStore(t)
	ctx := t.Context()
	a := insertRawFeed(t, db, "Twin A", "https://twin.test/feed")
	b := insertRawFeed(t, db, "Twin B", "https://twin.test/feed/")
	for _, id := range []string{a, b} {
		if err := db.SoftDeleteFeed(ctx, id); err != nil {
			t.Fatalf("SoftDeleteFeed: %v", err)
		}
	}
	low := min(a, b)

	for name, want := range map[string]string{"Twin A": a, "Twin B": b, "Twin C": low} {
		plan, err := db.PlanImport(ctx, config.FeedsDoc{Feeds: []config.Feed{
			{Name: name, URL: "https://twin.test/feed"},
		}})
		if err != nil {
			t.Fatalf("PlanImport: %v", err)
		}
		if plan[0].Status != PlanRemoved || plan[0].id != want {
			t.Errorf("%s: row = %q id %q, want removed id %q", name, plan[0].Status, plan[0].id, want)
		}
	}
}
