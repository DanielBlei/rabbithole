// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// PruneFilter picks the items to delete.
type PruneFilter struct {
	All          bool // every item; cannot be combined with the selectors below
	Source       string
	After        time.Time
	Before       time.Time
	IncludeSaved bool // also delete bookmarked, rated or noted items
}

// PruneResult reports what a prune did, or what it would do.
type PruneResult struct {
	Deleted int       // items deleted, or that would be deleted
	Kept    int       // spared for being bookmarked, rated or noted
	Sample  []ItemRow // newest first; set by PrunePreview only
}

// savedItem matches an item carrying state you wrote yourself, the one thing on
// a row a re-ingest cannot bring back.
const savedItem = "(bookmarked = 1 OR user_score IS NOT NULL OR user_note IS NOT NULL)"

// validate rejects a filter that would empty the feed without saying so, one
// that says so and then narrows anyway, and a window with nothing in it.
func (filter PruneFilter) validate() error {
	narrowed := filter.Source != "" || !filter.After.IsZero() || !filter.Before.IsZero()
	if filter.All && narrowed {
		return fmt.Errorf("%w: prune all cannot be combined with a source or a date window", ErrInvalidFilter)
	}
	if !filter.All && !narrowed {
		return fmt.Errorf("%w: prune needs all, or at least one of source, after and before", ErrInvalidFilter)
	}
	if !filter.After.IsZero() && !filter.Before.IsZero() && !filter.Before.After(filter.After) {
		return fmt.Errorf("%w: prune window is empty: before %s is not after %s",
			ErrInvalidFilter, filter.Before.Format(time.RFC3339), filter.After.Format(time.RFC3339))
	}
	return nil
}

// selection is every item the caller picked, saved ones included. The leading
// tautology lets All add no bounds and still produce valid SQL.
func (filter PruneFilter) selection() (string, []any) {
	where, args := []string{"1 = 1"}, []any(nil)
	if filter.Source != "" {
		where = append(where, "source = ?")
		args = append(args, filter.Source)
	}
	if !filter.After.IsZero() {
		where = append(where, itemDate+" >= ?")
		args = append(args, sqlTime(filter.After))
	}
	if !filter.Before.IsZero() {
		where = append(where, itemDate+" < ?")
		args = append(args, sqlTime(filter.Before))
	}
	return strings.Join(where, " AND "), args
}

// whereClause narrows the selection to what actually gets deleted.
func (filter PruneFilter) whereClause() (string, []any) {
	where, args := filter.selection()
	if !filter.IncludeSaved {
		where += " AND NOT " + savedItem
	}
	return where, args
}

// counts splits the selection into what goes and what stays, in one pass, so the
// two numbers always add up.
func (s *Store) counts(ctx context.Context, filter PruneFilter) (deleted, kept int, err error) {
	selection, args := filter.selection()
	var total, saved int
	q := "SELECT COUNT(*), COUNT(*) FILTER (WHERE " + savedItem + ") FROM items WHERE " + selection
	if err := s.db.QueryRowContext(ctx, q, args...).Scan(&total, &saved); err != nil {
		return 0, 0, fmt.Errorf("count prunable items: %w", err)
	}
	if filter.IncludeSaved {
		return total, 0, nil
	}
	return total - saved, saved, nil
}

// PrunePreview reports what PruneItems would delete, plus up to sample of those
// items newest first — the end of the window a mistaken bound reaches first.
func (s *Store) PrunePreview(ctx context.Context, filter PruneFilter, sample int) (PruneResult, error) {
	if err := filter.validate(); err != nil {
		return PruneResult{}, err
	}
	deleted, kept, err := s.counts(ctx, filter)
	if err != nil {
		return PruneResult{}, err
	}
	result := PruneResult{Deleted: deleted, Kept: kept}
	if sample <= 0 || deleted == 0 {
		return result, nil
	}

	where, args := filter.whereClause()
	q := "SELECT " + itemRowColumns + " FROM items WHERE " + where +
		" ORDER BY " + itemDate + " DESC, id DESC LIMIT ?"
	rows, err := s.db.QueryContext(ctx, q, append(args, sample)...)
	if err != nil {
		return PruneResult{}, fmt.Errorf("query prune sample: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		r, err := scanItemRow(rows)
		if err != nil {
			return PruneResult{}, fmt.Errorf("scan prune sample: %w", err)
		}
		result.Sample = append(result.Sample, r)
	}
	return result, rows.Err()
}

// PruneItems deletes the items matching filter and reports how many went and how
// many were spared.
//
// A deleted link its feed still lists comes back on the next ingest run and is
// scored again from scratch, since dedup only sees rows that are still there.
func (s *Store) PruneItems(ctx context.Context, filter PruneFilter) (PruneResult, error) {
	if err := filter.validate(); err != nil {
		return PruneResult{}, err
	}
	// Counted before the delete, while the rows are still there to count.
	_, kept, err := s.counts(ctx, filter)
	if err != nil {
		return PruneResult{}, err
	}

	where, args := filter.whereClause()
	res, err := s.db.ExecContext(ctx, "DELETE FROM items WHERE "+where, args...)
	if err != nil {
		return PruneResult{}, fmt.Errorf("prune items: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return PruneResult{}, fmt.Errorf("prune items: %w", err)
	}
	return PruneResult{Deleted: int(n), Kept: kept}, nil
}
