// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/feeds"
)

func feedPtrBool(b bool) *bool { return &b }
func feedPtrInt(n int) *int    { return &n }
func feedPtrDur(d time.Duration) *config.Duration {
	v := config.Duration(d)
	return &v
}

// feedNamed finds a stored feed for assertions.
func feedNamed(t *testing.T, list []config.Feed, name string) config.Feed {
	t.Helper()
	for _, f := range list {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("no feed named %q in %+v", name, list)
	return config.Feed{}
}

func liveFeeds(t *testing.T, db *Store) []config.Feed {
	t.Helper()
	list, err := db.Feeds(t.Context())
	if err != nil {
		t.Fatalf("Feeds: %v", err)
	}
	return list
}

// A feed round-trips with its unset knobs still unset — NULL means "inherit",
// and collapsing it to a zero would silently pin every feed to the values it
// happened to have when it was stored.
func TestFeedRoundTripsUnsetKnobs(t *testing.T) {
	db := openTestStore(t)
	full := config.Feed{
		Name: "Full", URL: "https://full.test/feed",
		Enabled: feedPtrBool(false), Since: feedPtrDur(48 * time.Hour),
		MaxItems: feedPtrInt(10), Tags: []string{"ai", "news"},
	}
	bare := config.Feed{Name: "Bare", URL: "https://bare.test/feed"}
	for _, f := range []config.Feed{full, bare} {
		if _, err := db.AddFeed(t.Context(), f); err != nil {
			t.Fatalf("AddFeed(%s): %v", f.Name, err)
		}
	}

	got := feedNamed(t, liveFeeds(t, db), "Full")
	if got.Enabled == nil || *got.Enabled {
		t.Errorf("Enabled = %v, want false", got.Enabled)
	}
	if got.Since == nil || got.Since.Short() != "2d" {
		t.Errorf("Since = %v, want 2d", got.Since)
	}
	if got.MaxItems == nil || *got.MaxItems != 10 {
		t.Errorf("MaxItems = %v, want 10", got.MaxItems)
	}
	if strings.Join(got.Tags, ",") != "ai,news" {
		t.Errorf("Tags = %v, want [ai news]", got.Tags)
	}

	empty := feedNamed(t, liveFeeds(t, db), "Bare")
	if empty.Enabled != nil || empty.Since != nil || empty.MaxItems != nil || empty.Tags != nil {
		t.Errorf("unset knobs came back set: %+v", empty)
	}
}

// Name and URL are both unique: one feed per name, one per link.
func TestAddFeedRejectsDuplicates(t *testing.T) {
	db := openTestStore(t)
	if _, err := db.AddFeed(t.Context(), config.Feed{Name: "Alpha", URL: "https://alpha.test/feed"}); err != nil {
		t.Fatalf("AddFeed: %v", err)
	}
	cases := []struct {
		name string
		feed config.Feed
		want error
		why  string
	}{
		{
			name: "same name",
			feed: config.Feed{Name: "Alpha", URL: "https://other.test/feed"},
			want: ErrFeedNameTaken,
			why:  "items are filed under the feed name, so two would merge",
		},
		{
			name: "same url",
			feed: config.Feed{Name: "Other", URL: "https://alpha.test/feed"},
			want: ErrFeedURLTaken,
			why:  "two entries for one link would share a fetch-history row",
		},
		{
			name: "no name",
			feed: config.Feed{URL: "https://other.test/feed"},
			want: ErrFeedInvalid,
			why:  "the page shows this against the field rather than as an error page",
		},
		{
			name: "wrong scheme",
			feed: config.Feed{Name: "Other", URL: "ftp://example.test/feed"},
			want: ErrFeedInvalid,
			why:  "a form field can hold anything",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := db.AddFeed(t.Context(), c.feed)
			if !errors.Is(err, c.want) {
				t.Errorf("err = %v, want %v (%s)", err, c.want, c.why)
			}
		})
	}
}

