// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const (
	statWeeks      = 8
	statWeek       = 7 * 24 * time.Hour
	statCohortDays = 30
)

// SourceStatsOpts carries the thresholds the store does not own: the score
// tiers and the ratings a thumbs up and a thumbs down write.
type SourceStatsOpts struct{ High, Mid, ThumbUp, ThumbDown int }

// SourceStat is one source's volume and reader activity.
type SourceStat struct {
	// Weeks counts stored items per week, oldest first. Weeks[7] is the newest
	// 7 days ending now.
	Weeks [statWeeks]int

	// The 30-day cohort: items stored in the last 30 days. Tiers split by the
	// llm score; an item with no score is Unscored, so Scored is the other three.
	Scored, High, Mid, Low, Unscored int

	// Reader activity over the same cohort. The store keeps no read timestamp,
	// so this is the current state of those items, not what happened in the window.
	Read, Skipped, Bookmarked, ThumbsUp, ThumbsDown int

	// Last is the newest created_at the source has ever had; zero when it has none.
	Last time.Time
}

// PerWeek is the 8-week average of stored items per week.
func (st SourceStat) PerWeek() float64 {
	sum := 0
	for _, n := range st.Weeks {
		sum += n
	}
	return float64(sum) / statWeeks
}

// SourceStats reports stats per source name, for every source with an item.
// Two statements: the first reads only the last 8 weeks through the created_at
// index; the second finds each source's newest item over all history, so a
// source quiet for longer than the window still reports when it last posted.
func (s *Store) SourceStats(ctx context.Context, now time.Time, o SourceStatsOpts) (map[string]SourceStat, error) {
	q, args := sourceStatsQuery(now, o)

	out := map[string]SourceStat{}
	err := s.inReadTx(ctx, func(tx *sql.Tx) error {
		rows, err := s.txQuery(ctx, tx, q, args...)
		if err != nil {
			return fmt.Errorf("source stats: %w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var (
				name string
				cum  [statWeeks]int
				st   SourceStat
			)
			dst := []any{&name}
			for k := range cum {
				dst = append(dst, &cum[k])
			}
			dst = append(dst, &st.High, &st.Mid, &st.Low, &st.Unscored,
				&st.Read, &st.Skipped, &st.Bookmarked, &st.ThumbsUp, &st.ThumbsDown)
			if err := rows.Scan(dst...); err != nil {
				return fmt.Errorf("scan source stats: %w", err)
			}
			for k := 0; k < statWeeks-1; k++ {
				st.Weeks[k] = cum[k] - cum[k+1]
			}
			st.Weeks[statWeeks-1] = cum[statWeeks-1]
			st.Scored = st.High + st.Mid + st.Low
			out[name] = st
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("source stats: %w", err)
		}

		last, err := s.txQuery(ctx, tx, "SELECT source, MAX(created_at) FROM items GROUP BY source")
		if err != nil {
			return fmt.Errorf("source last item: %w", err)
		}
		defer func() { _ = last.Close() }()
		for last.Next() {
			var name, at string
			if err := last.Scan(&name, &at); err != nil {
				return fmt.Errorf("scan source last item: %w", err)
			}
			t, err := time.Parse(time.RFC3339Nano, at)
			if err != nil {
				return fmt.Errorf("parse last item time %q: %w", at, err)
			}
			st := out[name]
			st.Last = utcTime(t)
			out[name] = st
		}
		return last.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// sourceStatsQuery builds the windowed aggregate: eight cumulative week counts,
// then the 30-day cohort counts, over items from the last 8 weeks only.
func sourceStatsQuery(now time.Time, o SourceStatsOpts) (string, []any) {
	// bounds[k] = now - 7d*(8-k): the start of week k, oldest first.
	var bounds [statWeeks]time.Time
	for k := range bounds {
		bounds[k] = now.Add(-statWeek * time.Duration(statWeeks-k))
	}
	cohort := sqlTime(now.AddDate(0, 0, -statCohortDays))

	var q strings.Builder
	var args []any
	q.WriteString("SELECT source")
	for _, b := range bounds {
		q.WriteString(", COUNT(*) FILTER (WHERE created_at >= ?)")
		args = append(args, sqlTime(b))
	}
	for _, f := range []struct {
		cond string
		arg  []any
	}{
		{"llm_score >= ?", []any{o.High}},
		{"llm_score >= ? AND llm_score < ?", []any{o.Mid, o.High}},
		{"llm_score < ?", []any{o.Mid}},
		{"llm_score IS NULL", nil},
		{"status = 'read'", nil},
		{"status = 'skipped'", nil},
		{"bookmarked = TRUE", nil},
		{"user_score = ?", []any{o.ThumbUp}},
		{"user_score = ?", []any{o.ThumbDown}},
	} {
		q.WriteString(", COUNT(*) FILTER (WHERE created_at >= ? AND " + f.cond + ")")
		args = append(args, cohort)
		args = append(args, f.arg...)
	}
	q.WriteString(" FROM items WHERE created_at >= ? GROUP BY source")
	args = append(args, sqlTime(bounds[0]))
	return q.String(), args
}
