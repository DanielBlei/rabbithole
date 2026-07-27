package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// todoSchema is the Maze page's task board. Kept here (not in sqlite.go's items
// schema) so the todo concern stays self-contained; Open execs it alongside the
// items migrations. due_on holds a date-only "2006-01-02" string — a calendar
// day is all the "due today / overdue" colouring needs, it sorts chronologically
// as text, and storing it in a plain TEXT column (not DATE) keeps the driver
// from auto-converting it to a time.Time and dropping the value on scan.
// completed_at stamps when a task was checked off, powering the by-date view.
// tags holds the task's labels as a comma-joined string (normalised by
// normalizeTags) — a plain column rather than a junction table because the board
// is small and the web layer derives the tag universe in-memory.
const todoSchema = `
CREATE TABLE IF NOT EXISTS todos (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	title        TEXT NOT NULL,
	note         TEXT NOT NULL DEFAULT '',
	done         BOOLEAN NOT NULL DEFAULT 0,
	due_on       TEXT,
	completed_at TIMESTAMP,
	tags         TEXT NOT NULL DEFAULT '',
	created_at   TIMESTAMP NOT NULL,
	updated_at   TIMESTAMP NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_todos_done ON todos(done);
CREATE INDEX IF NOT EXISTS idx_todos_due ON todos(due_on);
`

// MaxTodoTitle caps a task title: titles are meant to be short and scannable, so
// anything longer belongs in the (optional) note. The web add form mirrors this
// with a maxlength, but AddTodo enforces it server-side too.
const MaxTodoTitle = 80

// defaultTodoLimit bounds ListTodos when the caller leaves Limit unset — enough
// to show a full board without an unbounded scan as completed tasks accumulate.
const defaultTodoLimit = 200

// dueDateLayout is the on-disk format for due_on: a bare calendar date, sortable
// lexicographically (so ORDER BY due_on is chronological) and timezone-free.
const dueDateLayout = "2006-01-02"

// maxTodoTags caps how many tags a task carries and maxTodoTag the rune length of
// each — labels are meant to be short and few, and the cap keeps a pasted blob
// from bloating the row or the filter.
const (
	maxTodoTags = 12
	maxTodoTag  = 30
)

// ErrTodoNotFound is returned when no task matches the given id.
var ErrTodoNotFound = errors.New("todo not found")

// ErrInvalidTodo is returned by AddTodo for a bad title (empty or over
// MaxTodoTitle). It wraps a more specific message; callers can errors.Is against
// it to map the cause to an HTTP 400 rather than a 500.
var ErrInvalidTodo = errors.New("invalid todo")

// Todo is one task on the Maze board. DueOn is the date the task is due (nil when
// none), normalised to local midnight on read; CompletedAt is set when Done and
// cleared when a task is re-opened.
type Todo struct {
	ID          int64
	Title       string
	Note        string
	Done        bool
	DueOn       *time.Time
	CompletedAt *time.Time
	Tags        []string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// TodoFilter narrows ListTodos. A nil Done matches both states; a non-nil Done
// restricts to open (false) or completed (true). Limit<=0 falls back to
// defaultTodoLimit.
type TodoFilter struct {
	Done  *bool
	Limit int
}

// todoColumns is the SELECT list backing GetTodo and ListTodos, kept in one place
// so its order stays in lockstep with scanTodo.
const todoColumns = "id, title, note, done, due_on, completed_at, tags, created_at, updated_at"

// scanTodo maps one row of todoColumns onto a Todo. due_on comes back as the bare
// date string and is parsed to a local-midnight time so the web layer's
// today/overdue comparisons stay in one timezone. Column order must match
// todoColumns exactly.
func scanTodo(sc rowScanner) (Todo, error) {
	var (
		t           Todo
		due         sql.NullString
		completedAt sql.NullTime
		tags        string
	)
	if err := sc.Scan(
		&t.ID,
		&t.Title,
		&t.Note,
		&t.Done,
		&due,
		&completedAt,
		&tags,
		&t.CreatedAt,
		&t.UpdatedAt,
	); err != nil {
		return Todo{}, err
	}
	if due.Valid && due.String != "" {
		if d, err := time.ParseInLocation(dueDateLayout, due.String, time.Local); err == nil {
			t.DueOn = &d
		}
	}
	if completedAt.Valid {
		t.CompletedAt = &completedAt.Time
	}
	t.Tags = splitTags(tags)
	return t, nil
}

// normalizeTags cleans a tag list for storage: each tag is trimmed, has commas
// (the on-disk delimiter) stripped and is truncated to maxTodoTag runes; empties
// are dropped and duplicates removed case-insensitively (first-seen casing wins);
// the result is capped at maxTodoTags. Returns nil when nothing survives.
func normalizeTags(tags []string) []string {
	var out []string
	seen := make(map[string]bool)
	for _, tag := range tags {
		tag = strings.TrimSpace(strings.ReplaceAll(tag, ",", ""))
		if tag == "" {
			continue
		}
		if r := []rune(tag); len(r) > maxTodoTag {
			tag = strings.TrimSpace(string(r[:maxTodoTag]))
		}
		key := strings.ToLower(tag)
		if tag == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, tag)
		if len(out) == maxTodoTags {
			break
		}
	}
	return out
}

