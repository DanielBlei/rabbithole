// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/DanielBlei/rabbithole/internal/feedback"
)

// RecentFeedback returns up to feedback.MaxExamples explicitly positive and
// negative ratings, newest item date first. Publication date is preferred;
// first-seen date is used when publication date is absent. Intermediate and
// missing ratings are excluded. Item ID makes equal dates deterministic.
func (s *Store) RecentFeedback(ctx context.Context) ([]feedback.Example, error) {
	examples := make([]feedback.Example, 0, 2*feedback.MaxExamples)
	for _, score := range []int{10, 0} {
		selected, err := s.recentFeedbackForScore(ctx, score)
		if err != nil {
			return nil, err
		}
		examples = append(examples, selected...)
	}
	return examples, nil
}

func (s *Store) recentFeedbackForScore(ctx context.Context, score int) ([]feedback.Example, error) {
	rows, err := s.query(ctx, `SELECT id, title, source, COALESCE(summary, ''),
		user_score, user_note, published_at, created_at
		FROM items
		WHERE user_score = ?
		ORDER BY `+itemDate+` DESC, id ASC
		LIMIT ?`, score, feedback.MaxExamples)
	if err != nil {
		return nil, fmt.Errorf("query recent feedback with score %d: %w", score, err)
	}
	defer func() { _ = rows.Close() }()

	examples := make([]feedback.Example, 0, feedback.MaxExamples)
	for rows.Next() {
		var (
			example     feedback.Example
			userScore   int
			userNote    sql.NullString
			publishedAt sql.NullTime
		)
		if err := rows.Scan(
			&example.ItemID,
			&example.Title,
			&example.Source,
			&example.Summary,
			&userScore,
			&userNote,
			&publishedAt,
			&example.FirstSeenAt,
		); err != nil {
			return nil, fmt.Errorf("scan recent feedback with score %d: %w", score, err)
		}
		example.UserScore = userScore
		example.PublishedAt = utcTimePtr(publishedAt)
		example.FirstSeenAt = utcTime(example.FirstSeenAt)
		if userNote.Valid {
			note := userNote.String
			example.UserNote = &note
		}
		examples = append(examples, example)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recent feedback with score %d: %w", score, err)
	}
	return examples, nil
}