// Deletion is soft, so the feed keeps its history and can come back.
func TestSoftDeleteAndRestore(t *testing.T) {
	db := openTestStore(t)
	id, err := db.AddFeed(t.Context(), config.Feed{Name: "Alpha", URL: "https://alpha.test/feed"})
	if err != nil {
		t.Fatalf("AddFeed: %v", err)
	}
	if err := db.RecordFeedFetches(t.Context(), []FeedFetch{
		{FeedID: id, Name: "Alpha", Items: 3, At: time.Now()},
	}); err != nil {
		t.Fatalf("RecordFeedFetches: %v", err)
	}
	if err := db.SoftDeleteFeed(t.Context(), id); err != nil {
		t.Fatalf("SoftDeleteFeed: %v", err)
	}

	if len(liveFeeds(t, db)) != 0 {
		t.Error("a deleted feed is still listed as live")
	}
	deleted, err := db.DeletedFeeds(t.Context())
	if err != nil {
		t.Fatalf("DeletedFeeds: %v", err)
	}
	if len(deleted) != 1 {
		t.Fatalf("DeletedFeeds = %+v, want the one just deleted", deleted)
	}
	// History outlives the feed, which is the point of not hard-deleting.
	health, err := db.FeedHealthByID(t.Context(), 5)
	if err != nil {
		t.Fatalf("FeedHealthByID: %v", err)
	}
	if health[id].Items != 3 {
		t.Errorf("history lost on delete: %+v", health[id])
	}

	if err := db.RestoreFeed(t.Context(), id); err != nil {
		t.Fatalf("RestoreFeed: %v", err)
	}
	if len(liveFeeds(t, db)) != 1 {
		t.Error("the restored feed did not come back")
	}
}

// A deleted feed still holds its unique name and URL, so re-adding it has to be
// an undelete rather than a constraint failure — with the values just typed.
func TestAddFeedRestoresADeletedMatch(t *testing.T) {
	db := openTestStore(t)
	const url = "https://alpha.test/feed"
	id, err := db.AddFeed(t.Context(), config.Feed{Name: "Alpha", URL: url})
	if err != nil {
		t.Fatalf("AddFeed: %v", err)
	}
	if err := db.SoftDeleteFeed(t.Context(), id); err != nil {
		t.Fatalf("SoftDeleteFeed: %v", err)
	}

	again, err := db.AddFeed(t.Context(), config.Feed{
		Name: "Alpha Renamed", URL: url, MaxItems: feedPtrInt(4),
	})
	if err != nil {
		t.Fatalf("re-adding a deleted feed: %v", err)
	}
	if again != id {
		t.Errorf("id = %q, want the original %q so history reattaches", again, id)
	}
	live := liveFeeds(t, db)
	if len(live) != 1 {
		t.Fatalf("live = %+v, want exactly the restored feed", live)
	}
	if live[0].Name != "Alpha Renamed" || live[0].MaxItems == nil || *live[0].MaxItems != 4 {
		t.Errorf("restore kept the old values instead of the new ones: %+v", live[0])
	}
}

// Editing a feed's URL keeps its ID, and with it its fetch history.
func TestUpdateFeedKeepsID(t *testing.T) {
	db := openTestStore(t)
	id, err := db.AddFeed(t.Context(), config.Feed{Name: "Alpha", URL: "https://alpha.test/feed"})
	if err != nil {
		t.Fatalf("AddFeed: %v", err)
	}
	if err := db.UpdateFeed(t.Context(), id, config.Feed{
		Name: "Alpha", URL: "https://alpha.test/feed.xml",
	}); err != nil {
		t.Fatalf("UpdateFeed: %v", err)
	}
	live := liveFeeds(t, db)
	if live[0].ID != id {
		t.Errorf("ID = %q, want the frozen %q", live[0].ID, id)
	}
	if live[0].URL != "https://alpha.test/feed.xml" {
		t.Errorf("URL = %q, want the edited one", live[0].URL)
	}
}

