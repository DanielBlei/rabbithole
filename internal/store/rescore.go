// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/DanielBlei/rabbithole/internal/feeds"
)

// ScoreReplacement is a completed replacement for an item's model-owned
// scoring fields. User-owned state is deliberately absent.
type ScoreReplacement struct {
	ItemID      string
	Score       int
	Reason      string
	Model       string
	ProfileID   string
	ProfileName string
	ProfileHash string
}

// RecentScoredItems returns already-scored stored items whose publication
// date, or first-seen date when publication is unknown, is on or after cutoff.
// Newest items come first with stable ID as the tie-break.
func (s *Store) RecentScoredItems(ctx context.Context, cutoff time.Time) ([]feeds.Item, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, source, title, link,
		COALESCE(summary, ''), published_at, COALESCE(tags, '')
		FROM items
		WHERE llm_score IS NOT NULL
			AND trim(id) <> '' AND trim(source) <> '' AND trim(title) <> '' AND trim(link) <> ''
			AND `+itemDate+` >= ?
		ORDER BY `+itemDate+` DESC, id ASC`, sqlTime(cutoff))
	if err != nil {
		return nil, fmt.Errorf("query recent scored items: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var items []feeds.Item
	for rows.Next() {
		var (
			item      feeds.Item
			published *time.Time
			tags      string
		)
		if err := rows.Scan(
			&item.ID, &item.Source, &item.Title, &item.Link, &item.Summary, &published, &tags,
		); err != nil {
			return nil, fmt.Errorf("scan recent scored item: %w", err)
		}
		if published != nil {
			item.Published = *published
		}
		if tags != "" {
			item.Tags = strings.Split(tags, ",")
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recent scored items: %w", err)
	}
	return items, nil
}

// ReplaceItemScores atomically writes completed replacement scores. It only
// touches model-owned score/provenance fields and updated_at; ratings, notes,
// status, bookmarks, tags, dates and digest membership remain unchanged.
// Missing rows are ignored so a concurrent prune cannot discard valid
// replacements for the other candidates.
func (s *Store) ReplaceItemScores(
	ctx context.Context,
	replacements []ScoreReplacement,
) (updated int, err error) {
	for _, replacement := range replacements {
		if replacement.ItemID == "" {
			return 0, fmt.Errorf("replacement item id cannot be blank")
		}
		if replacement.Score < minScore || replacement.Score > maxScore {
			return 0, fmt.Errorf(
				"replacement score %d out of range %d-%d",
				replacement.Score, minScore, maxScore,
			)
		}
		if replacement.ProfileID == "" || replacement.ProfileName == "" || replacement.ProfileHash == "" {
			return 0, fmt.Errorf("replacement profile provenance cannot be blank")
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin score replacement: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `UPDATE items SET
		llm_score = ?, llm_score_reason = ?, llm_score_model = ?,
		llm_profile_id = ?, llm_profile_name = ?, llm_profile_hash = ?,
		updated_at = ?
		WHERE id = ?`)
	if err != nil {
		return 0, fmt.Errorf("prepare score replacement: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	now := sqlTime(time.Now())
	for _, replacement := range replacements {
		res, err := stmt.ExecContext(ctx,
			replacement.Score, replacement.Reason, nullIfEmpty(replacement.Model),
			replacement.ProfileID, replacement.ProfileName, replacement.ProfileHash,
			now, replacement.ItemID,
		)
		if err != nil {
			return 0, fmt.Errorf("replace score for item %s: %w", replacement.ItemID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("score replacement rows affected: %w", err)
		}
		updated += int(n)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit score replacements: %w", err)
	}
	return updated, nil
}

func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}
