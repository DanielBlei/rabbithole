// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/config"
)

// addBulkFeed stores a feed with the given own values and returns its ID.
func addBulkFeed(t *testing.T, db *Store, name string, enabled *bool, tags ...string) string {
	t.Helper()
	id, err := db.AddFeed(t.Context(), config.Feed{
		Name: name, URL: "https://" + strings.ToLower(name) + ".test/feed", Enabled: enabled, Tags: tags,
	})
	if err != nil {
		t.Fatalf("AddFeed(%s): %v", name, err)
	}
	return id
}

func bulkFeed(t *testing.T, db *Store, id string) config.Feed {
	t.Helper()
	f, err := db.FeedByID(t.Context(), id)
	if err != nil {
		t.Fatalf("FeedByID: %v", err)
	}
	return f
}

func stateIDs(states []FeedState) []string {
	var ids []string
	for _, st := range states {
		ids = append(ids, st.ID)
	}
	return ids
}

func enabledIs(f config.Feed, want *bool) bool {
	if f.Enabled == nil || want == nil {
		return f.Enabled == want
	}
	return *f.Enabled == *want
}

func TestSetFeedsEnabled(t *testing.T) {
	db := openTestStore(t)
	inherit := addBulkFeed(t, db, "Inherit", nil)
	off := addBulkFeed(t, db, "Off", feedPtrBool(false))
	on := addBulkFeed(t, db, "On", feedPtrBool(true))
	gone := addBulkFeed(t, db, "Gone", nil)
	if err := db.SoftDeleteFeed(t.Context(), gone); err != nil {
		t.Fatalf("SoftDeleteFeed: %v", err)
	}

	got, err := db.SetFeedsEnabled(t.Context(), []string{inherit, off, on, gone, "missing"}, true)
	if err != nil {
		t.Fatalf("SetFeedsEnabled: %v", err)
	}
	if want := []string{inherit, off}; !slices.Equal(stateIDs(got), want) {
		t.Fatalf("changed = %v, want %v (already-on, removed and unknown ids skipped)", stateIDs(got), want)
	}
	if got[0].Enabled != nil {
		t.Errorf("state of the inheriting row = %v, want nil", got[0].Enabled)
	}
	if got[1].Enabled == nil || *got[1].Enabled {
		t.Errorf("state of the disabled row = %v, want false", got[1].Enabled)
	}
	for _, id := range []string{inherit, off, on} {
		if f := bulkFeed(t, db, id); f.Enabled == nil || !*f.Enabled {
			t.Errorf("%s enabled = %v, want true", f.Name, f.Enabled)
		}
	}
	if f := bulkFeed(t, db, gone); f.Enabled != nil {
		t.Errorf("removed row enabled = %v, want it untouched (nil)", f.Enabled)
	}

	got, err = db.SetFeedsEnabled(t.Context(), []string{inherit, off, on}, false)
	if err != nil || len(got) != 3 {
		t.Fatalf("disable = %v, %v, want 3 changed", stateIDs(got), err)
	}
	if f := bulkFeed(t, db, on); f.Enabled == nil || *f.Enabled {
		t.Errorf("On enabled = %v, want false", f.Enabled)
	}
}

func TestSetFeedsEnabledSkipsKeepInherit(t *testing.T) {
	db := openTestStore(t)
	off := addBulkFeed(t, db, "Off", feedPtrBool(false))
	inherit := addBulkFeed(t, db, "Inherit", nil)

	got, err := db.SetFeedsEnabled(t.Context(), []string{off}, false)
	if err != nil || len(got) != 0 {
		t.Fatalf("disable of a disabled row = %v, %v, want nothing", stateIDs(got), err)
	}
	if f := bulkFeed(t, db, inherit); f.Enabled != nil {
		t.Errorf("untouched row enabled = %v, want nil", f.Enabled)
	}
}