// splitTags parses the comma-joined on-disk tag string back into a slice,
// re-normalising so a hand-edited column can't surface stray blanks.
func splitTags(s string) []string {
	if s == "" {
		return nil
	}
	return normalizeTags(strings.Split(s, ","))
}

// joinTags normalises and comma-joins tags for storage.
func joinTags(tags []string) string {
	return strings.Join(normalizeTags(tags), ",")
}

// AddTodo inserts a new open task. title is trimmed and validated (non-empty, at
// most MaxTodoTitle runes); note is optional free text; due is the optional due
// date (only its calendar day is kept); tags are optional labels, normalised by
// normalizeTags. It returns the created Todo.
func (s *Store) AddTodo(ctx context.Context, title, note string, due *time.Time, tags []string) (Todo, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return Todo{}, fmt.Errorf("%w: empty title", ErrInvalidTodo)
	}
	if len([]rune(title)) > MaxTodoTitle {
		return Todo{}, fmt.Errorf("%w: title over %d characters", ErrInvalidTodo, MaxTodoTitle)
	}

	var dueVal any
	if due != nil {
		dueVal = due.Format(dueDateLayout)
	}
	now := time.Now()
	res, err := s.db.ExecContext(ctx,
		"INSERT INTO todos (title, note, done, due_on, tags, created_at, updated_at) VALUES (?, ?, 0, ?, ?, ?, ?)",
		title, strings.TrimSpace(note), dueVal, joinTags(tags), now, now)
	if err != nil {
		return Todo{}, fmt.Errorf("insert todo: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Todo{}, fmt.Errorf("todo insert id: %w", err)
	}
	return s.GetTodo(ctx, id)
}

// GetTodo returns the task with the given id, or ErrTodoNotFound.
func (s *Store) GetTodo(ctx context.Context, id int64) (Todo, error) {
	t, err := scanTodo(s.db.QueryRowContext(ctx, "SELECT "+todoColumns+" FROM todos WHERE id = ?", id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Todo{}, fmt.Errorf("%w: %d", ErrTodoNotFound, id)
		}
		return Todo{}, fmt.Errorf("get todo %d: %w", id, err)
	}
	return t, nil
}

// ListTodos returns tasks matching filter. Open tasks are ordered by due date
// (soonest first, undated last) then creation; completed tasks are ordered by
// most-recently completed, which is also how the by-date view groups them.
func (s *Store) ListTodos(ctx context.Context, filter TodoFilter) ([]Todo, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = defaultTodoLimit
	}

	q := "SELECT " + todoColumns + " FROM todos"
	var args []any
	if filter.Done != nil {
		q += " WHERE done = ?"
		args = append(args, *filter.Done)
	}
	if filter.Done != nil && *filter.Done {
		q += " ORDER BY completed_at DESC"
	} else {
		q += " ORDER BY due_on IS NULL, due_on ASC, created_at ASC"
	}
	q += " LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query todos: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var todos []Todo
	for rows.Next() {
		t, err := scanTodo(rows)
		if err != nil {
			return nil, fmt.Errorf("scan todo: %w", err)
		}
		todos = append(todos, t)
	}
	return todos, rows.Err()
}

// ToggleTodo flips a task's done state: an open task becomes completed (stamping
// completed_at), a completed one re-opens (clearing it). It returns the updated
// Todo, or ErrTodoNotFound.
func (s *Store) ToggleTodo(ctx context.Context, id int64) (Todo, error) {
	cur, err := s.GetTodo(ctx, id)
	if err != nil {
		return Todo{}, err
	}

	done := !cur.Done
	now := time.Now()
	var completed any
	if done {
		completed = now
	}
	if _, err := s.db.ExecContext(ctx,
		"UPDATE todos SET done = ?, completed_at = ?, updated_at = ? WHERE id = ?",
		done, completed, now, id); err != nil {
		return Todo{}, fmt.Errorf("toggle todo %d: %w", id, err)
	}
	return s.GetTodo(ctx, id)
}

// SetTodoTags replaces a task's labels with the normalised tags and returns the
// updated Todo, or ErrTodoNotFound when no task matched.
func (s *Store) SetTodoTags(ctx context.Context, id int64, tags []string) (Todo, error) {
	res, err := s.db.ExecContext(ctx,
		"UPDATE todos SET tags = ?, updated_at = ? WHERE id = ?",
		joinTags(tags), time.Now(), id)
	if err != nil {
		return Todo{}, fmt.Errorf("set todo tags %d: %w", id, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return Todo{}, fmt.Errorf("rows affected: %w", err)
	} else if n == 0 {
		return Todo{}, fmt.Errorf("%w: %d", ErrTodoNotFound, id)
	}
	return s.GetTodo(ctx, id)
}

// DeleteTodo removes a task, returning ErrTodoNotFound when none matched.
func (s *Store) DeleteTodo(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM todos WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete todo %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %d", ErrTodoNotFound, id)
	}
	return nil
}
