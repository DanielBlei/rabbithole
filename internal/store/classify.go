// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
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
	updated_at         TIMESTAMP NOT NULL,
	claim_token        TEXT NOT NULL DEFAULT '',
	claim_expires_at   TIMESTAMP
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
	updated_at         TIMESTAMPTZ NOT NULL,
	claim_token        TEXT NOT NULL DEFAULT '',
	claim_expires_at   TIMESTAMPTZ
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

// ErrStaleClaim is returned when a completion or failure write carries a claim
// token the queue no longer honours: another worker re-claimed the item, a
// version change or manual enqueue reset it, or the item finished under a
// different claim. The caller dropped a race it should not fight — the safe
// move is to discard the verdict, not to overwrite the newer state.
var ErrStaleClaim = errors.New("classification claim is no longer current")

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

// ClassificationCandidate is one scored item the worker may classify, together
// with the claim token that proves this worker owns it until it completes.
type ClassificationCandidate struct {
	Item       feeds.Item
	Attempts   int
	ClaimToken string
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
	ClaimToken        string
	ClaimExpiresAt    *time.Time
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
//
// claimToken is the ownership token ClaimClassification minted for this item.
// A non-empty token must still be the current one when the write lands: the
// tags and the ok state change in the same transaction, and a worker whose
// claim was taken over sees ErrStaleClaim with nothing written. An empty token
// is a manual write and skips the ownership check.
func (s *Store) ReplaceItemTags(
	ctx context.Context,
	itemID string,
	version int,
	claimToken string,
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
	if claimToken != "" {
		// Fail fast, before any tag is written, when the claim is no longer
		// ours. markClassifiedTx re-checks under the row lock at commit time:
		// between the two checks a concurrent re-claim can only shrink the
		// window, not slip through it.
		var one int
		err := s.txQueryRow(ctx, tx,
			"SELECT 1 FROM item_classification WHERE item_id = ? AND claim_token = ?",
			itemID, claimToken).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return TagWriteResult{}, fmt.Errorf("%w: item %s", ErrStaleClaim, itemID)
		}
		if err != nil {
			return TagWriteResult{}, fmt.Errorf("check claim for item %s: %w", itemID, err)
		}
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
	if err := s.markClassifiedTx(ctx, tx, itemID, version, now, claimToken); err != nil {
		return TagWriteResult{}, err
	}
	if err := s.commitWrite(ctx, tx); err != nil {
		return TagWriteResult{}, fmt.Errorf("commit tag replacement: %w", err)
	}
	return result, nil
}

// markClassifiedTx moves an item's queue row to ok at version, releasing the
// claim: a completed row holds no ownership. With a claimToken the update is
// guarded by it, so a worker whose claim was taken over writes nothing and
// learns so through ErrStaleClaim.
func (s *Store) markClassifiedTx(
	ctx context.Context,
	tx *sql.Tx,
	itemID string,
	version int,
	now, claimToken string,
) error {
	upsert := `INSERT INTO item_classification
		(item_id, classifier_version, status, attempts, next_attempt_at, last_error, updated_at,
			claim_token, claim_expires_at)
		VALUES (?, ?, 'ok', 0, NULL, '', ?, '', NULL)
		ON CONFLICT (item_id) DO UPDATE SET
			classifier_version = excluded.classifier_version,
			status = 'ok',
			attempts = 0,
			next_attempt_at = NULL,
			last_error = '',
			updated_at = excluded.updated_at,
			claim_token = '',
			claim_expires_at = NULL`
	args := []any{itemID, version, now}
	if claimToken != "" {
		upsert += `
			WHERE item_classification.claim_token = ?`
		args = append(args, claimToken)
	}
	res, err := s.txExec(ctx, tx, upsert, args...)
	if err != nil {
		return fmt.Errorf("record classification for item %s: %w", itemID, err)
	}
	if claimToken == "" {
		return nil
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("record classification for item %s: %w", itemID, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: item %s was re-claimed or reset mid-write", ErrStaleClaim, itemID)
	}
	return nil
}

// canonicalLabel is a normalized slug together with its first display form.
type canonicalLabel struct {
	Slug  string
	Label string
}

// canonicalLabels normalizes and deduplicates labels, preserving the first
// spelling seen for display. Labels are sanitized before anything is stored,
// and invalid labels fail the write.
func canonicalLabels(labels []string) ([]canonicalLabel, error) {
	out := make([]canonicalLabel, 0, len(labels))
	seen := make(map[string]struct{}, len(labels))
	for _, raw := range labels {
		label := sanitizeLabel(strings.TrimSpace(raw))
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

// sanitizeLabel strips the characters a model-authored label must never carry
// into a terminal: C0 and C1 controls — including the ESC that opens ANSI and
// OSC escape sequences — DEL, and Unicode format characters such as the bidi
// overrides. The zero-width joiners legitimate text uses are content, not
// formatting, and survive; ordinary UTF-8, including multilingual labels,
// passes through untouched. This is the store's boundary check: everything
// that persists a tag label goes through it, so no display path can replay a
// control sequence a model emitted.
func sanitizeLabel(s string) string {
	clean := true
	for _, r := range s {
		if isControlRune(r) {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if !isControlRune(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// isControlRune reports whether r is stripped by sanitizeLabel.
func isControlRune(r rune) bool {
	switch {
	case r == '‌' || r == '‍': // zero-width non-joiner / joiner
		return false
	case r < 0x20 || r == 0x7F: // C0 controls and DEL
		return true
	case r >= 0x80 && r <= 0x9F: // C1 controls
		return true
	case unicode.Is(unicode.Cf, r): // format characters (bidi overrides, tags)
		return true
	}
	return false
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
	canonicalText := sanitizeLabel(strings.TrimSpace(canonical))
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
				claim_token = '', claim_expires_at = NULL,
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

// classificationItemFilter returns the predicate a claimable item satisfies,
// prefixed with the alias the caller joined items under.
func classificationItemFilter(alias string) string {
	return alias + ".llm_score IS NOT NULL" +
		" AND trim(" + alias + ".id) <> ''" +
		" AND trim(" + alias + ".source) <> ''" +
		" AND trim(" + alias + ".title) <> ''" +
		" AND trim(" + alias + ".link) <> ''"
}

// classificationQueueFilter returns the predicate that makes a queue row
// claimable at version: never seen at this version (a version bump invalidates
// any previous ownership), or pending or failed with retries left, its backoff
// elapsed and no unexpired claim holding it. iAlias names the items alias the
// caller joined under.
func classificationQueueFilter(cAlias string) string {
	return "(" + cAlias + ".classifier_version <> ?" +
		" OR ((" + cAlias + ".status = 'pending'" +
		" OR (" + cAlias + ".status = 'failed' AND " + cAlias + ".attempts < ?))" +
		" AND (" + cAlias + ".claim_expires_at IS NULL OR " + cAlias + ".claim_expires_at <= ?)" +
		" AND (" + cAlias + ".next_attempt_at IS NULL OR " + cAlias + ".next_attempt_at <= ?)))"
}

// newClaimToken mints a fresh ownership token. Randomness, not sequence: two
// workers claiming around the same crash must never mint the same token, or
// the loser's writes would look like the winner's.
func newClaimToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// ClaimClassification atomically claims up to limit scored items the worker
// may process now: never classified, classified at an older version, pending,
// or failed with retries still available and its backoff elapsed. Failed items
// that have exhausted maxAttempts wait for a version bump or a manual retry.
//
// A claim installs a fresh, unguessable ownership token and a lease of lease
// from now. A second worker — in this or another process — cannot claim the
// same rows while the lease holds; after it expires the row is reclaimable,
// which is how a crashed worker's items recover without operator action. The
// token, not the lease, authorizes completion: a slow generation whose lease
// expired still commits when nobody re-claimed the item, and a worker that
// lost a re-claim sees ErrStaleClaim from its writes instead of overwriting
// the newer verdict. Attempts restart at zero when the stored version differs
// from the requested one, so a version bump restores the full retry budget.
func (s *Store) ClaimClassification(
	ctx context.Context,
	version int,
	now time.Time,
	limit, maxAttempts int,
	lease time.Duration,
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
	if lease <= 0 {
		return nil, fmt.Errorf("classification claim lease must be positive, got %s", lease)
	}
	token, err := newClaimToken()
	if err != nil {
		return nil, fmt.Errorf("mint classification claim: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin classification claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	nowStr := sqlTime(now)
	// 1. Queue rows for items never classified, bounded by limit, so step 2
	//    can take them. Ordering matches the claim order: newest first. The
	//    ON CONFLICT is not decoration: on Postgres two claims can both see a
	//    missing row before either commits (read committed), and the loser's
	//    insert must be a no-op, not a unique-violation error.
	if _, err := s.txExec(ctx, tx, `INSERT INTO item_classification
			(item_id, classifier_version, status, attempts, next_attempt_at, last_error, updated_at)
		SELECT i.id, ?, 'pending', 0, NULL, '', ?
		FROM items i LEFT JOIN item_classification c ON c.item_id = i.id
		WHERE c.item_id IS NULL AND `+classificationItemFilter("i")+`
		ORDER BY `+itemDate+` DESC, i.id ASC
		LIMIT ?
		ON CONFLICT (item_id) DO NOTHING`, version, nowStr, limit); err != nil {
		return nil, fmt.Errorf("queue unclassified items: %w", err)
	}

	// 2. Pick the candidates, newest first.
	rows, err := s.txQuery(ctx, tx, `SELECT c.item_id
		FROM item_classification c JOIN items i ON i.id = c.item_id
		WHERE `+classificationItemFilter("i")+`
			AND `+classificationQueueFilter("c")+`
		ORDER BY `+itemDate+` DESC, i.id ASC
		LIMIT ?`, version, maxAttempts, nowStr, nowStr, limit)
	if err != nil {
		return nil, fmt.Errorf("pick classification candidates: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan classification candidate id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("read classification candidates: %w", err)
	}
	_ = rows.Close()
	if len(ids) == 0 {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit classification claim: %w", err)
		}
		return nil, nil
	}

	// 3. Take ownership. The eligibility predicate is repeated in the UPDATE —
	//    not just baked into the id list — because on Postgres a concurrent
	//    worker's claim can commit between step 2 and this statement; the
	//    re-checked predicate, evaluated under the row lock, makes the loser's
	//    UPDATE a no-op instead of a steal. Claiming also clears the failure
	//    backoff: the lease, not next_attempt_at, now bounds crash recovery.
	claimArgs := append([]any{token, sqlTimeOrNull(now.Add(lease)), version, version, nowStr},
		toAnySlice(ids)...)
	claimArgs = append(claimArgs, version, maxAttempts, nowStr, nowStr)
	if _, err := s.txExec(ctx, tx, `UPDATE item_classification SET
			claim_token = ?, claim_expires_at = ?, classifier_version = ?,
			attempts = CASE WHEN classifier_version = ? THEN attempts ELSE 0 END,
			next_attempt_at = NULL, updated_at = ?,
			status = 'pending', last_error = ''
		WHERE item_id IN (`+placeholders(len(ids))+`)
			AND EXISTS (SELECT 1 FROM items i WHERE i.id = item_classification.item_id
				AND `+classificationItemFilter("i")+`)
			AND `+classificationQueueFilter("item_classification"),
		claimArgs...); err != nil {
		return nil, fmt.Errorf("claim classification items: %w", err)
	}

	// 4. Return exactly what this claim owns.
	items, err := s.txQuery(ctx, tx, `SELECT i.id, i.source, i.title, i.link,
			COALESCE(i.summary, ''), i.published_at, COALESCE(i.tags, ''),
			COALESCE(c.attempts, 0)
		FROM items i JOIN item_classification c ON c.item_id = i.id
		WHERE c.claim_token = ?`, token)
	if err != nil {
		return nil, fmt.Errorf("read claimed items: %w", err)
	}
	defer func() { _ = items.Close() }()
	var out []ClassificationCandidate
	for items.Next() {
		var (
			item      feeds.Item
			published *time.Time
			tags      string
			attempts  int
		)
		if err := items.Scan(&item.ID, &item.Source, &item.Title, &item.Link,
			&item.Summary, &published, &tags, &attempts); err != nil {
			return nil, fmt.Errorf("scan classification candidate: %w", err)
		}
		if published != nil {
			item.Published = *published
		}
		if tags != "" {
			item.Tags = strings.Split(tags, ",")
		}
		out = append(out, ClassificationCandidate{Item: item, Attempts: attempts, ClaimToken: token})
	}
	if err := items.Err(); err != nil {
		return nil, fmt.Errorf("read claimed items: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit classification claim: %w", err)
	}
	return out, nil
}

// toAnySlice saves the caller an explicit loop when spreading ids into
// placeholders.
func toAnySlice(ids []string) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}

// MarkClassificationFailed records a failed attempt and schedules its next
// try. A missing item is ignored, as with tag replacement.
// MarkClassificationFailed records a failed attempt. An empty claimToken is an
// unguarded write for manual and tooling paths; a non-empty one must match the
// token the worker claimed, and a mismatch — the item was re-claimed, reset or
// enqueued after this worker's claim — returns an error wrapping
// ErrStaleClaim instead of overwriting the newer state. Both paths release the
// claim, so a failure never leaves a lease to expire. A missing item is not an
// error: pruning an item abandons its classification, and no row exists to
// update.
func (s *Store) MarkClassificationFailed(
	ctx context.Context,
	itemID string,
	version, attempts int,
	lastError string,
	next time.Time,
	claimToken string,
) error {
	if version < 1 {
		return fmt.Errorf("classifier version must be positive, got %d", version)
	}
	if attempts < 0 {
		return fmt.Errorf("attempts cannot be negative, got %d", attempts)
	}
	if claimToken == "" {
		_, err := s.exec(ctx, `INSERT INTO item_classification
			(item_id, classifier_version, status, attempts, next_attempt_at, last_error, updated_at)
			SELECT id, ?, 'failed', ?, ?, ?, ? FROM items WHERE id = ?
			ON CONFLICT (item_id) DO UPDATE SET
				classifier_version = excluded.classifier_version,
				status = 'failed',
				attempts = excluded.attempts,
				next_attempt_at = excluded.next_attempt_at,
				last_error = excluded.last_error,
				claim_token = '', claim_expires_at = NULL,
				updated_at = excluded.updated_at`,
			version, attempts, sqlTimeOrNull(next), lastError, sqlTime(time.Now()), itemID)
		if err != nil {
			return fmt.Errorf("record classification failure for item %s: %w", itemID, err)
		}
		return nil
	}
	res, err := s.exec(ctx, `UPDATE item_classification SET
			classifier_version = ?, status = 'failed', attempts = ?,
			next_attempt_at = ?, last_error = ?,
			claim_token = '', claim_expires_at = NULL, updated_at = ?
		WHERE item_id = ? AND claim_token = ?`,
		version, attempts, sqlTimeOrNull(next), lastError, sqlTime(time.Now()), itemID, claimToken)
	if err != nil {
		return fmt.Errorf("record classification failure for item %s: %w", itemID, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("record classification failure for item %s: %w", itemID, err)
	} else if n == 0 {
		var one int
		switch err := s.queryRow(ctx, `SELECT 1 FROM items WHERE id = ?`, itemID).Scan(&one); {
		case errors.Is(err, sql.ErrNoRows):
			return nil // pruned while we held the claim; nothing to record
		case err != nil:
			return fmt.Errorf("record classification failure for item %s: %w", itemID, err)
		}
		return fmt.Errorf("record classification failure for item %s: %w", itemID, ErrStaleClaim)
	}
	return nil
}

// ClassificationState returns the queue row for item, and false when none exists.
func (s *Store) ClassificationState(ctx context.Context, itemID string) (ClassificationState, bool, error) {
	var (
		state   ClassificationState
		next    sql.NullTime
		expires sql.NullTime
		upd     time.Time
	)
	err := s.queryRow(ctx, `SELECT item_id, classifier_version, status, attempts,
			next_attempt_at, last_error, updated_at, claim_token, claim_expires_at
		FROM item_classification WHERE item_id = ?`, itemID).
		Scan(&state.ItemID, &state.ClassifierVersion, &state.Status, &state.Attempts,
			&next, &state.LastError, &upd, &state.ClaimToken, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return ClassificationState{}, false, nil
	}
	if err != nil {
		return ClassificationState{}, false, fmt.Errorf("read classification state: %w", err)
	}
	state.NextAttemptAt = utcTimePtr(next)
	state.ClaimExpiresAt = utcTimePtr(expires)
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