func TestSoftDeleteAndRestoreFeeds(t *testing.T) {
	db := openTestStore(t)
	a := addBulkFeed(t, db, "A", feedPtrBool(true), "x")
	b := addBulkFeed(t, db, "B", nil)

	got, err := db.SoftDeleteFeeds(t.Context(), []string{a, b, "missing"})
	if err != nil {
		t.Fatalf("SoftDeleteFeeds: %v", err)
	}
	if want := []string{a, b}; !slices.Equal(stateIDs(got), want) {
		t.Fatalf("removed = %v, want %v", stateIDs(got), want)
	}
	if got[0].DeletedAt != nil || got[0].Enabled == nil || !*got[0].Enabled ||
		!slices.Equal(got[0].Tags, []string{"x"}) {
		t.Errorf("state before removal = %+v", got[0])
	}
	if len(liveFeeds(t, db)) != 0 {
		t.Error("removed feeds still live")
	}

	again, err := db.SoftDeleteFeeds(t.Context(), []string{a})
	if err != nil || len(again) != 0 {
		t.Errorf("second remove = %v, %v, want nothing", stateIDs(again), err)
	}

	restored, err := db.RestoreFeeds(t.Context(), []string{a, "missing"})
	if err != nil {
		t.Fatalf("RestoreFeeds: %v", err)
	}
	if !slices.Equal(stateIDs(restored), []string{a}) || restored[0].DeletedAt == nil {
		t.Fatalf("restored = %+v, want a with its deleted_at", restored)
	}
	if live := liveFeeds(t, db); len(live) != 1 || live[0].ID != a {
		t.Errorf("live after restore = %+v, want only A", live)
	}

	live, err := db.RestoreFeeds(t.Context(), []string{a})
	if err != nil || len(live) != 0 {
		t.Errorf("restore of a live row = %v, %v, want nothing", stateIDs(live), err)
	}
}

func TestAddFeedsTag(t *testing.T) {
	db := openTestStore(t)
	plain := addBulkFeed(t, db, "Plain", nil)
	has := addBulkFeed(t, db, "Has", nil, "AI", "news")
	gone := addBulkFeed(t, db, "Gone", nil)
	if err := db.SoftDeleteFeed(t.Context(), gone); err != nil {
		t.Fatalf("SoftDeleteFeed: %v", err)
	}
	var twelve []string
	for i := range 12 {
		twelve = append(twelve, "t"+string(rune('a'+i)))
	}
	full := addBulkFeed(t, db, "Full", nil, twelve...)

	got, err := db.AddFeedsTag(t.Context(), []string{plain, has, gone, full, "missing"}, "  ai,  ")
	if err != nil {
		t.Fatalf("AddFeedsTag: %v", err)
	}
	if !slices.Equal(stateIDs(got), []string{plain}) {
		t.Fatalf("changed = %v, want only Plain (Has holds ai, Full is capped)", stateIDs(got))
	}
	if got[0].Tags != nil {
		t.Errorf("state tags = %v, want nil", got[0].Tags)
	}
	if f := bulkFeed(t, db, plain); !slices.Equal(f.Tags, []string{"ai"}) {
		t.Errorf("Plain tags = %v, want [ai]", f.Tags)
	}
	if f := bulkFeed(t, db, has); !slices.Equal(f.Tags, []string{"AI", "news"}) {
		t.Errorf("Has tags = %v, want unchanged", f.Tags)
	}
	if f := bulkFeed(t, db, full); !slices.Equal(f.Tags, twelve) {
		t.Errorf("Full tags = %v, want unchanged", f.Tags)
	}

	got, err = db.AddFeedsTag(t.Context(), []string{has}, "extra")
	if err != nil || len(got) != 1 {
		t.Fatalf("add extra = %v, %v", stateIDs(got), err)
	}
	if f := bulkFeed(t, db, has); !slices.Equal(f.Tags, []string{"AI", "news", "extra"}) {
		t.Errorf("Has tags = %v, want the tag appended", f.Tags)
	}

	if _, err := db.AddFeedsTag(t.Context(), []string{plain}, " , "); err == nil {
		t.Error("an empty tag was accepted")
	}
}

