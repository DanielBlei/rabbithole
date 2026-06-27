package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"time"
)

// ideaSchema is the Maze page's idea board — loose sticky notes, separate from
// the todo board. Kept here (not in sqlite.go's items schema) so the concern
// stays self-contained; Open execs it alongside the items/todos migrations.
//
// deleted_at is a soft-delete tombstone: NULL means live, a timestamp means the
// note is hidden everywhere (the frontend never shows it, but the row survives
// for recovery/audit). A plain "deleted_at IS NULL" predicate is all the live
// query needs, so no extra boolean column is carried. position is the manual
// drag-and-drop order (ascending = display order); a new note is given a
// position below the current minimum so it lands first.
const ideaSchema = `
CREATE TABLE IF NOT EXISTS ideas (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	body       TEXT NOT NULL,
	color      TEXT NOT NULL DEFAULT 'amber',
	position   INTEGER NOT NULL DEFAULT 0,
	created_at TIMESTAMP NOT NULL,
	updated_at TIMESTAMP NOT NULL,
	deleted_at TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_ideas_live ON ideas(deleted_at, position);
`

// ideaAddColumns backfills columns on idea databases created before they existed,
// mirroring todoAddColumns. Runs after ideaSchema; a "duplicate column" error on
// a fresh DB is expected and tolerated by Open. Empty for now (kept for the
// pattern, so a later column can be added without touching Open).
var ideaAddColumns []string

// MaxIdeaBody caps a note's text. Sticky notes are meant to be loose and short;
// the web composer mirrors this with a maxlength, but AddIdea/UpdateIdea enforce
// it server-side too.
const MaxIdeaBody = 280

// defaultIdeaLimit bounds ListIdeas — generous, since a scratch board shouldn't
// grow unbounded but we never want to silently drop a live note.
const defaultIdeaLimit = 500

// IdeaColors is the sticky-note palette. It is the single source of truth shared
// by the store (random pick on create, validation on edit) and the web layer
// (the edit-mode colour swatches range over it). Order is presentation-only.
var IdeaColors = []string{"amber", "rose", "green", "cyan", "violet", "coral"}

// ErrIdeaNotFound is returned when no live idea matches the given id.
var ErrIdeaNotFound = errors.New("idea not found")

// ErrInvalidIdea is returned for a bad body (empty or over MaxIdeaBody). Callers
// can errors.Is against it to map the cause to an HTTP 400.
var ErrInvalidIdea = errors.New("invalid idea")

// Idea is one sticky note on the Maze board. Color is one of IdeaColors. Position
// is the manual order. DeletedAt is nil for a live note (soft-deleted notes are
// never returned by ListIdeas).
type Idea struct {
	ID        int64
	Body      string
	Color     string
	Position  int64
	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time
}

// ideaColumns is the SELECT list backing GetIdea and ListIdeas, kept in one place
// so its order stays in lockstep with scanIdea.
const ideaColumns = "id, body, color, position, created_at, updated_at, deleted_at"

// scanIdea maps one row of ideaColumns onto an Idea. Column order must match
// ideaColumns exactly.
func scanIdea(sc rowScanner) (Idea, error) {
	var (
		i         Idea
		deletedAt sql.NullTime
	)
	if err := sc.Scan(&i.ID, &i.Body, &i.Color, &i.Position, &i.CreatedAt, &i.UpdatedAt, &deletedAt); err != nil {
		return Idea{}, err
	}
	if deletedAt.Valid {
		i.DeletedAt = &deletedAt.Time
	}
	return i, nil
}

// validColor reports whether c is a known palette colour.
func validColor(c string) bool {
	for _, k := range IdeaColors {
		if k == c {
			return true
		}
	}
	return false
}

// randomColor picks a palette colour for a new note, so a fresh board still
// looks varied without the user choosing up front (they can recolour on edit).
func randomColor() string {
	return IdeaColors[rand.Intn(len(IdeaColors))]
}

// cleanBody trims and validates a note body (non-empty, at most MaxIdeaBody
// runes), returning ErrInvalidIdea on failure.
func cleanBody(body string) (string, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return "", fmt.Errorf("%w: empty body", ErrInvalidIdea)
	}
	if len([]rune(body)) > MaxIdeaBody {
		return "", fmt.Errorf("%w: body over %d characters", ErrInvalidIdea, MaxIdeaBody)
	}
	return body, nil
}

