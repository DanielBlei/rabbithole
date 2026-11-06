// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/DanielBlei/rabbithole/internal/config"
)

// FeedState is the part of a feed a bulk operation can change, as it was before
// the write. Tags are the feed's own tags, not the merged list. RemovedAt is the
// one field not read first: the time SoftDeleteFeeds stamped on the row, which
// its revert matches.
type FeedState struct {
	ID        string
	Enabled   *bool
	Tags      []string
	DeletedAt *time.Time
	RemovedAt time.Time
}

// BulkOp names a bulk operation so RevertFeeds knows which field it touched.
type BulkOp string

const (
	BulkEnable    BulkOp = "enable"
	BulkDisable   BulkOp = "disable"
	BulkRemove    BulkOp = "remove"
	BulkRestore   BulkOp = "restore"
	BulkAddTag    BulkOp = "addtag"
	BulkRemoveTag BulkOp = "removetag"
)

const (
	sqlFeedStates = `SELECT id, enabled, tags, deleted_at FROM feeds WHERE id IN `

	// The tags are written only over the value they were computed from, so a
	// write that raced this one is skipped rather than overwritten.
	sqlSetFeedTags = `UPDATE feeds SET tags = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL AND COALESCE(tags, '') = ?`

	// The reverts match on the value the bulk wrote, so a field changed since
	// by hand is left alone.
	sqlRevertFeedEnabled = `UPDATE feeds SET enabled = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL AND enabled = ?`
	sqlRevertFeedRemove  = `UPDATE feeds SET deleted_at = NULL, updated_at = ? WHERE id = ? AND deleted_at = ?`
	sqlRevertFeedRestore = `UPDATE feeds SET deleted_at = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`

	sqlDeletedFeedsAt = `SELECT ` + feedColumns + `, deleted_at FROM feeds WHERE deleted_at IS NOT NULL ORDER BY lower(name)`
)

// stateChunk is how many ids one state read names.
const stateChunk = 100

// SetFeedsEnabled writes an explicit enabled value on each live feed that does
// not already hold it and returns the rows it changed. A feed that inherits
// (NULL) is written, since the caller decides which ids change their resolved
// value.
func (s *Store) SetFeedsEnabled(ctx context.Context, ids []string, on bool) ([]FeedState, error) {
	return s.bulkFeeds(ctx, ids, func(tx *sql.Tx, st *FeedState, live bool) (bool, error) {
		if !live || (st.Enabled != nil && *st.Enabled == on) {
			return false, nil
		}
		if _, err := s.txExec(ctx, tx, sqlSetFeedEnabled, on, sqlTime(time.Now()), st.ID); err != nil {
			return false, fmt.Errorf("set feed enabled %q: %w", st.ID, err)
		}
		return true, nil
	})
}

// SoftDeleteFeeds removes each live feed and returns the rows it changed.
func (s *Store) SoftDeleteFeeds(ctx context.Context, ids []string) ([]FeedState, error) {
	return s.bulkFeeds(ctx, ids, func(tx *sql.Tx, st *FeedState, live bool) (bool, error) {
		if !live {
			return false, nil
		}
		st.RemovedAt = time.Now().UTC()
		now := sqlTime(st.RemovedAt)
		if _, err := s.txExec(ctx, tx, sqlSoftDeleteFeed, now, now, st.ID); err != nil {
			return false, fmt.Errorf("delete feed %q: %w", st.ID, err)
		}
		return true, nil
	})
}

// RestoreFeeds brings back each removed feed and returns the rows it changed.
func (s *Store) RestoreFeeds(ctx context.Context, ids []string) ([]FeedState, error) {
	return s.bulkFeeds(ctx, ids, func(tx *sql.Tx, st *FeedState, live bool) (bool, error) {
		if live {
			return false, nil
		}
		if _, err := s.txExec(ctx, tx, sqlUndeleteFeed, sqlTime(time.Now()), st.ID); err != nil {
			return false, fmt.Errorf("restore feed %q: %w", st.ID, err)
		}
		return true, nil
	})
}

// AddFeedsTag appends tag to the own tags of each live feed that lacks it
// (case-insensitive) and has room, and returns the rows it changed.
func (s *Store) AddFeedsTag(ctx context.Context, ids []string, tag string) ([]FeedState, error) {
	tag, err := BulkTag(tag)
	if err != nil {
		return nil, err
	}
	return s.bulkFeeds(ctx, ids, func(tx *sql.Tx, st *FeedState, live bool) (bool, error) {
		if !live || len(st.Tags) >= maxTodoTags || hasTag(st.Tags, tag) {
			return false, nil
		}
		return s.writeFeedTags(ctx, tx, st.ID, st.Tags, append(slices.Clone(st.Tags), tag))
	})
}