func TestRemoveFeedsTag(t *testing.T) {
	db := openTestStore(t)
	two := addBulkFeed(t, db, "Two", nil, "AI", "news")
	one := addBulkFeed(t, db, "One", nil, "ai")
	none := addBulkFeed(t, db, "None", nil, "other")
	gone := addBulkFeed(t, db, "Gone", nil, "ai")
	if err := db.SoftDeleteFeed(t.Context(), gone); err != nil {
		t.Fatalf("SoftDeleteFeed: %v", err)
	}

	got, err := db.RemoveFeedsTag(t.Context(), []string{two, one, none, gone, "missing"}, "Ai")
	if err != nil {
		t.Fatalf("RemoveFeedsTag: %v", err)
	}
	if want := []string{two, one}; !slices.Equal(stateIDs(got), want) {
		t.Fatalf("changed = %v, want %v", stateIDs(got), want)
	}
	if !slices.Equal(got[0].Tags, []string{"AI", "news"}) {
		t.Errorf("state tags = %v, want the list before removal", got[0].Tags)
	}
	if f := bulkFeed(t, db, two); !slices.Equal(f.Tags, []string{"news"}) {
		t.Errorf("Two tags = %v, want [news]", f.Tags)
	}
	if f := bulkFeed(t, db, one); f.Tags != nil {
		t.Errorf("One tags = %v, want none", f.Tags)
	}
	var raw *string
	if err := db.queryRow(t.Context(), `SELECT tags FROM feeds WHERE id = ?`, one).Scan(&raw); err != nil {
		t.Fatalf("read tags: %v", err)
	}
	if raw != nil {
		t.Errorf("stored tags = %q, want NULL after the last tag goes", *raw)
	}
	if f := bulkFeed(t, db, gone); !slices.Equal(f.Tags, []string{"ai"}) {
		t.Errorf("removed row tags = %v, want untouched", f.Tags)
	}
}

// failOnName makes any update of a feed named "Boom" fail, so a batch that
// reaches it fails after earlier ids were written.
func failOnName(t *testing.T, db *Store) {
	t.Helper()
	ctx := t.Context()
	var setup, teardown []string
	if testPGDSN != "" {
		setup = []string{
			`CREATE FUNCTION bulk_boom() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN
					IF NEW.name = 'Boom' THEN RAISE EXCEPTION 'boom'; END IF;
					RETURN NEW;
				END $$`,
			`CREATE TRIGGER bulk_boom BEFORE UPDATE ON feeds FOR EACH ROW EXECUTE FUNCTION bulk_boom()`,
		}
		teardown = []string{`DROP TRIGGER bulk_boom ON feeds`, `DROP FUNCTION bulk_boom()`}
	} else {
		setup = []string{
			`CREATE TRIGGER bulk_boom BEFORE UPDATE ON feeds WHEN NEW.name = 'Boom'
				BEGIN SELECT RAISE(ABORT, 'boom'); END`,
		}
		teardown = []string{`DROP TRIGGER bulk_boom`}
	}
	for _, q := range setup {
		if _, err := db.exec(ctx, q); err != nil {
			t.Fatalf("install failing trigger: %v", err)
		}
	}
	t.Cleanup(func() {
		for _, q := range teardown {
			if _, err := db.exec(context.Background(), q); err != nil {
				t.Errorf("remove failing trigger: %v", err)
			}
		}
	})
}

