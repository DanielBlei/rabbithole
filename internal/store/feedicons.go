// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// feedIconSchema holds one row per feed: the outcome of the last icon lookup
// and, when it found one, the PNG itself. Keyed on feed_id like feed_fetches.
// feed_url records the URL the lookup ran for, since the id stays frozen when a
// feed's URL is edited. site_host is the site key that was requested. Rows are
// never deleted: feeds are soft-deleted, so a restored feed keeps its icon.
const feedIconSchema = `
CREATE TABLE IF NOT EXISTS feed_icons (
	feed_id    TEXT PRIMARY KEY,
	feed_url   TEXT NOT NULL DEFAULT '',
	site_host  TEXT NOT NULL DEFAULT '',
	status     TEXT NOT NULL,
	mime       TEXT NOT NULL DEFAULT '',
	bytes      BLOB,
	dark       BOOLEAN NOT NULL DEFAULT 0,
	etag       TEXT NOT NULL DEFAULT '',
	error      TEXT NOT NULL DEFAULT '',
	fetched_at TIMESTAMP NOT NULL
);
`

const feedIconSchemaPG = `
CREATE TABLE IF NOT EXISTS feed_icons (
	feed_id    TEXT PRIMARY KEY,
	feed_url   TEXT NOT NULL DEFAULT '',
	site_host  TEXT NOT NULL DEFAULT '',
	status     TEXT NOT NULL,
	mime       TEXT NOT NULL DEFAULT '',
	bytes      BYTEA,
	dark       BOOLEAN NOT NULL DEFAULT FALSE,
	etag       TEXT NOT NULL DEFAULT '',
	error      TEXT NOT NULL DEFAULT '',
	fetched_at TIMESTAMPTZ NOT NULL
);
`

// Icon lookup outcomes for feed_icons.status. Only ok rows carry bytes.
const (
	IconStatusOK    = "ok"
	IconStatusNone  = "none"
	IconStatusError = "error"
)

// ErrFeedIconNotFound means the feed has no stored icon.
var ErrFeedIconNotFound = errors.New("feed icon not found")

// FeedIcon is the icon lookup result for one feed. Bytes is nil from
// FeedIconMeta.
type FeedIcon struct {
	FeedID, FeedURL, SiteHost, Status, Mime, ETag, Error string
	Dark                                                 bool
	FetchedAt                                            time.Time
	Bytes                                                []byte
}

const (
	sqlUpsertFeedIcon = `INSERT INTO feed_icons
	(feed_id, feed_url, site_host, status, mime, bytes, dark, etag, error, fetched_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (feed_id) DO UPDATE SET
	feed_url = excluded.feed_url,
	site_host = excluded.site_host,
	status = excluded.status,
	mime = excluded.mime,
	bytes = excluded.bytes,
	dark = excluded.dark,
	etag = excluded.etag,
	error = excluded.error,
	fetched_at = excluded.fetched_at`

	sqlFeedIconMeta = `SELECT feed_id, feed_url, site_host, status, mime, dark, etag, error, fetched_at
FROM feed_icons`

	sqlFeedIconBytes = `SELECT feed_id, feed_url, site_host, status, mime, bytes, dark, etag, error, fetched_at
FROM feed_icons WHERE feed_id = ? AND status = '` + IconStatusOK + `'`
)

// UpsertFeedIcons writes every icon in one transaction, replacing any existing
// row for the same feed. A row that is not ok carries no bytes.
func (s *Store) UpsertFeedIcons(ctx context.Context, icons []FeedIcon) error {
	if len(icons) == 0 {
		return nil
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		for _, ic := range icons {
			var data []byte
			if ic.Status == IconStatusOK {
				data = ic.Bytes
			}
			if _, err := s.txExec(ctx, tx, sqlUpsertFeedIcon,
				ic.FeedID, ic.FeedURL, ic.SiteHost, ic.Status, ic.Mime, data,
				ic.Dark, ic.ETag, ic.Error, sqlTime(ic.FetchedAt)); err != nil {
				return fmt.Errorf("upsert feed icon %s: %w", ic.FeedID, err)
			}
		}
		return nil
	})
}

// FeedIconMeta returns every icon row keyed by feed id, without the bytes.
func (s *Store) FeedIconMeta(ctx context.Context) (map[string]FeedIcon, error) {
	rows, err := s.query(ctx, sqlFeedIconMeta)
	if err != nil {
		return nil, fmt.Errorf("read feed icons: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]FeedIcon{}
	for rows.Next() {
		var (
			ic      FeedIcon
			fetched time.Time
		)
		if err := rows.Scan(&ic.FeedID, &ic.FeedURL, &ic.SiteHost, &ic.Status, &ic.Mime,
			&ic.Dark, &ic.ETag, &ic.Error, &fetched); err != nil {
			return nil, fmt.Errorf("scan feed icon: %w", err)
		}
		ic.FetchedAt = utcTime(fetched)
		out[ic.FeedID] = ic
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read feed icons: %w", err)
	}
	return out, nil
}

// FeedIconBytes returns the stored icon for feedID, or ErrFeedIconNotFound when
// the feed has no row or its lookup found nothing.
func (s *Store) FeedIconBytes(ctx context.Context, feedID string) (FeedIcon, error) {
	var (
		ic      FeedIcon
		fetched time.Time
	)
	err := s.queryRow(ctx, sqlFeedIconBytes, feedID).Scan(&ic.FeedID, &ic.FeedURL, &ic.SiteHost,
		&ic.Status, &ic.Mime, &ic.Bytes, &ic.Dark, &ic.ETag, &ic.Error, &fetched)
	if errors.Is(err, sql.ErrNoRows) {
		return FeedIcon{}, ErrFeedIconNotFound
	}
	if err != nil {
		return FeedIcon{}, fmt.Errorf("read feed icon %s: %w", feedID, err)
	}
	ic.FetchedAt = utcTime(fetched)
	return ic, nil
}
