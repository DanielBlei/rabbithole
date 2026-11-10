// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/DanielBlei/rabbithole/internal/feeds"
	tagspkg "github.com/DanielBlei/rabbithole/internal/tags"
)

// Content tags are model-authored keywords and assumptions. They are kept
// apart from items.tags, which holds the source feed's own tags: one is a
// classification, the other is feed metadata.
const tagsSchema = `
CREATE TABLE IF NOT EXISTS tags (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	slug       TEXT NOT NULL UNIQUE,
	label      TEXT NOT NULL,
	parent_id  INTEGER REFERENCES tags(id),
	created_at TIMESTAMP NOT NULL
);`

const tagsSchemaPG = `
CREATE TABLE IF NOT EXISTS tags (
	id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	slug       TEXT NOT NULL UNIQUE,
	label      TEXT NOT NULL,
	parent_id  BIGINT REFERENCES tags(id),
	created_at TIMESTAMPTZ NOT NULL
);`

const tagAliasesSchema = `
CREATE TABLE IF NOT EXISTS tag_aliases (
	alias      TEXT PRIMARY KEY,
	tag_id     INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
	created_at TIMESTAMP NOT NULL
);`

const tagAliasesSchemaPG = `
CREATE TABLE IF NOT EXISTS tag_aliases (
	alias      TEXT PRIMARY KEY,
	tag_id     BIGINT NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
	created_at TIMESTAMPTZ NOT NULL
);`

const itemTagsSchema = `
CREATE TABLE IF NOT EXISTS item_tags (
	item_id            TEXT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
	tag_id             INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
	kind               TEXT NOT NULL CHECK (kind IN ('keyword', 'assumption')),
	classifier_version INTEGER NOT NULL,
	created_at         TIMESTAMP NOT NULL,
	PRIMARY KEY (item_id, tag_id)
);
CREATE INDEX IF NOT EXISTS idx_item_tags_tag ON item_tags(tag_id, kind, item_id);`

const itemTagsSchemaPG = `
CREATE TABLE IF NOT EXISTS item_tags (
	item_id            TEXT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
	tag_id             BIGINT NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
	kind               TEXT NOT NULL CHECK (kind IN ('keyword', 'assumption')),
	classifier_version INTEGER NOT NULL,
	created_at         TIMESTAMPTZ NOT NULL,
	PRIMARY KEY (item_id, tag_id)
);
CREATE INDEX IF NOT EXISTS idx_item_tags_tag ON item_tags(tag_id, kind, item_id);`

const itemClassificationSchema = `
CREATE TABLE IF NOT EXISTS item_classification (
	item_id            TEXT PRIMARY KEY REFERENCES items(id) ON DELETE CASCADE,
	classifier_version INTEGER NOT NULL,
	status             TEXT NOT NULL CHECK (status IN ('pending', 'ok', 'failed')),
	attempts           INTEGER NOT NULL DEFAULT 0,
	next_attempt_at    TIMESTAMP,
	last_error         TEXT NOT NULL DEFAULT '',
	updated_at         TIMESTAMP NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_item_classification_queue
	ON item_classification(status, classifier_version, next_attempt_at);`

const itemClassificationSchemaPG = `
CREATE TABLE IF NOT EXISTS item_classification (
	item_id            TEXT PRIMARY KEY REFERENCES items(id) ON DELETE CASCADE,
	classifier_version INTEGER NOT NULL,
	status             TEXT NOT NULL CHECK (status IN ('pending', 'ok', 'failed')),
	attempts           INTEGER NOT NULL DEFAULT 0,
	next_attempt_at    TIMESTAMPTZ,
	last_error         TEXT NOT NULL DEFAULT '',
	updated_at         TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_item_classification_queue
	ON item_classification(status, classifier_version, next_attempt_at);`

// Tag kinds. The same tag may be a keyword on one item and an assumption on
// another, so the kind belongs to the edge rather than the tag.
const (
	TagKindKeyword    = "keyword"
	TagKindAssumption = "assumption"
)

// Classification queue states for one item at one classifier version.
const (
	ClassificationPending = "pending"
	ClassificationOK      = "ok"
	ClassificationFailed  = "failed"
)

// Limits from the approved design. The store enforces the label sizes because
// they protect the schema; the classifier enforces the per-list caps.
const (
	maxTagSlugRunes  = 64
	maxTagLabelRunes = 128
)