func TestBulkFeedsRollBackOnFailure(t *testing.T) {
	db := openTestStore(t)
	ok := addBulkFeed(t, db, "Fine", nil, "keep")
	boom := addBulkFeed(t, db, "Boom", nil, "keep")
	failOnName(t, db)
	ctx := t.Context()

	ops := map[string]func() ([]FeedState, error){
		"enable":    func() ([]FeedState, error) { return db.SetFeedsEnabled(ctx, []string{ok, boom}, true) },
		"remove":    func() ([]FeedState, error) { return db.SoftDeleteFeeds(ctx, []string{ok, boom}) },
		"addtag":    func() ([]FeedState, error) { return db.AddFeedsTag(ctx, []string{ok, boom}, "new") },
		"removetag": func() ([]FeedState, error) { return db.RemoveFeedsTag(ctx, []string{ok, boom}, "keep") },
	}
	for name, run := range ops {
		got, err := run()
		if err == nil {
			t.Fatalf("%s succeeded, want the second row to fail", name)
		}
		if got != nil {
			t.Errorf("%s returned %v with an error", name, stateIDs(got))
		}
		f := bulkFeed(t, db, ok)
		if f.Enabled != nil || !slices.Equal(f.Tags, []string{"keep"}) {
			t.Errorf("%s left the first row changed: %+v", name, f)
		}
		if n := len(liveFeeds(t, db)); n != 2 {
			t.Errorf("%s left %d live feeds, want 2", name, n)
		}
	}
}

func TestRevertFeedsEnable(t *testing.T) {
	db := openTestStore(t)
	ctx := t.Context()
	a := addBulkFeed(t, db, "A", nil)
	b := addBulkFeed(t, db, "B", feedPtrBool(false))
	c := addBulkFeed(t, db, "C", nil)

	states, err := db.SetFeedsEnabled(ctx, []string{a, b, c}, true)
	if err != nil || len(states) != 3 {
		t.Fatalf("enable = %v, %v", stateIDs(states), err)
	}
	if err := db.SetFeedEnabled(ctx, c, feedPtrBool(false)); err != nil {
		t.Fatalf("SetFeedEnabled: %v", err)
	}

	n, err := db.RevertFeeds(ctx, BulkEnable, "", states)
	if err != nil || n != 2 {
		t.Fatalf("revert = %d, %v, want 2 (C was changed by hand)", n, err)
	}
	if f := bulkFeed(t, db, a); f.Enabled != nil {
		t.Errorf("A enabled = %v, want nil restored", f.Enabled)
	}
	if f := bulkFeed(t, db, b); !enabledIs(f, feedPtrBool(false)) {
		t.Errorf("B enabled = %v, want false restored", f.Enabled)
	}
	if f := bulkFeed(t, db, c); !enabledIs(f, feedPtrBool(false)) {
		t.Errorf("C enabled = %v, want the hand edit kept", f.Enabled)
	}
}

// Bulk A disables rows 1 and 2, the user re-enables row 2, and undoing A reverts
// only row 1.
func TestRevertFeedsDisableIsConditional(t *testing.T) {
	db := openTestStore(t)
	ctx := t.Context()
	r1 := addBulkFeed(t, db, "R1", feedPtrBool(true))
	r2 := addBulkFeed(t, db, "R2", nil)

	states, err := db.SetFeedsEnabled(ctx, []string{r1, r2}, false)
	if err != nil || len(states) != 2 {
		t.Fatalf("disable = %v, %v", stateIDs(states), err)
	}
	if err := db.SetFeedEnabled(ctx, r2, feedPtrBool(true)); err != nil {
		t.Fatalf("SetFeedEnabled: %v", err)
	}

	n, err := db.RevertFeeds(ctx, BulkDisable, "", states)
	if err != nil || n != 1 {
		t.Fatalf("revert = %d, %v, want 1", n, err)
	}
	if f := bulkFeed(t, db, r1); !enabledIs(f, feedPtrBool(true)) {
		t.Errorf("R1 enabled = %v, want true restored", f.Enabled)
	}
	if f := bulkFeed(t, db, r2); !enabledIs(f, feedPtrBool(true)) {
		t.Errorf("R2 enabled = %v, want the hand edit kept", f.Enabled)
	}
}

