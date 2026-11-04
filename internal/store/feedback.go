// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/DanielBlei/rabbithole/internal/feedback"
)

const (
	// maxFeedbackExclusionLinks keeps the NOT IN list under SQLite's default
	// bound parameter limit, matching the chunk size used for other link lists.
	maxFeedbackExclusionLinks = linkChunkSize
	// maxFeedbackScan bounds the fallback query used when an operation has more
	// candidates than fit in one NOT IN list. It examines the newest rated rows
	// in a single query, so adaptive feedback can still deactivate only when
	// every row in that window is itself a current candidate.
	maxFeedbackScan = 500
)

// RecentFeedback returns up to feedback.MaxExamples explicitly positive and
// negative ratings, newest item date first. Publication date is preferred;
// first-seen date is used when publication date is absent. Intermediate and
// missing ratings are excluded. Item ID makes equal dates deterministic.
func (s *Store) RecentFeedback(ctx context.Context) ([]feedback.Example, error) {
	return s.RecentFeedbackExcluding(ctx, nil)
}

// RecentFeedbackExcluding is RecentFeedback with excludedLinks removed before
// the per-polarity limit, so an older eligible rating can take a slot occupied
// by an article in the current ingest or rescore. Links identify the rows here
// because link is the store's canonical unique key and survives a feed changing
// an article's derived ID. The SQL exclusion list is bounded; when it is
// truncated, the store rescans the newest maxFeedbackScan rated rows and filters
// candidates in memory, so no candidate is ever returned as its own example.
func (s *Store) RecentFeedbackExcluding(
	ctx context.Context,
	excludedLinks []string,
) ([]feedback.Example, error) {
	all := uniqueLinks(excludedLinks)
	sqlExcluded := all
	if len(sqlExcluded) > maxFeedbackExclusionLinks {
		sqlExcluded = sqlExcluded[:maxFeedbackExclusionLinks]
	}
	examples := make([]feedback.Example, 0, 2*feedback.MaxExamples)
	for _, score := range []int{10, 0} {
		selected, err := s.recentFeedbackForScore(ctx, score, sqlExcluded, all)
		if err != nil {
			return nil, err
		}
		examples = append(examples, selected...)
	}
	return examples, nil
}

func (s *Store) recentFeedbackForScore(
	ctx context.Context,
	score int,
	sqlExcluded []string,
	allExcluded []string,
) ([]feedback.Example, error) {
	examples, err := s.queryFeedback(ctx, score, sqlExcluded, feedback.MaxExamples)
	if err != nil {
		return nil, err
	}
	if len(allExcluded) <= len(sqlExcluded) {
		return examples, nil
	}
	// Candidates beyond the bounded NOT IN list can occupy the query's limit
	// and then be dropped by the caller. Rescan a bounded newest-first window
	// without SQL exclusion so older eligible ratings can backfill the slots.
	usable := filterExcluded(examples, allExcluded)
	if len(usable) == feedback.MaxExamples {
		return usable, nil
	}
	scanned, err := s.queryFeedback(ctx, score, nil, maxFeedbackScan)
	if err != nil {
		return nil, err
	}
	return filterExcluded(scanned, allExcluded), nil
}

func (s *Store) queryFeedback(
	ctx context.Context,
	score int,
	excludedLinks []string,
	limit int,
) ([]feedback.Example, error) {
	query := `SELECT id, title, source, link, COALESCE(summary, ''),
		user_score, user_note, published_at, created_at
		FROM items
		WHERE user_score = ?`
	args := []any{score}
	if len(excludedLinks) > 0 {
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(excludedLinks)), ",")
		query += ` AND link NOT IN (` + placeholders + `)`
		for _, link := range excludedLinks {
			args = append(args, link)
		}
	}
	query += ` ORDER BY ` + itemDate + ` DESC, id ASC LIMIT ?`
	args = append(args, limit)

	rows, err := s.query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query recent feedback with score %d: %w", score, err)
	}
	defer func() { _ = rows.Close() }()

	examples := make([]feedback.Example, 0, limit)
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
			&example.Link,
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

// filterExcluded drops candidates and duplicate IDs from an ordered scan and
// keeps the newest MaxExamples. Ordering comes from queryFeedback.
func filterExcluded(examples []feedback.Example, excludedLinks []string) []feedback.Example {
	excluded := make(map[string]struct{}, len(excludedLinks))
	for _, link := range excludedLinks {
		excluded[link] = struct{}{}
	}
	out := make([]feedback.Example, 0, feedback.MaxExamples)
	seen := make(map[string]struct{}, feedback.MaxExamples)
	for _, example := range examples {
		if _, skip := excluded[example.Link]; skip {
			continue
		}
		if _, duplicate := seen[example.ItemID]; duplicate {
			continue
		}
		seen[example.ItemID] = struct{}{}
		out = append(out, example)
		if len(out) == feedback.MaxExamples {
			break
		}
	}
	return out
}

func uniqueLinks(links []string) []string {
	if len(links) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(links))
	out := make([]string, 0, len(links))
	for _, link := range links {
		if link == "" {
			continue
		}
		if _, ok := seen[link]; ok {
			continue
		}
		seen[link] = struct{}{}
		out = append(out, link)
	}
	return out
}