// ErrInvalidTag is returned for a label or slug that cannot be stored. It wraps
// ErrInvalidFilter's caller-error shape for query inputs but stays distinct.
var ErrInvalidTag = errors.New("invalid content tag")

// ItemTag is one content tag applied to an item.
type ItemTag struct {
	Slug              string
	Label             string
	Kind              string
	ClassifierVersion int
}

// TagInfo is a canonical tag with the aliases that resolve to it.
type TagInfo struct {
	Slug    string
	Label   string
	Aliases []string
}

// TagCount is one row of the reverse vocabulary summary.
type TagCount struct {
	Slug  string
	Label string
	Count int
}

// ClassificationCandidate is one scored item the worker may classify.
type ClassificationCandidate struct {
	Item     feeds.Item
	Attempts int
}

// ClassificationState is the queue row for one item.
type ClassificationState struct {
	ItemID            string
	ClassifierVersion int
	Status            string
	Attempts          int
	NextAttemptAt     *time.Time
	LastError         string
	UpdatedAt         time.Time
}

// TagWriteResult reports how many edges replaced an item's previous tags.
type TagWriteResult struct {
	ItemID      string
	Keywords    int
	Assumptions int
}

// Page bounds a reverse-lookup result. Zero Limit takes the item list default.
type Page struct {
	Limit  int
	Offset int
}