// AddIdea inserts a new live note at a position below the current minimum, so it
// sorts first (newest-on-top). color is the user's optional choice; an empty or
// unknown value falls back to a random palette colour (so a note still looks
// varied when created without picking). Returns the created Idea.
func (s *Store) AddIdea(ctx context.Context, body, color string) (Idea, error) {
	body, err := cleanBody(body)
	if err != nil {
		return Idea{}, err
	}
	if !validColor(color) {
		color = randomColor()
	}
	now := time.Now()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO ideas (body, color, position, created_at, updated_at)
		 VALUES (?, ?, (SELECT COALESCE(MIN(position), 0) - 1 FROM ideas WHERE deleted_at IS NULL), ?, ?)`,
		body, color, now, now)
	if err != nil {
		return Idea{}, fmt.Errorf("insert idea: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Idea{}, fmt.Errorf("idea insert id: %w", err)
	}
	return s.GetIdea(ctx, id)
}

// GetIdea returns the live note with the given id, or ErrIdeaNotFound. A
// soft-deleted note is treated as not found.
func (s *Store) GetIdea(ctx context.Context, id int64) (Idea, error) {
	i, err := scanIdea(s.db.QueryRowContext(ctx,
		"SELECT "+ideaColumns+" FROM ideas WHERE id = ? AND deleted_at IS NULL", id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Idea{}, fmt.Errorf("%w: %d", ErrIdeaNotFound, id)
		}
		return Idea{}, fmt.Errorf("get idea %d: %w", id, err)
	}
	return i, nil
}

// ListIdeas returns the live notes in manual order (position ascending, newest
// first on ties). Soft-deleted notes are excluded.
func (s *Store) ListIdeas(ctx context.Context) ([]Idea, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+ideaColumns+" FROM ideas WHERE deleted_at IS NULL ORDER BY position ASC, created_at DESC LIMIT ?",
		defaultIdeaLimit)
	if err != nil {
		return nil, fmt.Errorf("query ideas: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var ideas []Idea
	for rows.Next() {
		i, err := scanIdea(rows)
		if err != nil {
			return nil, fmt.Errorf("scan idea: %w", err)
		}
		ideas = append(ideas, i)
	}
	return ideas, rows.Err()
}

// UpdateIdea edits a live note's body and colour. An unknown colour is ignored
// (the current colour is kept) rather than erroring, so a stale form value can't
// blank the note. Returns the updated Idea, or ErrIdeaNotFound.
func (s *Store) UpdateIdea(ctx context.Context, id int64, body, color string) (Idea, error) {
	cur, err := s.GetIdea(ctx, id)
	if err != nil {
		return Idea{}, err
	}
	body, err = cleanBody(body)
	if err != nil {
		return Idea{}, err
	}
	if !validColor(color) {
		color = cur.Color
	}
	if _, err := s.db.ExecContext(ctx,
		"UPDATE ideas SET body = ?, color = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL",
		body, color, time.Now(), id); err != nil {
		return Idea{}, fmt.Errorf("update idea %d: %w", id, err)
	}
	return s.GetIdea(ctx, id)
}

// DeleteIdea soft-deletes a note by stamping deleted_at — the row stays in the
// database but is hidden everywhere. Returns ErrIdeaNotFound when no live note
// matched (already deleted or never existed).
func (s *Store) DeleteIdea(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx,
		"UPDATE ideas SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL",
		time.Now(), id)
	if err != nil {
		return fmt.Errorf("delete idea %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %d", ErrIdeaNotFound, id)
	}
	return nil
}

// ReorderIdeas rewrites the manual order to match ids (the new display order,
// left-to-right). Positions become 0..n-1 in that sequence. Unknown or
// soft-deleted ids are skipped. updated_at is deliberately left untouched so a
// reorder doesn't bump every note's "edited" time. Runs in one transaction.
func (s *Store) ReorderIdeas(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin reorder: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for pos, id := range ids {
		if _, err := tx.ExecContext(ctx,
			"UPDATE ideas SET position = ? WHERE id = ? AND deleted_at IS NULL",
			pos, id); err != nil {
			return fmt.Errorf("reorder idea %d: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit reorder: %w", err)
	}
	return nil
}