func TestRevertFeedsRemoveAndRestore(t *testing.T) {
	db := openTestStore(t)
	ctx := t.Context()
	a := addBulkFeed(t, db, "A", nil)
	b := addBulkFeed(t, db, "B", nil)

	removed, err := db.SoftDeleteFeeds(ctx, []string{a, b})
	if err != nil || len(removed) != 2 {
		t.Fatalf("remove = %v, %v", stateIDs(removed), err)
	}
	if _, err := db.RestoreFeeds(ctx, []string{b}); err != nil {
		t.Fatalf("RestoreFeeds: %v", err)
	}
	n, err := db.RevertFeeds(ctx, BulkRemove, "", removed)
	if err != nil || n != 1 {
		t.Fatalf("undo remove = %d, %v, want 1 (B was already back)", n, err)
	}
	if n := len(liveFeeds(t, db)); n != 2 {
		t.Errorf("live = %d, want 2", n)
	}

	if _, err := db.SoftDeleteFeeds(ctx, []string{a, b}); err != nil {
		t.Fatalf("remove again: %v", err)
	}
	_, before, err := db.DeletedFeedsAt(ctx)
	if err != nil || len(before) != 2 {
		t.Fatalf("DeletedFeedsAt = %v, %v", before, err)
	}
	restored, err := db.RestoreFeeds(ctx, []string{a, b})
	if err != nil || len(restored) != 2 {
		t.Fatalf("restore = %v, %v", stateIDs(restored), err)
	}
	if err := db.SoftDeleteFeed(ctx, b); err != nil {
		t.Fatalf("SoftDeleteFeed: %v", err)
	}
	_, bAfterHand, err := db.DeletedFeedsAt(ctx)
	if err != nil {
		t.Fatalf("DeletedFeedsAt: %v", err)
	}

	n, err = db.RevertFeeds(ctx, BulkRestore, "", restored)
	if err != nil || n != 1 {
		t.Fatalf("undo restore = %d, %v, want 1 (B was removed again by hand)", n, err)
	}
	_, after, err := db.DeletedFeedsAt(ctx)
	if err != nil {
		t.Fatalf("DeletedFeedsAt: %v", err)
	}
	if !after[a].Equal(before[a]) {
		t.Errorf("A deleted_at = %v, want the original %v", after[a], before[a])
	}
	if !after[b].Equal(bAfterHand[b]) {
		t.Errorf("B deleted_at = %v, want the hand removal %v kept", after[b], bAfterHand[b])
	}
}

func TestRevertFeedsTags(t *testing.T) {
	db := openTestStore(t)
	ctx := t.Context()
	a := addBulkFeed(t, db, "A", nil, "x")
	b := addBulkFeed(t, db, "B", nil)
	c := addBulkFeed(t, db, "C", nil, "x")

	added, err := db.AddFeedsTag(ctx, []string{a, b, c}, "new")
	if err != nil || len(added) != 3 {
		t.Fatalf("add = %v, %v", stateIDs(added), err)
	}
	// C is retagged by hand after the bulk.
	if _, err := db.AddFeedsTag(ctx, []string{c}, "later"); err != nil {
		t.Fatalf("AddFeedsTag: %v", err)
	}
	n, err := db.RevertFeeds(ctx, BulkAddTag, "NEW", added)
	if err != nil || n != 2 {
		t.Fatalf("undo addtag = %d, %v, want 2", n, err)
	}
	if f := bulkFeed(t, db, a); !slices.Equal(f.Tags, []string{"x"}) {
		t.Errorf("A tags = %v, want [x]", f.Tags)
	}
	if f := bulkFeed(t, db, b); f.Tags != nil {
		t.Errorf("B tags = %v, want none", f.Tags)
	}
	if f := bulkFeed(t, db, c); !slices.Equal(f.Tags, []string{"x", "new", "later"}) {
		t.Errorf("C tags = %v, want untouched", f.Tags)
	}

	d := addBulkFeed(t, db, "D", nil, "x", "y")
	e := addBulkFeed(t, db, "E", nil, "x", "y")
	removed, err := db.RemoveFeedsTag(ctx, []string{d, e}, "x")
	if err != nil || len(removed) != 2 {
		t.Fatalf("remove tag = %v, %v", stateIDs(removed), err)
	}
	if _, err := db.RemoveFeedsTag(ctx, []string{e}, "y"); err != nil {
		t.Fatalf("RemoveFeedsTag: %v", err)
	}
	n, err = db.RevertFeeds(ctx, BulkRemoveTag, "X", removed)
	if err != nil || n != 1 {
		t.Fatalf("undo removetag = %d, %v, want 1", n, err)
	}
	if f := bulkFeed(t, db, d); !slices.Equal(f.Tags, []string{"x", "y"}) {
		t.Errorf("D tags = %v, want [x y]", f.Tags)
	}
	if f := bulkFeed(t, db, e); f.Tags != nil {
		t.Errorf("E tags = %v, want the hand edit kept", f.Tags)
	}

	if _, err := db.RevertFeeds(ctx, BulkOp("nope"), "", removed); err == nil {
		t.Error("an unknown operation was accepted")
	}
}