// ReplaceItemTags atomically replaces one item's content tags and records the
// classification as ok at version. Only classifier-owned rows are replaced:
// feed tags, scores and user state are untouched. A missing item is ignored so
// a concurrent prune cannot turn a valid classification into an error.
func (s *Store) ReplaceItemTags(
	ctx context.Context,
	itemID string,
	version int,
	keywords, assumptions []string,
) (TagWriteResult, error) {
	if strings.TrimSpace(itemID) == "" {
		return TagWriteResult{}, fmt.Errorf("item id cannot be blank")
	}
	if version < 1 {
		return TagWriteResult{}, fmt.Errorf("classifier version must be positive, got %d", version)
	}
	kw, err := canonicalLabels(keywords)
	if err != nil {
		return TagWriteResult{}, err
	}
	as, err := canonicalLabels(assumptions)
	if err != nil {
		return TagWriteResult{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TagWriteResult{}, fmt.Errorf("begin tag replacement: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var exists int
	err = s.txQueryRow(ctx, tx, "SELECT 1 FROM items WHERE id = ?", itemID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return TagWriteResult{}, nil
	}
	if err != nil {
		return TagWriteResult{}, fmt.Errorf("check item %s: %w", itemID, err)
	}

	kindByTag := make(map[int64]string)
	edges := make([]struct {
		tagID int64
		kind  string
	}, 0, len(kw)+len(as))
	addEdges := func(labels []canonicalLabel, kind string) error {
		for _, label := range labels {
			tagID, err := s.ensureTagTx(ctx, tx, label)
			if err != nil {
				return err
			}
			if _, seen := kindByTag[tagID]; seen {
				continue
			}
			kindByTag[tagID] = kind
			edges = append(edges, struct {
				tagID int64
				kind  string
			}{tagID: tagID, kind: kind})
		}
		return nil
	}
	if err := addEdges(kw, TagKindKeyword); err != nil {
		return TagWriteResult{}, err
	}
	if err := addEdges(as, TagKindAssumption); err != nil {
		return TagWriteResult{}, err
	}

	if _, err := s.txExec(ctx, tx, "DELETE FROM item_tags WHERE item_id = ?", itemID); err != nil {
		return TagWriteResult{}, fmt.Errorf("clear tags for item %s: %w", itemID, err)
	}
	now := sqlTime(time.Now())
	stmt, err := s.prepareTx(ctx, tx,
		`INSERT INTO item_tags (item_id, tag_id, kind, classifier_version, created_at)
		 VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return TagWriteResult{}, fmt.Errorf("prepare tag insert: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	result := TagWriteResult{ItemID: itemID}
	for _, edge := range edges {
		if _, err := stmt.ExecContext(ctx, itemID, edge.tagID, edge.kind, version, now); err != nil {
			return TagWriteResult{}, fmt.Errorf("insert tag for item %s: %w", itemID, err)
		}
		if edge.kind == TagKindKeyword {
			result.Keywords++
		} else {
			result.Assumptions++
		}
	}
	if err := s.markClassifiedTx(ctx, tx, itemID, version, now); err != nil {
		return TagWriteResult{}, err
	}
	if err := s.commitWrite(ctx, tx); err != nil {
		return TagWriteResult{}, fmt.Errorf("commit tag replacement: %w", err)
	}
	return result, nil
}

// markClassifiedTx moves an item's queue row to ok at version.
func (s *Store) markClassifiedTx(ctx context.Context, tx *sql.Tx, itemID string, version int, now string) error {
	_, err := s.txExec(ctx, tx, `INSERT INTO item_classification
		(item_id, classifier_version, status, attempts, next_attempt_at, last_error, updated_at)
		VALUES (?, ?, 'ok', 0, NULL, '', ?)
		ON CONFLICT (item_id) DO UPDATE SET
			classifier_version = excluded.classifier_version,
			status = 'ok',
			attempts = 0,
			next_attempt_at = NULL,
			last_error = '',
			updated_at = excluded.updated_at`, itemID, version, now)
	if err != nil {
		return fmt.Errorf("record classification for item %s: %w", itemID, err)
	}
	return nil
}

// canonicalLabel is a normalized slug together with its first display form.
type canonicalLabel struct {
	Slug  string
	Label string
}

// canonicalLabels normalizes and deduplicates labels, preserving the first
// spelling seen for display. Invalid labels fail before anything is stored.
func canonicalLabels(labels []string) ([]canonicalLabel, error) {
	out := make([]canonicalLabel, 0, len(labels))
	seen := make(map[string]struct{}, len(labels))
	for _, raw := range labels {
		label := strings.TrimSpace(raw)
		slug, err := tagspkg.Normalize(label)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidTag, err)
		}
		if utf8.RuneCountInString(slug) > maxTagSlugRunes {
			return nil, fmt.Errorf("%w: slug %q exceeds %d characters", ErrInvalidTag, slug, maxTagSlugRunes)
		}
		if utf8.RuneCountInString(label) > maxTagLabelRunes {
			return nil, fmt.Errorf("%w: label %q exceeds %d characters", ErrInvalidTag, label, maxTagLabelRunes)
		}
		if _, ok := seen[slug]; ok {
			continue
		}
		seen[slug] = struct{}{}
		out = append(out, canonicalLabel{Slug: slug, Label: label})
	}
	return out, nil
}

// ensureTagTx resolves an alias first, then a canonical tag, then creates one.
// The upsert makes concurrent creation harmless: the loser's insert is a no-op
// and the following lookup finds the winner's row.
func (s *Store) ensureTagTx(ctx context.Context, tx *sql.Tx, label canonicalLabel) (int64, error) {
	var tagID int64
	err := s.txQueryRow(ctx, tx, "SELECT tag_id FROM tag_aliases WHERE alias = ?", label.Slug).Scan(&tagID)
	if err == nil {
		return tagID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("resolve tag alias %s: %w", label.Slug, err)
	}
	if _, err := s.txExec(ctx, tx,
		`INSERT INTO tags (slug, label, created_at) VALUES (?, ?, ?)
		 ON CONFLICT (slug) DO NOTHING`, label.Slug, label.Label, sqlTime(time.Now())); err != nil {
		return 0, fmt.Errorf("create tag %s: %w", label.Slug, err)
	}
	if err := s.txQueryRow(ctx, tx, "SELECT id FROM tags WHERE slug = ?", label.Slug).Scan(&tagID); err != nil {
		return 0, fmt.Errorf("look up tag %s: %w", label.Slug, err)
	}
	return tagID, nil
}

// AddTagAlias points alias at canonical, creating the canonical tag when it does
// not exist yet. Aliases are curated; nothing infers them.
func (s *Store) AddTagAlias(ctx context.Context, alias, canonical string) error {
	aliasSlug, err := tagspkg.Normalize(strings.TrimSpace(alias))
	if err != nil {
		return fmt.Errorf("%w: alias %q: %v", ErrInvalidTag, alias, err)
	}
	canonicalText := strings.TrimSpace(canonical)
	canonicalSlug, err := tagspkg.Normalize(canonicalText)
	if err != nil {
		return fmt.Errorf("%w: canonical %q: %v", ErrInvalidTag, canonical, err)
	}
	if aliasSlug == canonicalSlug {
		return fmt.Errorf("%w: alias %q already equals its canonical tag", ErrInvalidTag, alias)
	}
	if utf8.RuneCountInString(canonicalText) > maxTagLabelRunes {
		return fmt.Errorf("%w: canonical %q exceeds %d characters",
			ErrInvalidTag, canonicalText, maxTagLabelRunes)
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		tagID, err := s.ensureTagTx(ctx, tx, canonicalLabel{Slug: canonicalSlug, Label: canonicalText})
		if err != nil {
			return err
		}
		if _, err := s.txExec(ctx, tx,
			`INSERT INTO tag_aliases (alias, tag_id, created_at) VALUES (?, ?, ?)
			 ON CONFLICT (alias) DO UPDATE SET tag_id = excluded.tag_id`,
			aliasSlug, tagID, sqlTime(time.Now())); err != nil {
			return fmt.Errorf("write tag alias %s: %w", aliasSlug, err)
		}
		return s.bumpVersion(ctx, tx)
	})
}

// ListTags returns every canonical tag with its aliases, ordered by slug.
func (s *Store) ListTags(ctx context.Context) ([]TagInfo, error) {
	rows, err := s.query(ctx, `SELECT t.slug, t.label, COALESCE(a.alias, '')
		FROM tags t LEFT JOIN tag_aliases a ON a.tag_id = t.id
		ORDER BY t.slug ASC, a.alias ASC`)
	if err != nil {
		return nil, fmt.Errorf("list tags: %w", err)
	}
	defer func() { _ = rows.Close() }()

	bySlug := make(map[string]int)
	var out []TagInfo
	for rows.Next() {
		var slug, label, alias string
		if err := rows.Scan(&slug, &label, &alias); err != nil {
			return nil, fmt.Errorf("scan tag: %w", err)
		}
		i, ok := bySlug[slug]
		if !ok {
			i = len(out)
			bySlug[slug] = i
			out = append(out, TagInfo{Slug: slug, Label: label})
		}
		if alias != "" {
			out[i].Aliases = append(out[i].Aliases, alias)
		}
	}
	return out, rows.Err()
}

// TagsForItem returns the item's content tags, keywords first.
func (s *Store) TagsForItem(ctx context.Context, itemID string, kinds ...string) ([]ItemTag, error) {
	q := `SELECT t.slug, t.label, it.kind, it.classifier_version
		FROM item_tags it JOIN tags t ON t.id = it.tag_id
		WHERE it.item_id = ?`
	args := []any{itemID}
	if len(kinds) > 0 {
		q += " AND it.kind IN (" + placeholders(len(kinds)) + ")"
		for _, kind := range kinds {
			args = append(args, kind)
		}
	}
	q += " ORDER BY CASE it.kind WHEN 'keyword' THEN 0 ELSE 1 END, t.slug ASC"
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query item tags: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ItemTag
	for rows.Next() {
		var t ItemTag
		if err := rows.Scan(&t.Slug, &t.Label, &t.Kind, &t.ClassifierVersion); err != nil {
			return nil, fmt.Errorf("scan item tag: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ItemsForTag returns items carrying the content tag slug, newest first.
func (s *Store) ItemsForTag(ctx context.Context, slug string, kinds []string, page Page) ([]ItemRow, error) {
	if err := tagspkg.Validate(slug); err != nil {
		return nil, fmt.Errorf("%w: tag %q", ErrInvalidFilter, slug)
	}
	limit := page.Limit
	if limit <= 0 {
		limit = defaultListLimit
	} else if limit > maxListLimit {
		limit = maxListLimit
	}
	offset := max(page.Offset, 0)
	sub := `SELECT it.item_id FROM item_tags it JOIN tags t ON t.id = it.tag_id WHERE t.slug = ?`
	args := []any{slug}
	if len(kinds) > 0 {
		sub += " AND it.kind IN (" + placeholders(len(kinds)) + ")"
		for _, kind := range kinds {
			args = append(args, kind)
		}
	}
	q := "SELECT " + itemRowColumns + " FROM items WHERE id IN (" + sub + ")" +
		" ORDER BY " + itemDate + " DESC, id DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query items for tag: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ItemRow
	for rows.Next() {
		r, err := scanItemRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scan item for tag: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TopTags returns the most frequent content tags since the cutoff. A zero
// cutoff defaults to 30 days and limit <= 0 to 50, matching the design.
func (s *Store) TopTags(ctx context.Context, since time.Time, kinds []string, limit int) ([]TagCount, error) {
	if since.IsZero() {
		since = time.Now().Add(-30 * 24 * time.Hour)
	}
	if limit <= 0 {
		limit = 50
	}
	q := `SELECT t.slug, t.label, COUNT(*)
		FROM item_tags it JOIN tags t ON t.id = it.tag_id
		JOIN items i ON i.id = it.item_id
		WHERE COALESCE(i.published_at, i.created_at) >= ?`
	args := []any{sqlTime(since)}
	if len(kinds) > 0 {
		q += " AND it.kind IN (" + placeholders(len(kinds)) + ")"
		for _, kind := range kinds {
			args = append(args, kind)
		}
	}
	q += " GROUP BY t.id, t.slug, t.label ORDER BY COUNT(*) DESC, t.slug ASC LIMIT ?"
	args = append(args, limit)
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query top tags: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []TagCount
	for rows.Next() {
		var t TagCount
		if err := rows.Scan(&t.Slug, &t.Label, &t.Count); err != nil {
			return nil, fmt.Errorf("scan top tag: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// EnqueueClassification makes the given items claimable by the worker at
// version. An item already ok at that version is left alone unless force is
// set, which also clears its attempt count for a manual retry.
func (s *Store) EnqueueClassification(ctx context.Context, itemIDs []string, version int, force bool) (int, error) {
	if version < 1 {
		return 0, fmt.Errorf("classifier version must be positive, got %d", version)
	}
	if len(itemIDs) == 0 {
		return 0, nil
	}
	forceInt := 0
	if force {
		forceInt = 1
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin classification enqueue: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := sqlTime(time.Now())
	enqueued := 0
	for _, itemID := range itemIDs {
		res, err := s.txExec(ctx, tx, `INSERT INTO item_classification
			(item_id, classifier_version, status, attempts, next_attempt_at, last_error, updated_at)
			SELECT id, ?, 'pending', 0, NULL, '', ? FROM items WHERE id = ?
			ON CONFLICT (item_id) DO UPDATE SET
				classifier_version = excluded.classifier_version,
				status = 'pending',
				attempts = 0,
				next_attempt_at = NULL,
				last_error = '',
				updated_at = excluded.updated_at
			WHERE item_classification.classifier_version <> excluded.classifier_version
				OR item_classification.status <> 'ok'
				OR ? = 1`, version, now, itemID, forceInt)
		if err != nil {
			return 0, fmt.Errorf("enqueue classification for item %s: %w", itemID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("classification enqueue rows affected: %w", err)
		}
		enqueued += int(n)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit classification enqueue: %w", err)
	}
	return enqueued, nil
}

// EligibleClassificationItems returns scored items in the window that are not
// yet ok at version, newest first. A zero cutoff means all history.
func (s *Store) EligibleClassificationItems(
	ctx context.Context,
	cutoff time.Time,
	limit, version int,
) ([]feeds.Item, error) {
	if version < 1 {
		return nil, fmt.Errorf("classifier version must be positive, got %d", version)
	}
	q := `SELECT i.id, i.source, i.title, i.link,
			COALESCE(i.summary, ''), i.published_at, COALESCE(i.tags, '')
		FROM items i LEFT JOIN item_classification c ON c.item_id = i.id
		WHERE i.llm_score IS NOT NULL
			AND trim(i.id) <> '' AND trim(i.source) <> '' AND trim(i.title) <> '' AND trim(i.link) <> ''
			AND (c.item_id IS NULL OR c.classifier_version <> ? OR c.status <> 'ok')`
	args := []any{version}
	if !cutoff.IsZero() {
		q += " AND " + itemDate + " >= ?"
		args = append(args, sqlTime(cutoff))
	}
	q += " ORDER BY " + itemDate + " DESC, i.id ASC"
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query eligible classification items: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanFeedItems(rows)
}

// EnqueueBackfill queues scored items in the window that are not yet classified
// at version, and returns how many rows became claimable.
func (s *Store) EnqueueBackfill(ctx context.Context, cutoff time.Time, limit, version int) (int, error) {
	items, err := s.EligibleClassificationItems(ctx, cutoff, limit, version)
	if err != nil {
		return 0, err
	}
	ids := make([]string, len(items))
	for i, item := range items {
		ids[i] = item.ID
	}
	return s.EnqueueClassification(ctx, ids, version, false)
}

// ClaimClassification returns up to limit scored items the worker may process
// now: never classified, classified at an older version, pending, or failed
// with retries still available and its backoff elapsed. Failed items that have
// exhausted maxAttempts wait for a version bump or a manual retry.
func (s *Store) ClaimClassification(
	ctx context.Context,
	version int,
	now time.Time,
	limit, maxAttempts int,
) ([]ClassificationCandidate, error) {
	if version < 1 {
		return nil, fmt.Errorf("classifier version must be positive, got %d", version)
	}
	if limit <= 0 {
		limit = 1
	}
	if maxAttempts <= 0 {
		maxAttempts = 5
	}
	rows, err := s.query(ctx, `SELECT i.id, i.source, i.title, i.link,
			COALESCE(i.summary, ''), i.published_at, COALESCE(i.tags, ''),
			COALESCE(c.attempts, 0)
		FROM items i LEFT JOIN item_classification c ON c.item_id = i.id
		WHERE i.llm_score IS NOT NULL
			AND trim(i.id) <> '' AND trim(i.source) <> '' AND trim(i.title) <> '' AND trim(i.link) <> ''
			AND (
				c.item_id IS NULL
				OR c.classifier_version <> ?
				OR (c.status = 'pending' AND (c.next_attempt_at IS NULL OR c.next_attempt_at <= ?))
				OR (c.status = 'failed' AND c.attempts < ?
					AND (c.next_attempt_at IS NULL OR c.next_attempt_at <= ?))
			)
		ORDER BY `+itemDate+` DESC, i.id ASC
		LIMIT ?`, version, sqlTime(now), maxAttempts, sqlTime(now), limit)
	if err != nil {
		return nil, fmt.Errorf("claim classification items: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ClassificationCandidate
	for rows.Next() {
		var (
			item      feeds.Item
			published *time.Time
			tags      string
			attempts  int
		)
		if err := rows.Scan(&item.ID, &item.Source, &item.Title, &item.Link,
			&item.Summary, &published, &tags, &attempts); err != nil {
			return nil, fmt.Errorf("scan classification candidate: %w", err)
		}
		if published != nil {
			item.Published = *published
		}
		if tags != "" {
			item.Tags = strings.Split(tags, ",")
		}
		out = append(out, ClassificationCandidate{Item: item, Attempts: attempts})
	}
	return out, rows.Err()
}

// MarkClassificationFailed records a failed attempt and schedules its next
// try. A missing item is ignored, as with tag replacement.
func (s *Store) MarkClassificationFailed(
	ctx context.Context,
	itemID string,
	version, attempts int,
	lastError string,
	next time.Time,
) error {
	if version < 1 {
		return fmt.Errorf("classifier version must be positive, got %d", version)
	}
	if attempts < 0 {
		return fmt.Errorf("attempts cannot be negative, got %d", attempts)
	}
	_, err := s.exec(ctx, `INSERT INTO item_classification
		(item_id, classifier_version, status, attempts, next_attempt_at, last_error, updated_at)
		SELECT id, ?, 'failed', ?, ?, ?, ? FROM items WHERE id = ?
		ON CONFLICT (item_id) DO UPDATE SET
			classifier_version = excluded.classifier_version,
			status = 'failed',
			attempts = excluded.attempts,
			next_attempt_at = excluded.next_attempt_at,
			last_error = excluded.last_error,
			updated_at = excluded.updated_at`,
		version, attempts, sqlTimeOrNull(next), lastError, sqlTime(time.Now()), itemID)
	if err != nil {
		return fmt.Errorf("record classification failure for item %s: %w", itemID, err)
	}
	return nil
}

// ClassificationState returns the queue row for item, and false when none exists.
func (s *Store) ClassificationState(ctx context.Context, itemID string) (ClassificationState, bool, error) {
	var (
		state ClassificationState
		next  sql.NullTime
		upd   time.Time
	)
	err := s.queryRow(ctx, `SELECT item_id, classifier_version, status, attempts,
			next_attempt_at, last_error, updated_at
		FROM item_classification WHERE item_id = ?`, itemID).
		Scan(&state.ItemID, &state.ClassifierVersion, &state.Status, &state.Attempts,
			&next, &state.LastError, &upd)
	if errors.Is(err, sql.ErrNoRows) {
		return ClassificationState{}, false, nil
	}
	if err != nil {
		return ClassificationState{}, false, fmt.Errorf("read classification state: %w", err)
	}
	state.NextAttemptAt = utcTimePtr(next)
	state.UpdatedAt = utcTime(upd)
	return state, true, nil
}

// scanFeedItems maps rows shaped like RecentScoredItems onto feeds.Item.
func scanFeedItems(rows *sql.Rows) ([]feeds.Item, error) {
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
			return nil, fmt.Errorf("scan item: %w", err)
		}
		if published != nil {
			item.Published = *published
		}
		if tags != "" {
			item.Tags = strings.Split(tags, ",")
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
