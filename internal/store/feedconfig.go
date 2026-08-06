// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/DanielBlei/rabbithole/internal/config"
)

// feedConfigSchema holds the configured feeds themselves — what to fetch and
// how — as opposed to feed_fetches, which records what happened when we did.
//
// id is config.FeedID(url), minted when the feed is first stored and frozen
// after: feed_fetches keys on the same value, so history survives a rename and,
// unlike before feeds moved in here, a change of URL too.
//
// The tuning columns are nullable because NULL is the whole point — it means
// "inherit from feed_defaults", mirroring the pointer fields on config.Feed. A
// zero would mean something else entirely (max_items 0 is "uncapped").
//
// Deletion is soft. Two reasons: the feed's fetch history stays attached in
// case it comes back, and the seeder recognises a deleted feed as one it has
// already seen, so removing a feed that came from the seed file doesn't undo
// itself on the next boot.
const feedConfigSchema = `
CREATE TABLE IF NOT EXISTS feeds (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL,
	url        TEXT NOT NULL,
	enabled    BOOLEAN,
	since      TEXT,
	max_items  INTEGER,
	tags       TEXT,
	created_at TIMESTAMP NOT NULL,
	updated_at TIMESTAMP NOT NULL,
	deleted_at TIMESTAMP
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_feeds_name ON feeds(name);
CREATE UNIQUE INDEX IF NOT EXISTS idx_feeds_url ON feeds(url);
CREATE INDEX IF NOT EXISTS idx_feeds_live ON feeds(deleted_at);

CREATE TABLE IF NOT EXISTS feed_defaults (
	id         INTEGER PRIMARY KEY CHECK (id = 1),
	enabled    BOOLEAN,
	since      TEXT,
	max_items  INTEGER,
	tags       TEXT,
	updated_at TIMESTAMP NOT NULL
);
`

// Feed-management errors. Name and URL are both unique, so the two collisions
// are reported apart: the Sources page puts the message on the field that
// caused it.
var (
	ErrFeedNotFound  = errors.New("feed not found")
	ErrFeedNameTaken = errors.New("a feed with that name already exists")
	ErrFeedURLTaken  = errors.New("a feed with that url already exists")
	// ErrFeedInvalid marks a rejection the user can fix by retyping, which the
	// Sources page shows against the form rather than as an error page.
	ErrFeedInvalid = errors.New("invalid feed")
)

const (
	feedColumns = "id, name, url, enabled, since, max_items, tags"

	sqlLiveFeeds    = `SELECT ` + feedColumns + ` FROM feeds WHERE deleted_at IS NULL ORDER BY name COLLATE NOCASE`
	sqlDeletedFeeds = `SELECT ` + feedColumns + ` FROM feeds WHERE deleted_at IS NOT NULL ORDER BY name COLLATE NOCASE`
	sqlFeedByID     = `SELECT ` + feedColumns + ` FROM feeds WHERE id = ?`

	sqlInsertFeed = `INSERT INTO feeds (id, name, url, enabled, since, max_items, tags, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`

	// A restore doubles as an update: re-adding a feed you deleted should land
	// on the values you just typed, not the ones it had when it was parked.
	sqlRestoreFeed = `UPDATE feeds
		SET name = ?, url = ?, enabled = ?, since = ?, max_items = ?, tags = ?, updated_at = ?, deleted_at = NULL
		WHERE id = ?`

	sqlUpdateFeed = `UPDATE feeds
		SET name = ?, url = ?, enabled = ?, since = ?, max_items = ?, tags = ?, updated_at = ?
		WHERE id = ? AND deleted_at IS NULL`

	sqlSetFeedEnabled = `UPDATE feeds SET enabled = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`
	sqlSoftDeleteFeed = `UPDATE feeds SET deleted_at = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`
	sqlUndeleteFeed   = `UPDATE feeds SET deleted_at = NULL, updated_at = ? WHERE id = ?`

	// Name and URL are unique across live *and* deleted rows, so collision
	// checks ignore deleted_at and the caller decides what to do with the hit.
	sqlFeedIDByName = `SELECT id, deleted_at IS NULL FROM feeds WHERE name = ?`
	sqlFeedIDByURL  = `SELECT id, deleted_at IS NULL FROM feeds WHERE url = ?`

	sqlFeedDefaults    = `SELECT enabled, since, max_items, tags FROM feed_defaults WHERE id = 1`
	sqlSetFeedDefaults = `INSERT INTO feed_defaults (id, enabled, since, max_items, tags, updated_at)
		VALUES (1, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			enabled = excluded.enabled, since = excluded.since,
			max_items = excluded.max_items, tags = excluded.tags, updated_at = excluded.updated_at`
	sqlHasFeedDefaults = `SELECT EXISTS(SELECT 1 FROM feed_defaults WHERE id = 1)`

	sqlRenameSource = `UPDATE items SET source = ? WHERE source = ?`
)