// RemoveFeedsTag drops tag (case-insensitive) from the own tags of each live
// feed that has it and returns the rows it changed. A feed left with no tags
// stores NULL.
func (s *Store) RemoveFeedsTag(ctx context.Context, ids []string, tag string) ([]FeedState, error) {
	tag, err := BulkTag(tag)
	if err != nil {
		return nil, err
	}
	return s.bulkFeeds(ctx, ids, func(tx *sql.Tx, st *FeedState, live bool) (bool, error) {
		if !live || !hasTag(st.Tags, tag) {
			return false, nil
		}
		kept := slices.DeleteFunc(slices.Clone(st.Tags), func(t string) bool {
			return strings.EqualFold(t, tag)
		})
		return s.writeFeedTags(ctx, tx, st.ID, st.Tags, kept)
	})
}

// RevertFeeds undoes a bulk operation from the states it returned. Only the
// field the operation touched is restored, and only on rows where it still
// holds what the operation wrote:
//
//   - enable: rows still enabled = true get their prior value (NULL included)
//   - disable: rows still enabled = false get their prior value
//   - remove: rows still removed are restored
//   - restore: live rows are removed again with their original deleted_at
//   - addtag: live rows whose own tags are the prior list plus tag get the
//     prior list
//   - removetag: live rows whose own tags are the prior list without tag get
//     the prior list
//
// tag is the one the tag operation was given, matched case-insensitively, and
// is ignored by the other operations. It returns how many rows were reverted.
func (s *Store) RevertFeeds(ctx context.Context, op BulkOp, tag string, states []FeedState) (int, error) {
	if op == BulkAddTag || op == BulkRemoveTag {
		var err error
		if tag, err = BulkTag(tag); err != nil {
			return 0, err
		}
	}
	applied := 0
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		applied = 0
		var cur map[string]FeedState
		if op == BulkAddTag || op == BulkRemoveTag {
			var err error
			ids := make([]string, len(states))
			for i, st := range states {
				ids[i] = st.ID
			}
			if cur, err = s.feedStatesTx(ctx, tx, ids); err != nil {
				return err
			}
		}
		for _, st := range states {
			ok, err := s.revertFeedTx(ctx, tx, op, tag, st, cur[st.ID])
			if err != nil {
				return err
			}
			if ok {
				applied++
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return applied, nil
}

// revertFeedTx reverts one row. cur is the row as it is now, read for the tag
// operations.
func (s *Store) revertFeedTx(ctx context.Context, tx *sql.Tx, op BulkOp, tag string, st, cur FeedState) (bool, error) {
	now := sqlTime(time.Now())
	var (
		res sql.Result
		err error
	)
	switch op {
	case BulkEnable:
		res, err = s.txExec(ctx, tx, sqlRevertFeedEnabled, nullBool(st.Enabled), now, st.ID, true)
	case BulkDisable:
		res, err = s.txExec(ctx, tx, sqlRevertFeedEnabled, nullBool(st.Enabled), now, st.ID, false)
	case BulkRemove:
		if st.RemovedAt.IsZero() {
			return false, nil
		}
		res, err = s.txExec(ctx, tx, sqlRevertFeedRemove, now, st.ID, sqlTime(st.RemovedAt))
	case BulkRestore:
		if st.DeletedAt == nil {
			return false, nil
		}
		res, err = s.txExec(ctx, tx, sqlRevertFeedRestore, sqlTime(*st.DeletedAt), now, st.ID)
	case BulkAddTag, BulkRemoveTag:
		return s.revertFeedTagsTx(ctx, tx, op, tag, st, cur)
	default:
		return false, fmt.Errorf("revert feeds: unknown operation %q", op)
	}
	if err != nil {
		return false, fmt.Errorf("revert feed %q: %w", st.ID, err)
	}
	return rowsChanged(res), nil
}

// revertFeedTagsTx puts the prior tags back when the current ones are exactly
// what the operation left: the prior list with tag appended (add) or without
// tag (remove).
func (s *Store) revertFeedTagsTx(
	ctx context.Context,
	tx *sql.Tx,
	op BulkOp,
	tag string,
	st, cur FeedState,
) (bool, error) {
	if cur.ID == "" || cur.DeletedAt != nil {
		return false, nil
	}
	var matches bool
	if op == BulkAddTag {
		n := len(st.Tags)
		matches = len(cur.Tags) == n+1 && slices.Equal(cur.Tags[:n], st.Tags) && strings.EqualFold(cur.Tags[n], tag)
	} else {
		left := slices.DeleteFunc(slices.Clone(st.Tags), func(t string) bool { return strings.EqualFold(t, tag) })
		matches = len(left) < len(st.Tags) && slices.Equal(cur.Tags, left)
	}
	if !matches {
		return false, nil
	}
	return s.writeFeedTags(ctx, tx, st.ID, cur.Tags, st.Tags)
}

// DeletedFeedsAt returns the soft-deleted feeds with when each was removed, by
// ID, in one read.
func (s *Store) DeletedFeedsAt(ctx context.Context) ([]config.Feed, map[string]time.Time, error) {
	rows, err := s.query(ctx, sqlDeletedFeedsAt)
	if err != nil {
		return nil, nil, fmt.Errorf("query deleted feeds: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var feeds []config.Feed
	at := make(map[string]time.Time)
	for rows.Next() {
		var deleted sql.NullTime
		f, err := scanFeed(deletedScanner{rows, &deleted})
		if err != nil {
			return nil, nil, err
		}
		feeds = append(feeds, f)
		if t := utcTimePtr(deleted); t != nil {
			at[f.ID] = *t
		}
	}
	return feeds, at, rows.Err()
}

// deletedScanner reads the removal time after a feed's own columns.
type deletedScanner struct {
	rows *sql.Rows
	at   *sql.NullTime
}

func (d deletedScanner) Scan(dest ...any) error {
	return d.rows.Scan(append(dest, d.at)...)
}

// bulkFeeds runs apply for each id in one transaction, handing it the row's
// state as read inside that transaction. Missing ids are skipped; apply reports
// whether it changed the row, and only changed rows are returned. Any error
// rolls the whole batch back.
func (s *Store) bulkFeeds(
	ctx context.Context,
	ids []string,
	apply func(tx *sql.Tx, st *FeedState, live bool) (bool, error),
) ([]FeedState, error) {
	ids = uniqueIDs(ids)
	var changed []FeedState
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		changed = nil
		states, err := s.feedStatesTx(ctx, tx, ids)
		if err != nil {
			return err
		}
		for _, id := range ids {
			st, found := states[id]
			if !found {
				continue
			}
			ok, err := apply(tx, &st, st.DeletedAt == nil)
			if err != nil {
				return err
			}
			if ok {
				changed = append(changed, st)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return changed, nil
}

// uniqueIDs drops repeated ids, keeping the first of each in order, so a row is
// never applied twice from one stale read.
func uniqueIDs(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// feedStatesTx reads the bulk-relevant fields of the named feeds, stateChunk ids
// to a query. An unknown id is absent from the result; a removed feed has
// DeletedAt set.
func (s *Store) feedStatesTx(ctx context.Context, tx *sql.Tx, ids []string) (map[string]FeedState, error) {
	out := make(map[string]FeedState, len(ids))
	for chunk := range slices.Chunk(ids, stateChunk) {
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		q := sqlFeedStates + "(?" + strings.Repeat(",?", len(chunk)-1) + ")"
		if err := s.scanFeedStates(ctx, tx, q, args, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) scanFeedStates(ctx context.Context, tx *sql.Tx, q string, args []any, out map[string]FeedState) error {
	rows, err := s.txQuery(ctx, tx, q, args...)
	if err != nil {
		return fmt.Errorf("read feeds: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			id      string
			enabled sql.NullBool
			rawTags sql.NullString
			deleted sql.NullTime
		)
		if err := rows.Scan(&id, &enabled, &rawTags, &deleted); err != nil {
			return fmt.Errorf("scan feeds: %w", err)
		}
		st := FeedState{ID: id, Tags: splitTags(rawTags.String), DeletedAt: utcTimePtr(deleted)}
		if enabled.Valid {
			st.Enabled = &enabled.Bool
		}
		out[id] = st
	}
	return rows.Err()
}

// writeFeedTags sets a feed's own tags to tags if they are still prior. A lost
// race writes nothing and reports false.
func (s *Store) writeFeedTags(ctx context.Context, tx *sql.Tx, id string, prior, tags []string) (bool, error) {
	res, err := s.txExec(ctx, tx, sqlSetFeedTags, nullTags(tags), sqlTime(time.Now()), id, joinTags(prior))
	if err != nil {
		return false, fmt.Errorf("set feed tags %q: %w", id, err)
	}
	return rowsChanged(res), nil
}

// BulkTag normalises the tag the way a stored tag is, rejecting one that
// normalises away. A bulk tag operation stores and matches this spelling.
func BulkTag(tag string) (string, error) {
	norm := normalizeTags([]string{tag})
	if len(norm) == 0 {
		return "", fmt.Errorf("%w: empty tag", ErrFeedInvalid)
	}
	return norm[0], nil
}

func hasTag(tags []string, tag string) bool {
	return slices.ContainsFunc(tags, func(t string) bool { return strings.EqualFold(t, tag) })
}

// rowsChanged treats a driver that cannot report the count as a change, since
// the statement ran.
func rowsChanged(res sql.Result) bool {
	n, err := res.RowsAffected()
	return err != nil || n > 0
}
