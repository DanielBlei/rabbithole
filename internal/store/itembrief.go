// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Item brief statuses. A brief is persisted only in a terminal state: a ready
// analysis or a failed attempt with no partial result.
const (
	ItemBriefReady  = "ready"
	ItemBriefFailed = "failed"
)

// ErrItemBriefNotFound is returned when an item has no stored brief.
var ErrItemBriefNotFound = errors.New("item brief not found")

// ItemBrief is one persisted full-text Decision Brief, plus the provenance of
// the analysis that produced it. One row exists per item; a refresh replaces it.
type ItemBrief struct {
	ItemID         string
	Status         string
	ErrorCode      string
	ErrorDetail    string
	Score          *int
	Recommendation string
	Why            string
	KeyPoints      []string
	Novelty        string
	Caveats        string
	Focus          string
	ReadingMinutes int
	WordCount      int
	Truncated      bool
	ContentHash    string
	FinalURL       string
	FeedScore      *int
	ProfileID      string
	ProfileName    string
	ProfileHash    string
	Provider       string
	Model          string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

const itemBriefSchema = `
CREATE TABLE IF NOT EXISTS item_briefs (
	item_id         TEXT PRIMARY KEY,
	status          TEXT NOT NULL,
	error_code      TEXT NOT NULL DEFAULT '',
	error_detail    TEXT NOT NULL DEFAULT '',
	score           INTEGER,
	recommendation  TEXT NOT NULL DEFAULT '',
	why             TEXT NOT NULL DEFAULT '',
	key_points      TEXT NOT NULL DEFAULT '[]',
	novelty         TEXT NOT NULL DEFAULT '',
	caveats         TEXT NOT NULL DEFAULT '',
	focus           TEXT NOT NULL DEFAULT '',
	reading_minutes INTEGER NOT NULL DEFAULT 0,
	word_count      INTEGER NOT NULL DEFAULT 0,
	truncated       BOOLEAN NOT NULL DEFAULT 0,
	content_hash    TEXT NOT NULL DEFAULT '',
	final_url       TEXT NOT NULL DEFAULT '',
	feed_score      INTEGER,
	profile_id      TEXT NOT NULL DEFAULT '',
	profile_name    TEXT NOT NULL DEFAULT '',
	profile_hash    TEXT NOT NULL DEFAULT '',
	provider        TEXT NOT NULL DEFAULT '',
	model           TEXT NOT NULL DEFAULT '',
	created_at      TIMESTAMP NOT NULL,
	updated_at      TIMESTAMP NOT NULL
);
`

const itemBriefSchemaPG = `
CREATE TABLE IF NOT EXISTS item_briefs (
	item_id         TEXT PRIMARY KEY,
	status          TEXT NOT NULL,
	error_code      TEXT NOT NULL DEFAULT '',
	error_detail    TEXT NOT NULL DEFAULT '',
	score           INTEGER,
	recommendation  TEXT NOT NULL DEFAULT '',
	why             TEXT NOT NULL DEFAULT '',
	key_points      TEXT NOT NULL DEFAULT '[]',
	novelty         TEXT NOT NULL DEFAULT '',
	caveats         TEXT NOT NULL DEFAULT '',
	focus           TEXT NOT NULL DEFAULT '',
	reading_minutes INTEGER NOT NULL DEFAULT 0,
	word_count      INTEGER NOT NULL DEFAULT 0,
	truncated       BOOLEAN NOT NULL DEFAULT FALSE,
	content_hash    TEXT NOT NULL DEFAULT '',
	final_url       TEXT NOT NULL DEFAULT '',
	feed_score      INTEGER,
	profile_id      TEXT NOT NULL DEFAULT '',
	profile_name    TEXT NOT NULL DEFAULT '',
	profile_hash    TEXT NOT NULL DEFAULT '',
	provider        TEXT NOT NULL DEFAULT '',
	model           TEXT NOT NULL DEFAULT '',
	created_at      TIMESTAMPTZ NOT NULL,
	updated_at      TIMESTAMPTZ NOT NULL
);
`

const itemBriefColumns = `item_id, status, error_code, error_detail, score,
	recommendation, why, key_points, novelty, caveats, focus, reading_minutes,
	word_count, truncated, content_hash, final_url, feed_score, profile_id,
	profile_name, profile_hash, provider, model, created_at, updated_at`

// SaveItemBrief inserts or replaces the brief for one item in a single
// statement, so a refresh never accumulates rows or exposes a partial one.
func (s *Store) SaveItemBrief(ctx context.Context, b ItemBrief) error {
	if strings.TrimSpace(b.ItemID) == "" {
		return errors.New("item brief item_id is required")
	}
	switch b.Status {
	case ItemBriefReady, ItemBriefFailed:
	default:
		return fmt.Errorf("invalid item brief status %q", b.Status)
	}
	if b.KeyPoints == nil {
		b.KeyPoints = []string{}
	}
	keyPoints, err := json.Marshal(b.KeyPoints)
	if err != nil {
		return fmt.Errorf("encode item brief key points: %w", err)
	}
	now := time.Now().UTC()
	if b.CreatedAt.IsZero() {
		b.CreatedAt = now
	}
	b.UpdatedAt = now
	_, err = s.exec(ctx, `INSERT INTO item_briefs (`+itemBriefColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (item_id) DO UPDATE SET
			status = excluded.status,
			error_code = excluded.error_code,
			error_detail = excluded.error_detail,
			score = excluded.score,
			recommendation = excluded.recommendation,
			why = excluded.why,
			key_points = excluded.key_points,
			novelty = excluded.novelty,
			caveats = excluded.caveats,
			focus = excluded.focus,
			reading_minutes = excluded.reading_minutes,
			word_count = excluded.word_count,
			truncated = excluded.truncated,
			content_hash = excluded.content_hash,
			final_url = excluded.final_url,
			feed_score = excluded.feed_score,
			profile_id = excluded.profile_id,
			profile_name = excluded.profile_name,
			profile_hash = excluded.profile_hash,
			provider = excluded.provider,
			model = excluded.model,
			created_at = excluded.created_at,
			updated_at = excluded.updated_at`,
		b.ItemID, b.Status, b.ErrorCode, b.ErrorDetail, nullableInt(b.Score),
		b.Recommendation, b.Why, string(keyPoints), b.Novelty, b.Caveats, b.Focus,
		b.ReadingMinutes, b.WordCount, b.Truncated, b.ContentHash, b.FinalURL,
		nullableInt(b.FeedScore), b.ProfileID, b.ProfileName, b.ProfileHash,
		b.Provider, b.Model, sqlTime(b.CreatedAt), sqlTime(b.UpdatedAt))
	if err != nil {
		return fmt.Errorf("save item brief %s: %w", b.ItemID, err)
	}
	return nil
}

func nullableInt(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

// ItemBrief returns the stored brief for one item.
func (s *Store) ItemBrief(ctx context.Context, itemID string) (ItemBrief, error) {
	row := s.queryRow(ctx, `SELECT `+itemBriefColumns+` FROM item_briefs WHERE item_id = ?`, itemID)
	b, err := scanItemBrief(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ItemBrief{}, fmt.Errorf("%w: %s", ErrItemBriefNotFound, itemID)
	}
	if err != nil {
		return ItemBrief{}, fmt.Errorf("get item brief %s: %w", itemID, err)
	}
	return b, nil
}

// ItemBriefsByItemIDs loads the briefs for a set of items in one query, so a
// feed page does not issue one query per row.
func (s *Store) ItemBriefsByItemIDs(ctx context.Context, ids []string) (map[string]ItemBrief, error) {
	out := make(map[string]ItemBrief, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	rows, err := s.query(
		ctx,
		`SELECT `+itemBriefColumns+` FROM item_briefs WHERE item_id IN (`+placeholders+`)`,
		args...)
	if err != nil {
		return nil, fmt.Errorf("query item briefs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		b, err := scanItemBrief(rows)
		if err != nil {
			return nil, fmt.Errorf("scan item brief: %w", err)
		}
		out[b.ItemID] = b
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate item briefs: %w", err)
	}
	return out, nil
}

func scanItemBrief(sc rowScanner) (ItemBrief, error) {
	var (
		b         ItemBrief
		score     sql.NullInt64
		feedScore sql.NullInt64
		keyPoints string
		created   time.Time
		updated   time.Time
	)
	if err := sc.Scan(&b.ItemID, &b.Status, &b.ErrorCode, &b.ErrorDetail, &score,
		&b.Recommendation, &b.Why, &keyPoints, &b.Novelty, &b.Caveats, &b.Focus,
		&b.ReadingMinutes, &b.WordCount, &b.Truncated, &b.ContentHash, &b.FinalURL,
		&feedScore, &b.ProfileID, &b.ProfileName, &b.ProfileHash, &b.Provider, &b.Model,
		&created, &updated); err != nil {
		return ItemBrief{}, err
	}
	if score.Valid {
		v := int(score.Int64)
		b.Score = &v
	}
	if feedScore.Valid {
		v := int(feedScore.Int64)
		b.FeedScore = &v
	}
	if keyPoints != "" {
		if err := json.Unmarshal([]byte(keyPoints), &b.KeyPoints); err != nil {
			return ItemBrief{}, fmt.Errorf("decode item brief key points: %w", err)
		}
	}
	if b.KeyPoints == nil {
		b.KeyPoints = []string{}
	}
	b.CreatedAt = utcTime(created)
	b.UpdatedAt = utcTime(updated)
	return b, nil
}