// A tag revert needs the exact tag the bulk used: a row whose tags only look
// like the bulk's result, because a different tag now sits where it was, stays.
func TestRevertFeedsTagsRequireTheTag(t *testing.T) {
	db := openTestStore(t)
	ctx := t.Context()
	a := addBulkFeed(t, db, "A", nil, "x")

	added, err := db.AddFeedsTag(ctx, []string{a}, "new")
	if err != nil || len(added) != 1 {
		t.Fatalf("add = %v, %v", stateIDs(added), err)
	}
	// The added tag is swapped for another by hand: same length, same prefix.
	if _, err := db.RemoveFeedsTag(ctx, []string{a}, "new"); err != nil {
		t.Fatalf("RemoveFeedsTag: %v", err)
	}
	if _, err := db.AddFeedsTag(ctx, []string{a}, "other"); err != nil {
		t.Fatalf("AddFeedsTag: %v", err)
	}
	n, err := db.RevertFeeds(ctx, BulkAddTag, "new", added)
	if err != nil || n != 0 {
		t.Fatalf("undo addtag = %d, %v, want 0", n, err)
	}
	if f := bulkFeed(t, db, a); !slices.Equal(f.Tags, []string{"x", "other"}) {
		t.Errorf("tags = %v, want [x other] untouched", f.Tags)
	}

	d := addBulkFeed(t, db, "D", nil, "x", "y")
	removed, err := db.RemoveFeedsTag(ctx, []string{d}, "x")
	if err != nil || len(removed) != 1 {
		t.Fatalf("remove tag = %v, %v", stateIDs(removed), err)
	}
	// Reverting with a different tag than the bulk used changes nothing.
	n, err = db.RevertFeeds(ctx, BulkRemoveTag, "y", removed)
	if err != nil || n != 0 {
		t.Fatalf("undo removetag with the wrong tag = %d, %v, want 0", n, err)
	}
	if f := bulkFeed(t, db, d); !slices.Equal(f.Tags, []string{"y"}) {
		t.Errorf("tags = %v, want [y] untouched", f.Tags)
	}

	if _, err := db.RevertFeeds(ctx, BulkAddTag, " ", added); !errors.Is(err, ErrFeedInvalid) {
		t.Errorf("empty tag err = %v, want ErrFeedInvalid", err)
	}
}

func TestDeletedFeedsAt(t *testing.T) {
	db := openTestStore(t)
	live := addBulkFeed(t, db, "Live", nil)
	gone := addBulkFeed(t, db, "Gone", nil)

	feeds, got, err := db.DeletedFeedsAt(t.Context())
	if err != nil || len(got) != 0 || len(feeds) != 0 {
		t.Fatalf("DeletedFeedsAt = %v, %v, %v, want empty", feeds, got, err)
	}
	before := time.Now().Add(-time.Second)
	if err := db.SoftDeleteFeed(t.Context(), gone); err != nil {
		t.Fatalf("SoftDeleteFeed: %v", err)
	}
	feeds, got, err = db.DeletedFeedsAt(t.Context())
	if err != nil {
		t.Fatalf("DeletedFeedsAt: %v", err)
	}
	at, ok := got[gone]
	if _, has := got[live]; has || !ok || len(got) != 1 || len(feeds) != 1 || feeds[0].ID != gone {
		t.Fatalf("DeletedFeedsAt = %v, %v, want only the removed feed", feeds, got)
	}
	if at.Before(before) || at.After(time.Now().Add(time.Second)) {
		t.Errorf("deleted_at = %v, want about now", at)
	}
	if at.Location() != time.UTC {
		t.Errorf("deleted_at zone = %v, want UTC", at.Location())
	}
}

