// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Briefing statuses. A generation is persisted as generating before the model
// is called, then replaced with exactly one terminal status. Nothing is stored
// partially: a failed generation keeps the error and no body.
const (
	BriefingGenerating = "generating"
	BriefingReady      = "ready"
	BriefingFailed     = "failed"
)

// MaxBriefingItems bounds the number of stored articles one briefing may read.
const MaxBriefingItems = 100

// ErrBriefingNotFound is returned when no briefing exists for a period.
var ErrBriefingNotFound = errors.New("briefing not found")

// Briefing is one persisted period briefing plus the provenance of the run
// that produced it.
type Briefing struct {
	ID                string
	PeriodKind        string
	PeriodStart       time.Time
	PeriodEnd         time.Time
	Status            string
	Error             string
	BodyMD            string
	ProfileID         string
	ProfileName       string
	ProfileHash       string
	Provider          string
	Model             string
	ItemCount         int
	ReferencedItemIDs []string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// BriefingItem is a scored stored article selected for a briefing period.
type BriefingItem struct {
	ID            string
	Source        string
	Title         string
	Link          string
	Summary       string
	Score         int
	Reason        string
	EffectiveDate time.Time
}

const briefingSchema = `
CREATE TABLE IF NOT EXISTS briefings (
	id                  TEXT PRIMARY KEY,
	period_kind         TEXT NOT NULL,
	period_start        TIMESTAMP NOT NULL,
	period_end          TIMESTAMP NOT NULL,
	status              TEXT NOT NULL,
	error               TEXT NOT NULL DEFAULT '',
	body_md             TEXT NOT NULL DEFAULT '',
	profile_id          TEXT NOT NULL DEFAULT '',
	profile_name        TEXT NOT NULL DEFAULT '',
	profile_hash        TEXT NOT NULL DEFAULT '',
	provider            TEXT NOT NULL DEFAULT '',
	model               TEXT NOT NULL DEFAULT '',
	item_count          INTEGER NOT NULL DEFAULT 0,
	referenced_item_ids TEXT NOT NULL DEFAULT '[]',
	created_at          TIMESTAMP NOT NULL,
	updated_at          TIMESTAMP NOT NULL,
	UNIQUE (period_kind, period_start)
);
CREATE INDEX IF NOT EXISTS idx_briefings_period ON briefings(period_kind, period_start DESC);
`

const briefingSchemaPG = `
CREATE TABLE IF NOT EXISTS briefings (
	id                  TEXT PRIMARY KEY,
	period_kind         TEXT NOT NULL,
	period_start        TIMESTAMPTZ NOT NULL,
	period_end          TIMESTAMPTZ NOT NULL,
	status              TEXT NOT NULL,
	error               TEXT NOT NULL DEFAULT '',
	body_md             TEXT NOT NULL DEFAULT '',
	profile_id          TEXT NOT NULL DEFAULT '',
	profile_name        TEXT NOT NULL DEFAULT '',
	profile_hash        TEXT NOT NULL DEFAULT '',
	provider            TEXT NOT NULL DEFAULT '',
	model               TEXT NOT NULL DEFAULT '',
	item_count          INTEGER NOT NULL DEFAULT 0,
	referenced_item_ids TEXT NOT NULL DEFAULT '[]',
	created_at          TIMESTAMPTZ NOT NULL,
	updated_at          TIMESTAMPTZ NOT NULL,
	UNIQUE (period_kind, period_start)
);
CREATE INDEX IF NOT EXISTS idx_briefings_period ON briefings(period_kind, period_start DESC);
`

// SaveBriefing inserts a briefing or replaces the row for the same period. The
// id and created_at of an existing row are kept so regeneration replaces the
// stored briefing rather than accumulating rows.
func (s *Store) SaveBriefing(ctx context.Context, b Briefing) (Briefing, error) {
	if b.PeriodKind != "daily" && b.PeriodKind != "weekly" {
		return Briefing{}, fmt.Errorf("briefing period kind must be daily or weekly, got %q", b.PeriodKind)
	}
	if !b.PeriodStart.Before(b.PeriodEnd) {
		return Briefing{}, errors.New("briefing period start must be before end")
	}
	switch b.Status {
	case BriefingGenerating, BriefingReady, BriefingFailed:
	default:
		return Briefing{}, fmt.Errorf("invalid briefing status %q", b.Status)
	}
	refs := b.ReferencedItemIDs
	if refs == nil {
		refs = []string{}
	}
	raw, err := json.Marshal(refs)
	if err != nil {
		return Briefing{}, fmt.Errorf("encode briefing references: %w", err)
	}
	now := sqlTime(time.Now())
	if b.ID == "" {
		return Briefing{}, errors.New("briefing id is required")
	}
	if b.CreatedAt.IsZero() {
		b.CreatedAt = time.Now()
	}
	if _, err := s.exec(ctx, `INSERT INTO briefings
		(id, period_kind, period_start, period_end, status, error, body_md,
		 profile_id, profile_name, profile_hash, provider, model,
		 item_count, referenced_item_ids, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (period_kind, period_start) DO UPDATE SET
			period_end = excluded.period_end,
			status = excluded.status,
			error = excluded.error,
			body_md = excluded.body_md,
			profile_id = excluded.profile_id,
			profile_name = excluded.profile_name,
			profile_hash = excluded.profile_hash,
			provider = excluded.provider,
			model = excluded.model,
			item_count = excluded.item_count,
			referenced_item_ids = excluded.referenced_item_ids,
			updated_at = excluded.updated_at`,
		b.ID, b.PeriodKind, sqlTime(b.PeriodStart), sqlTime(b.PeriodEnd), b.Status,
		b.Error, b.BodyMD, b.ProfileID, b.ProfileName, b.ProfileHash, b.Provider, b.Model,
		b.ItemCount, string(raw), sqlTime(b.CreatedAt), now); err != nil {
		return Briefing{}, fmt.Errorf("save briefing: %w", err)
	}
	return s.BriefingForPeriod(ctx, b.PeriodKind, b.PeriodStart)
}

// BriefingForPeriod returns the stored briefing for one period.
func (s *Store) BriefingForPeriod(ctx context.Context, kind string, start time.Time) (Briefing, error) {
	var (
		b       Briefing
		refs    string
		startT  time.Time
		endT    time.Time
		created time.Time
		updated time.Time
	)
	err := s.queryRow(ctx, `SELECT id, period_kind, period_start, period_end, status,
		error, body_md, profile_id, profile_name, profile_hash, provider, model,
		item_count, referenced_item_ids, created_at, updated_at
		FROM briefings WHERE period_kind = ? AND period_start = ?`,
		kind, sqlTime(start)).Scan(
		&b.ID, &b.PeriodKind, &startT, &endT, &b.Status, &b.Error, &b.BodyMD,
		&b.ProfileID, &b.ProfileName, &b.ProfileHash, &b.Provider, &b.Model,
		&b.ItemCount, &refs, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Briefing{}, fmt.Errorf("%w: %s %s", ErrBriefingNotFound, kind,
			start.UTC().Format("2006-01-02"))
	}
	if err != nil {
		return Briefing{}, fmt.Errorf("get briefing: %w", err)
	}
	b.PeriodStart = utcTime(startT)
	b.PeriodEnd = utcTime(endT)
	b.CreatedAt = utcTime(created)
	b.UpdatedAt = utcTime(updated)
	if refs != "" {
		if err := json.Unmarshal([]byte(refs), &b.ReferencedItemIDs); err != nil {
			return Briefing{}, fmt.Errorf("decode briefing references: %w", err)
		}
	}
	return b, nil
}

// BriefingItems returns scored articles in the half-open period [start, end),
// ordered by relevance, effective date, then stable item ID. The query is
// bounded in SQL so a briefing never loads the full item table.
func (s *Store) BriefingItems(ctx context.Context, start, end time.Time, limit int) ([]BriefingItem, error) {
	if !start.Before(end) {
		return nil, errors.New("briefing period start must be before end")
	}
	if limit < 1 || limit > MaxBriefingItems {
		return nil, fmt.Errorf("briefing limit must be between 1 and %d", MaxBriefingItems)
	}
	rows, err := s.query(ctx, `SELECT id, source, title, link, COALESCE(summary, ''),
		published_at, created_at, llm_score, COALESCE(llm_score_reason, '')
		FROM items
		WHERE llm_score IS NOT NULL AND `+itemDate+` >= ? AND `+itemDate+` < ?
		ORDER BY llm_score DESC, `+itemDate+` DESC, id ASC
		LIMIT ?`, sqlTime(start), sqlTime(end), limit)
	if err != nil {
		return nil, fmt.Errorf("query briefing items: %w", err)
	}
	defer func() { _ = rows.Close() }()

	items := make([]BriefingItem, 0, limit)
	for rows.Next() {
		var (
			item      BriefingItem
			published sql.NullTime
			firstSeen time.Time
		)
		if err := rows.Scan(&item.ID, &item.Source, &item.Title, &item.Link, &item.Summary,
			&published, &firstSeen, &item.Score, &item.Reason); err != nil {
			return nil, fmt.Errorf("scan briefing item: %w", err)
		}
		if published.Valid {
			item.EffectiveDate = utcTime(published.Time)
		} else {
			item.EffectiveDate = utcTime(firstSeen)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate briefing items: %w", err)
	}
	return items, nil
}