func TestUpdateFeedRejectsAnotherFeedsName(t *testing.T) {
	db := openTestStore(t)
	if _, err := db.AddFeed(t.Context(), config.Feed{Name: "Alpha", URL: "https://alpha.test/feed"}); err != nil {
		t.Fatalf("AddFeed: %v", err)
	}
	id, err := db.AddFeed(t.Context(), config.Feed{Name: "Beta", URL: "https://beta.test/feed"})
	if err != nil {
		t.Fatalf("AddFeed: %v", err)
	}
	err = db.UpdateFeed(t.Context(), id, config.Feed{Name: "Alpha", URL: "https://beta.test/feed"})
	if !errors.Is(err, ErrFeedNameTaken) {
		t.Errorf("err = %v, want ErrFeedNameTaken", err)
	}
	// A feed keeping its own name is not a conflict with itself.
	if err := db.UpdateFeed(t.Context(), id, config.Feed{Name: "Beta", URL: "https://beta.test/feed"}); err != nil {
		t.Errorf("a feed could not keep its own name: %v", err)
	}
}

func TestSetFeedEnabledClearsToInherit(t *testing.T) {
	db := openTestStore(t)
	id, err := db.AddFeed(t.Context(), config.Feed{
		Name: "Alpha", URL: "https://alpha.test/feed", Enabled: feedPtrBool(true),
	})
	if err != nil {
		t.Fatalf("AddFeed: %v", err)
	}
	if err := db.SetFeedEnabled(t.Context(), id, nil); err != nil {
		t.Fatalf("SetFeedEnabled: %v", err)
	}
	if got := liveFeeds(t, db)[0].Enabled; got != nil {
		t.Errorf("Enabled = %v, want nil so it inherits again", got)
	}
}

func TestFeedNotFound(t *testing.T) {
	db := openTestStore(t)
	if _, err := db.FeedByID(t.Context(), "nope"); !errors.Is(err, ErrFeedNotFound) {
		t.Errorf("FeedByID: err = %v, want ErrFeedNotFound", err)
	}
	if err := db.SoftDeleteFeed(t.Context(), "nope"); !errors.Is(err, ErrFeedNotFound) {
		t.Errorf("SoftDeleteFeed: err = %v, want ErrFeedNotFound", err)
	}
}

// Seeding runs on every boot, so the thing that matters is what it does the
// second time: nothing at all to feeds the store already knows about, whatever
// state they are in.
func TestSeedFeedsIsAdditiveOnly(t *testing.T) {
	doc := config.FeedsDoc{
		Defaults: config.FeedDefaults{MaxItems: feedPtrInt(25)},
		Feeds: []config.Feed{
			{Name: "Alpha", URL: "https://alpha.test/feed"},
			{Name: "Beta", URL: "https://beta.test/feed"},
			{Name: "Gamma", URL: "https://gamma.test/feed"},
		},
	}
	db := openTestStore(t)
	first, err := db.SeedFeeds(t.Context(), doc)
	if err != nil {
		t.Fatalf("SeedFeeds: %v", err)
	}
	if first.Added != 3 || first.Skipped != 0 {
		t.Fatalf("first seed = %+v, want 3 added", first)
	}

	// Three edits a re-seed must not undo: a park, a retune, and a delete.
	alpha := feedNamed(t, liveFeeds(t, db), "Alpha")
	if err := db.SetFeedEnabled(t.Context(), alpha.ID, feedPtrBool(false)); err != nil {
		t.Fatalf("SetFeedEnabled: %v", err)
	}
	beta := feedNamed(t, liveFeeds(t, db), "Beta")
	if err := db.UpdateFeed(t.Context(), beta.ID, config.Feed{
		Name: "Beta", URL: "https://beta.test/feed", Since: feedPtrDur(3 * time.Hour),
	}); err != nil {
		t.Fatalf("UpdateFeed: %v", err)
	}
	gamma := feedNamed(t, liveFeeds(t, db), "Gamma")
	if err := db.SoftDeleteFeed(t.Context(), gamma.ID); err != nil {
		t.Fatalf("SoftDeleteFeed: %v", err)
	}

	second, err := db.SeedFeeds(t.Context(), doc)
	if err != nil {
		t.Fatalf("second SeedFeeds: %v", err)
	}
	if second.Added != 0 || second.Skipped != 3 {
		t.Fatalf("second seed = %+v, want nothing added", second)
	}

	live := liveFeeds(t, db)
	if len(live) != 2 {
		t.Fatalf("live = %+v, want Alpha and Beta only — Gamma was deleted", live)
	}
	if got := feedNamed(t, live, "Alpha"); got.Enabled == nil || *got.Enabled {
		t.Error("re-seeding un-parked a feed that was switched off")
	}
	if got := feedNamed(t, live, "Beta"); got.Since == nil || got.Since.Short() != "3h" {
		t.Error("re-seeding overwrote a retuned window")
	}
}