func TestWriteFeedTagsIsCompareAndSet(t *testing.T) {
	db := openTestStore(t)
	ctx := t.Context()
	tagged := addBulkFeed(t, db, "Tagged", nil, "x")
	bare := addBulkFeed(t, db, "Bare", nil)

	write := func(id string, prior, tags []string) bool {
		t.Helper()
		var ok bool
		err := db.inTx(ctx, func(tx *sql.Tx) error {
			var err error
			ok, err = db.writeFeedTags(ctx, tx, id, prior, tags)
			return err
		})
		if err != nil {
			t.Fatalf("writeFeedTags: %v", err)
		}
		return ok
	}

	if write(tagged, []string{"stale"}, []string{"y"}) {
		t.Error("a mismatching prior wrote")
	}
	if f := bulkFeed(t, db, tagged); !slices.Equal(f.Tags, []string{"x"}) {
		t.Errorf("tags = %v, want [x] untouched", f.Tags)
	}
	if write(bare, []string{"x"}, []string{"y"}) {
		t.Error("a prior of x over no tags wrote")
	}
	if !write(bare, nil, []string{"y"}) {
		t.Error("a nil prior over no tags did not write")
	}
	if !write(tagged, []string{"x"}, []string{"x", "y"}) {
		t.Error("a matching prior did not write")
	}
	if f := bulkFeed(t, db, tagged); !slices.Equal(f.Tags, []string{"x", "y"}) {
		t.Errorf("tags = %v, want [x y]", f.Tags)
	}
}

func TestBulkFeedsSpanStateChunks(t *testing.T) {
	db := openTestStore(t)
	ctx := t.Context()
	var ids []string
	for i := range stateChunk*2 + 30 {
		ids = append(ids, addBulkFeed(t, db, fmt.Sprintf("Feed%03d", i), nil))
	}
	ids = append(ids, ids[0], "missing")

	got, err := db.SetFeedsEnabled(ctx, ids, false)
	if err != nil {
		t.Fatalf("SetFeedsEnabled: %v", err)
	}
	if want := ids[:len(ids)-2]; !slices.Equal(stateIDs(got), want) {
		t.Fatalf("changed %d rows in order, want %d (repeat and unknown ids skipped)", len(got), len(want))
	}
	n, err := db.RevertFeeds(ctx, BulkDisable, "", got)
	if err != nil || n != len(got) {
		t.Fatalf("undo = %d, %v, want %d", n, err, len(got))
	}
}

func TestRevertFeedsRemoveLeavesALaterRemoval(t *testing.T) {
	db := openTestStore(t)
	ctx := t.Context()
	a := addBulkFeed(t, db, "A", nil)

	removed, err := db.SoftDeleteFeeds(ctx, []string{a})
	if err != nil || len(removed) != 1 || removed[0].RemovedAt.IsZero() {
		t.Fatalf("remove = %+v, %v, want the written time recorded", removed, err)
	}
	if err := db.RestoreFeed(ctx, a); err != nil {
		t.Fatalf("RestoreFeed: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	if err := db.SoftDeleteFeed(ctx, a); err != nil {
		t.Fatalf("SoftDeleteFeed: %v", err)
	}
	n, err := db.RevertFeeds(ctx, BulkRemove, "", removed)
	if err != nil || n != 0 {
		t.Fatalf("undo = %d, %v, want 0 (removed again since)", n, err)
	}
	if n := len(liveFeeds(t, db)); n != 0 {
		t.Errorf("live = %d, want 0", n)
	}
}
