// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/DanielBlei/rabbithole/internal/feeds"
)

// MaxDigestItems bounds the number of stored articles a digest query can return.
const MaxDigestItems = 50

// DigestItem is a scored stored article with the date used to select it. The
// effective date is published_at when present, otherwise the row's created_at.
type DigestItem struct {
	Item          feeds.Item
	Score         int
	Reason        string
	EffectiveDate time.Time
}

// DigestItems returns scored articles in the half-open period [start, end),
// ordered by relevance, effective date, then stable item ID. The query is
// bounded in SQL so it never loads the full item table into memory.
func (s *Store) DigestItems(ctx context.Context, start, end time.Time, limit int) ([]DigestItem, error) {
	if !start.Before(end) {
		return nil, errors.New("digest period start must be before end")
	}
	if limit < 1 || limit > MaxDigestItems {
		return nil, fmt.Errorf("digest limit must be between 1 and %d", MaxDigestItems)
	}
	rows, err := s.query(ctx, `SELECT id, source, title, link,
		published_at, created_at,
		llm_score, COALESCE(llm_score_reason, '')
		FROM items
		WHERE llm_score IS NOT NULL AND `+itemDate+` >= ? AND `+itemDate+` < ?
		ORDER BY llm_score DESC, `+itemDate+` DESC, id ASC
		LIMIT ?`, sqlTime(start), sqlTime(end), limit)
	if err != nil {
		return nil, fmt.Errorf("query digest items: %w", err)
	}
	defer func() { _ = rows.Close() }()

	items := make([]DigestItem, 0, limit)
	for rows.Next() {
		var (
			item        DigestItem
			published   sql.NullTime
			firstSeen   time.Time
			publishedAt *time.Time
		)
		if err := rows.Scan(&item.Item.ID, &item.Item.Source, &item.Item.Title, &item.Item.Link,
			&published, &firstSeen, &item.Score, &item.Reason); err != nil {
			return nil, fmt.Errorf("scan digest item: %w", err)
		}
		if published.Valid {
			publishedAt = utcTimePtr(published)
			item.Item.Published = *publishedAt
			item.EffectiveDate = *publishedAt
		} else {
			item.EffectiveDate = utcTime(firstSeen)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate digest items: %w", err)
	}
	return items, nil
}