// A new entry in the seed file is the supported way to add feeds by hand, so it
// has to land without disturbing anything else.
func TestSeedFeedsAddsOnlyWhatIsNew(t *testing.T) {
	db := openTestStore(t)
	doc := config.FeedsDoc{Feeds: []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}}}
	if _, err := db.SeedFeeds(t.Context(), doc); err != nil {
		t.Fatalf("SeedFeeds: %v", err)
	}

	doc.Feeds = append(doc.Feeds, config.Feed{Name: "Added", URL: "https://added.test/feed"})
	result, err := db.SeedFeeds(t.Context(), doc)
	if err != nil {
		t.Fatalf("SeedFeeds: %v", err)
	}
	if result.Added != 1 || result.Skipped != 1 {
		t.Fatalf("result = %+v, want 1 added and 1 skipped", result)
	}
	if len(liveFeeds(t, db)) != 2 {
		t.Errorf("live = %+v, want both", liveFeeds(t, db))
	}
}

// A broken entry is worth a warning, never a failed boot: the store already
// holds the feed set, so refusing to start over a bad seed file helps nobody.
func TestSeedFeedsSkipsBadEntries(t *testing.T) {
	db := openTestStore(t)
	result, err := db.SeedFeeds(t.Context(), config.FeedsDoc{Feeds: []config.Feed{
		{Name: "Good", URL: "https://good.test/feed"},
		{Name: "No URL"},
		{Name: "Bad URL", URL: "ftp://bad.test/feed"},
		{URL: "https://nameless.test/feed"},
		// Duplicated inside the file: the second is the same feed.
		{Name: "Twin", URL: "https://good.test/feed"},
	}})
	if err != nil {
		t.Fatalf("SeedFeeds: %v", err)
	}
	if result.Added != 1 {
		t.Errorf("Added = %d, want just the good one", result.Added)
	}
	if result.Skipped != 4 {
		t.Errorf("Skipped = %d, want the four bad or duplicate entries", result.Skipped)
	}
	if len(result.Warnings) != 3 {
		t.Errorf("Warnings = %v, want one per malformed entry", result.Warnings)
	}
}

// Defaults seed once. After that they belong to whoever last edited them.
func TestSeedFeedsLeavesEditedDefaultsAlone(t *testing.T) {
	db := openTestStore(t)
	doc := config.FeedsDoc{
		Defaults: config.FeedDefaults{MaxItems: feedPtrInt(25)},
		Feeds:    []config.Feed{{Name: "Alpha", URL: "https://alpha.test/feed"}},
	}
	if _, err := db.SeedFeeds(t.Context(), doc); err != nil {
		t.Fatalf("SeedFeeds: %v", err)
	}
	if err := db.SetFeedDefaults(t.Context(), config.FeedDefaults{MaxItems: feedPtrInt(5)}); err != nil {
		t.Fatalf("SetFeedDefaults: %v", err)
	}
	if _, err := db.SeedFeeds(t.Context(), doc); err != nil {
		t.Fatalf("second SeedFeeds: %v", err)
	}

	got, err := db.FeedDefaults(t.Context())
	if err != nil {
		t.Fatalf("FeedDefaults: %v", err)
	}
	if got.MaxItems == nil || *got.MaxItems != 5 {
		t.Errorf("MaxItems = %v, want the edited 5", got.MaxItems)
	}
}