// Feeds returns every feed that hasn't been deleted, ordered by name so the
// Sources page and the export agree on order regardless of insertion history.
func (s *Store) Feeds(ctx context.Context) ([]config.Feed, error) {
	return s.queryFeeds(ctx, sqlLiveFeeds)
}

// DeletedFeeds returns the soft-deleted feeds, for the page's "show deleted"
// filter — the only route back to a feed you removed by accident.
func (s *Store) DeletedFeeds(ctx context.Context) ([]config.Feed, error) {
	return s.queryFeeds(ctx, sqlDeletedFeeds)
}

func (s *Store) queryFeeds(ctx context.Context, query string) ([]config.Feed, error) {
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query feeds: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []config.Feed
	for rows.Next() {
		f, err := scanFeed(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// FeedByID returns one feed, deleted or not, so the detail pane can render a
// parked feed the same way it renders a live one.
func (s *Store) FeedByID(ctx context.Context, id string) (config.Feed, error) {
	f, err := scanFeed(s.db.QueryRowContext(ctx, sqlFeedByID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return config.Feed{}, fmt.Errorf("%w: %s", ErrFeedNotFound, id)
	}
	return f, err
}

// FeedDefaults returns the set-wide fallbacks. A store that has never had them
// written returns the zero value, where every knob is unset — which the cascade
// already handles as "fall through to the global config and the built-ins".
func (s *Store) FeedDefaults(ctx context.Context) (config.FeedDefaults, error) {
	var (
		d        config.FeedDefaults
		enabled  sql.NullBool
		since    sql.NullString
		maxItems sql.NullInt64
		tags     sql.NullString
	)
	err := s.db.QueryRowContext(ctx, sqlFeedDefaults).Scan(&enabled, &since, &maxItems, &tags)
	if errors.Is(err, sql.ErrNoRows) {
		return d, nil
	}
	if err != nil {
		return d, fmt.Errorf("query feed defaults: %w", err)
	}
	if enabled.Valid {
		d.Enabled = &enabled.Bool
	}
	if since.Valid {
		parsed, err := parseStoredDuration(since.String)
		if err != nil {
			return d, fmt.Errorf("feed defaults: %w", err)
		}
		d.Since = parsed
	}
	if maxItems.Valid {
		n := int(maxItems.Int64)
		d.MaxItems = &n
	}
	d.Tags = splitTags(tags.String)
	return d, nil
}

// FeedsDoc assembles the live feed set in its declared form — the shape the
// seed file uses and the export renders. Round-tripping through it is what
// makes "copy this YAML into a fresh install" reproduce the same set.
func (s *Store) FeedsDoc(ctx context.Context) (config.FeedsDoc, error) {
	defaults, err := s.FeedDefaults(ctx)
	if err != nil {
		return config.FeedsDoc{}, err
	}
	feeds, err := s.Feeds(ctx)
	if err != nil {
		return config.FeedsDoc{}, err
	}
	return config.FeedsDoc{Defaults: defaults, Feeds: feeds}, nil
}

// AddFeed stores a new feed and returns its ID.
//
// A URL or name matching a soft-deleted feed restores that row rather than
// colliding with it: adding back something you removed is an undelete, and it
// reattaches the feed's fetch history. A match against a live feed is the
// error the unique indexes exist to produce.
func (s *Store) AddFeed(ctx context.Context, f config.Feed) (string, error) {
	f, err := validateFeedInput(f)
	if err != nil {
		return "", err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	existing, err := findFeedConflict(ctx, tx, f.Name, f.URL, "")
	if err != nil {
		return "", err
	}

	now := sqlTime(time.Now())
	id := existing
	if id == "" {
		id = config.FeedID(f.URL)
		// The ID is the URL's digest, so a feed added, hard-purged and added
		// again would collide on the primary key. Nothing hard-deletes today,
		// but leaving it as a constraint error would be an unhelpful surprise.
		if _, err := tx.ExecContext(ctx, sqlInsertFeed, id, f.Name, f.URL,
			nullBool(f.Enabled), nullDuration(f.Since), nullInt(f.MaxItems), nullTags(f.Tags),
			now, now); err != nil {
			return "", fmt.Errorf("insert feed %q: %w", f.Name, err)
		}
	} else if _, err := tx.ExecContext(ctx, sqlRestoreFeed, f.Name, f.URL,
		nullBool(f.Enabled), nullDuration(f.Since), nullInt(f.MaxItems), nullTags(f.Tags),
		now, id); err != nil {
		return "", fmt.Errorf("restore feed %q: %w", f.Name, err)
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
}

// UpdateFeed writes new values for an existing feed. The ID is untouched, so
// editing the URL keeps the feed's identity and its history.
func (s *Store) UpdateFeed(ctx context.Context, id string, f config.Feed) error {
	f, err := validateFeedInput(f)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Any conflict here is with a different feed — a live one is a collision, a
	// deleted one still holds the unique name or URL and has to be reported too
	// rather than being silently resurrected under someone else's edit.
	if _, err := findFeedConflict(ctx, tx, f.Name, f.URL, id); err != nil {
		return err
	}

	res, err := tx.ExecContext(ctx, sqlUpdateFeed, f.Name, f.URL,
		nullBool(f.Enabled), nullDuration(f.Since), nullInt(f.MaxItems), nullTags(f.Tags),
		sqlTime(time.Now()), id)
	if err != nil {
		return fmt.Errorf("update feed %q: %w", id, err)
	}
	if err := expectOneRow(res, id); err != nil {
		return err
	}
	return tx.Commit()
}

// SetFeedEnabled parks or unparks a feed. A nil value clears the column, so the
// feed goes back to inheriting whatever the defaults say.
func (s *Store) SetFeedEnabled(ctx context.Context, id string, on *bool) error {
	res, err := s.db.ExecContext(ctx, sqlSetFeedEnabled, nullBool(on), sqlTime(time.Now()), id)
	if err != nil {
		return fmt.Errorf("set feed enabled %q: %w", id, err)
	}
	return expectOneRow(res, id)
}

// SoftDeleteFeed hides a feed from the ingest cycle and the page while keeping
// its row — see feedConfigSchema for why deletion isn't a DELETE.
func (s *Store) SoftDeleteFeed(ctx context.Context, id string) error {
	now := sqlTime(time.Now())
	res, err := s.db.ExecContext(ctx, sqlSoftDeleteFeed, now, now, id)
	if err != nil {
		return fmt.Errorf("delete feed %q: %w", id, err)
	}
	return expectOneRow(res, id)
}

// RestoreFeed brings a soft-deleted feed back with the values it had.
func (s *Store) RestoreFeed(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, sqlUndeleteFeed, sqlTime(time.Now()), id)
	if err != nil {
		return fmt.Errorf("restore feed %q: %w", id, err)
	}
	return expectOneRow(res, id)
}

// SetFeedDefaults replaces the set-wide fallbacks.
func (s *Store) SetFeedDefaults(ctx context.Context, d config.FeedDefaults) error {
	if err := d.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrFeedInvalid, err)
	}
	if _, err := s.db.ExecContext(ctx, sqlSetFeedDefaults,
		nullBool(d.Enabled), nullDuration(d.Since), nullInt(d.MaxItems), nullTags(d.Tags),
		sqlTime(time.Now())); err != nil {
		return fmt.Errorf("set feed defaults: %w", err)
	}
	return nil
}

// SeedResult reports what a seeding pass did. Warnings name entries the file
// declared but the store wouldn't take; they are worth surfacing but never
// worth failing a boot over.
type SeedResult struct {
	Added    int
	Skipped  int
	Warnings []string
}

// SeedFeeds imports feeds from the seed file that the store has never seen.
//
// It only ever inserts. A feed already present is skipped whatever state it is
// in — including deleted, which is what stops a feed you removed in the UI from
// reappearing on the next boot, and including disabled, so parking a feed
// survives a restart. That makes running this on every boot safe: the file adds
// to the store and never argues with it.
func (s *Store) SeedFeeds(ctx context.Context, doc config.FeedsDoc) (SeedResult, error) {
	var result SeedResult

	known, err := s.knownFeedKeys(ctx)
	if err != nil {
		return result, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := sqlTime(time.Now())
	for i, f := range doc.Feeds {
		f, err := validateFeedInput(f)
		if err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("feed %d: %v", i, err))
			result.Skipped++
			continue
		}
		// Entries duplicated inside the file are caught by the same map, since
		// each insert adds its keys to it.
		if known[feedURLKey(f.URL)] || known[feedNameKey(f.Name)] {
			result.Skipped++
			continue
		}
		id := config.FeedID(f.URL)
		if _, err := tx.ExecContext(ctx, sqlInsertFeed, id, f.Name, f.URL,
			nullBool(f.Enabled), nullDuration(f.Since), nullInt(f.MaxItems), nullTags(f.Tags),
			now, now); err != nil {
			return result, fmt.Errorf("seed feed %q: %w", f.Name, err)
		}
		known[feedURLKey(f.URL)] = true
		known[feedNameKey(f.Name)] = true
		result.Added++
	}

	// Defaults seed once and are never overwritten: after the first boot they
	// belong to whoever last edited them on the Sources page.
	var hasDefaults bool
	if err := tx.QueryRowContext(ctx, sqlHasFeedDefaults).Scan(&hasDefaults); err != nil {
		return result, fmt.Errorf("check feed defaults: %w", err)
	}
	if !hasDefaults {
		d := doc.Defaults
		if _, err := tx.ExecContext(ctx, sqlSetFeedDefaults,
			nullBool(d.Enabled), nullDuration(d.Since), nullInt(d.MaxItems), nullTags(d.Tags),
			now); err != nil {
			return result, fmt.Errorf("seed feed defaults: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	return result, nil
}

// RenameSource re-files recorded items under a feed's new name and reports how
// many moved. items.source stores the feed's name rather than its ID, so
// without this a rename would strand every item the feed has already
// contributed under a label nothing points at any more.
func (s *Store) RenameSource(ctx context.Context, oldName, newName string) (int64, error) {
	if oldName == newName || oldName == "" {
		return 0, nil
	}
	res, err := s.db.ExecContext(ctx, sqlRenameSource, newName, oldName)
	if err != nil {
		return 0, fmt.Errorf("rename source %q: %w", oldName, err)
	}
	moved, err := res.RowsAffected()
	if err != nil {
		return 0, nil // the update landed; the count is a nicety
	}
	return moved, nil
}

// knownFeedKeys is every name and URL the store holds, deleted rows included,
// as the prefixed keys of one map — the seeder's "have I seen this" test.
func (s *Store) knownFeedKeys(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, url FROM feeds`)
	if err != nil {
		return nil, fmt.Errorf("query feed keys: %w", err)
	}
	defer func() { _ = rows.Close() }()

	known := make(map[string]bool)
	for rows.Next() {
		var name, url string
		if err := rows.Scan(&name, &url); err != nil {
			return nil, fmt.Errorf("scan feed keys: %w", err)
		}
		known[feedNameKey(name)] = true
		known[feedURLKey(url)] = true
	}
	return known, rows.Err()
}

func feedNameKey(name string) string { return "name:" + name }
func feedURLKey(url string) string   { return "url:" + url }

func scanFeed(row rowScanner) (config.Feed, error) {
	var (
		f        config.Feed
		enabled  sql.NullBool
		since    sql.NullString
		maxItems sql.NullInt64
		tags     sql.NullString
	)
	if err := row.Scan(&f.ID, &f.Name, &f.URL, &enabled, &since, &maxItems, &tags); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return f, err
		}
		return f, fmt.Errorf("scan feed: %w", err)
	}
	if enabled.Valid {
		f.Enabled = &enabled.Bool
	}
	if since.Valid {
		parsed, err := parseStoredDuration(since.String)
		if err != nil {
			return f, fmt.Errorf("feed %q: %w", f.Name, err)
		}
		f.Since = parsed
	}
	if maxItems.Valid {
		n := int(maxItems.Int64)
		f.MaxItems = &n
	}
	f.Tags = splitTags(tags.String)
	return f, nil
}

// findFeedConflict reports the ID of a feed already holding name or url. A live
// match is an error; a deleted one is returned as the row to restore. exclude
// is the feed being edited, which can of course keep its own name and URL.
func findFeedConflict(ctx context.Context, tx *sql.Tx, name, url, exclude string) (string, error) {
	for _, probe := range []struct {
		query string
		arg   string
		taken error
	}{
		{sqlFeedIDByURL, url, ErrFeedURLTaken},
		{sqlFeedIDByName, name, ErrFeedNameTaken},
	} {
		var (
			id   string
			live bool
		)
		err := tx.QueryRowContext(ctx, probe.query, probe.arg).Scan(&id, &live)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("check feed conflict: %w", err)
		}
		if id == exclude {
			continue
		}
		if live {
			return "", fmt.Errorf("%w: %s", probe.taken, probe.arg)
		}
		// Deleted. When adding, this is the row to bring back; when editing
		// another feed, it still owns the unique value and has to be reported.
		if exclude != "" {
			return "", fmt.Errorf("%w: %s (deleted — restore it instead)", probe.taken, probe.arg)
		}
		return id, nil
	}
	return "", nil
}

// validateFeedInput checks a feed before it reaches the database, so a bad
// value comes back as an explained error rather than a constraint failure.
func validateFeedInput(f config.Feed) (config.Feed, error) {
	f.URL = config.NormalizeFeedURL(f.URL)
	if strings.TrimSpace(f.Name) == "" {
		return f, fmt.Errorf("%w: name is required", ErrFeedInvalid)
	}
	if err := config.ValidateFeedURL(f.URL); err != nil {
		return f, fmt.Errorf("%w: %w", ErrFeedInvalid, err)
	}
	if err := f.Validate(); err != nil {
		return f, fmt.Errorf("%w: %w", ErrFeedInvalid, err)
	}
	return f, nil
}

// expectOneRow turns "the update matched nothing" into ErrFeedNotFound, which
// the handlers map to a 404 rather than a silent success.
func expectOneRow(res sql.Result, id string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return nil // the statement ran; the driver just won't say how many rows
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrFeedNotFound, id)
	}
	return nil
}

// parseStoredDuration reads back a window written by nullDuration.
func parseStoredDuration(s string) (*config.Duration, error) {
	parsed, err := config.ParseDuration(s)
	if err != nil {
		return nil, err
	}
	d := config.Duration(parsed)
	return &d, nil
}

// The null* helpers map config.Feed's "unset" pointers onto SQL NULLs, which is
// how the store spells the same idea.
func nullBool(b *bool) any {
	if b == nil {
		return nil
	}
	return *b
}

func nullInt(n *int) any {
	if n == nil {
		return nil
	}
	return *n
}

// nullDuration stores the window in its short form ("7d"), so the export emits
// what was typed rather than a nanosecond count.
func nullDuration(d *config.Duration) any {
	if d == nil {
		return nil
	}
	return d.Short()
}

// nullTags joins tags the way items.tags stores them, with no tags as NULL
// rather than an empty string.
func nullTags(tags []string) any {
	joined := joinTags(tags)
	if joined == "" {
		return nil
	}
	return joined
}