// FeedsDoc is the export shape, so it has to be a seed file: what comes out
// must reproduce the same set going back in.
func TestFeedsDocRoundTrips(t *testing.T) {
	source := openTestStore(t)
	original := config.FeedsDoc{
		Defaults: config.FeedDefaults{Since: feedPtrDur(120 * time.Hour), Tags: []string{"all"}},
		Feeds: []config.Feed{
			{Name: "Alpha", URL: "https://alpha.test/feed", Since: feedPtrDur(48 * time.Hour), Tags: []string{"ai"}},
			{Name: "Parked", URL: "https://parked.test/feed", Enabled: feedPtrBool(false)},
		},
	}
	if _, err := source.SeedFeeds(t.Context(), original); err != nil {
		t.Fatalf("SeedFeeds: %v", err)
	}
	exported, err := source.FeedsDoc(t.Context())
	if err != nil {
		t.Fatalf("FeedsDoc: %v", err)
	}

	// Feeding the export into a fresh store must land on the same set.
	fresh := openTestStore(t)
	if _, err := fresh.SeedFeeds(t.Context(), exported); err != nil {
		t.Fatalf("re-seeding the export: %v", err)
	}
	reimported, err := fresh.FeedsDoc(t.Context())
	if err != nil {
		t.Fatalf("FeedsDoc: %v", err)
	}

	if len(reimported.Feeds) != len(original.Feeds) {
		t.Fatalf("round trip = %d feeds, want %d", len(reimported.Feeds), len(original.Feeds))
	}
	if got := feedNamed(t, reimported.Feeds, "Alpha"); got.Since == nil || got.Since.Short() != "2d" {
		t.Errorf("Alpha's window did not survive: %v", got.Since)
	}
	if got := feedNamed(t, reimported.Feeds, "Parked"); got.Enabled == nil || *got.Enabled {
		t.Errorf("the parked feed came back enabled: %v", got.Enabled)
	}
	if reimported.Defaults.Since == nil || reimported.Defaults.Since.Short() != "5d" {
		t.Errorf("defaults did not survive: %+v", reimported.Defaults)
	}
}

// items.source stores the feed's name, so a rename has to take its items along.
func TestRenameSource(t *testing.T) {
	db := openTestStore(t)
	if err := db.Record(t.Context(), []feeds.Item{
		{ID: "a", Source: "Old Name", Title: "A", Link: "https://x.test/a"},
		{ID: "b", Source: "Old Name", Title: "B", Link: "https://x.test/b"},
		{ID: "c", Source: "Other", Title: "C", Link: "https://x.test/c"},
	}, nil, time.Now()); err != nil {
		t.Fatalf("Record: %v", err)
	}

	moved, err := db.RenameSource(t.Context(), "Old Name", "New Name")
	if err != nil {
		t.Fatalf("RenameSource: %v", err)
	}
	if moved != 2 {
		t.Errorf("moved = %d, want 2", moved)
	}

	sources, err := db.Sources(t.Context())
	if err != nil {
		t.Fatalf("Sources: %v", err)
	}
	byName := make(map[string]int, len(sources))
	for _, s := range sources {
		byName[s.Source] = s.Count
	}
	if byName["New Name"] != 2 || byName["Other"] != 1 || byName["Old Name"] != 0 {
		t.Errorf("sources = %+v, want the two moved and Other untouched", sources)
	}

	// A no-op rename must not touch anything.
	if moved, err := db.RenameSource(t.Context(), "New Name", "New Name"); err != nil || moved != 0 {
		t.Errorf("no-op rename moved %d rows (err %v)", moved, err)
	}
}
