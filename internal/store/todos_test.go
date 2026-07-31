// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTodoStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, context.Background()
}

// A task's full lifecycle: added open, completed (which stamps completed_at and
// moves it to the done list), then re-opened and deleted — each transition
// reflected by ListTodos.
func TestTodoLifecycle(t *testing.T) {
	db, ctx := openTodoStore(t)

	todo, err := db.AddTodo(ctx, "  write the thing  ", "with a note", nil, nil)
	if err != nil {
		t.Fatalf("AddTodo: %v", err)
	}
	if todo.Title != "write the thing" {
		t.Errorf("title not trimmed: %q", todo.Title)
	}
	if todo.Done || todo.CompletedAt != nil {
		t.Error("new task should be open with no completed_at")
	}

	open, err := db.ListTodos(ctx, TodoFilter{Done: boolPtr(false)})
	if err != nil || len(open) != 1 {
		t.Fatalf("open list = %d (%v), want 1", len(open), err)
	}

	done, err := db.ToggleTodo(ctx, todo.ID)
	if err != nil {
		t.Fatalf("ToggleTodo: %v", err)
	}
	if !done.Done || done.CompletedAt == nil {
		t.Error("toggled task should be done with completed_at set")
	}

	if open, _ := db.ListTodos(ctx, TodoFilter{Done: boolPtr(false)}); len(open) != 0 {
		t.Errorf("open list after complete = %d, want 0", len(open))
	}
	if comp, _ := db.ListTodos(ctx, TodoFilter{Done: boolPtr(true)}); len(comp) != 1 {
		t.Errorf("completed list = %d, want 1", len(comp))
	}

	reopened, err := db.ToggleTodo(ctx, todo.ID)
	if err != nil {
		t.Fatalf("ToggleTodo re-open: %v", err)
	}
	if reopened.Done || reopened.CompletedAt != nil {
		t.Error("re-opened task should clear done and completed_at")
	}

	if err := db.DeleteTodo(ctx, todo.ID); err != nil {
		t.Fatalf("DeleteTodo: %v", err)
	}
	if all, _ := db.ListTodos(ctx, TodoFilter{}); len(all) != 0 {
		t.Errorf("list after delete = %d, want 0", len(all))
	}
}

// Open tasks are ordered by due date soonest-first, with undated tasks last —
// the order the board renders top-to-bottom.
func TestListTodosDueOrder(t *testing.T) {
	db, ctx := openTodoStore(t)

	day := func(s string) *time.Time {
		d, _ := time.ParseInLocation(dueDateLayout, s, time.Local)
		return &d
	}
	if _, err := db.AddTodo(ctx, "no due", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddTodo(ctx, "later", "", day("2026-12-01"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddTodo(ctx, "soon", "", day("2026-06-27"), nil); err != nil {
		t.Fatal(err)
	}

	open, err := db.ListTodos(ctx, TodoFilter{Done: boolPtr(false)})
	if err != nil {
		t.Fatalf("ListTodos: %v", err)
	}
	var got []string
	for _, todo := range open {
		got = append(got, todo.Title)
	}
	want := []string{"soon", "later", "no due"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v", got, want)
	}
	// A round-tripped due date survives as the same calendar day.
	if open[0].DueOn == nil || open[0].DueOn.Format(dueDateLayout) != "2026-06-27" {
		t.Errorf("due date did not round-trip: %v", open[0].DueOn)
	}
}

// AddTodo rejects an empty or over-long title with ErrInvalidTodo (so the web
// layer can answer 400), and never writes the bad row.
func TestAddTodoValidation(t *testing.T) {
	db, ctx := openTodoStore(t)

	if _, err := db.AddTodo(ctx, "   ", "", nil, nil); !errors.Is(err, ErrInvalidTodo) {
		t.Errorf("empty title err = %v, want ErrInvalidTodo", err)
	}
	if _, err := db.AddTodo(ctx, strings.Repeat("x", MaxTodoTitle+1), "", nil, nil); !errors.Is(err, ErrInvalidTodo) {
		t.Errorf("long title err = %v, want ErrInvalidTodo", err)
	}
	if all, _ := db.ListTodos(ctx, TodoFilter{}); len(all) != 0 {
		t.Errorf("invalid adds wrote %d rows, want 0", len(all))
	}
}

// Toggling or deleting a task that doesn't exist is ErrTodoNotFound, not a
// silent no-op.
func TestTodoNotFound(t *testing.T) {
	db, ctx := openTodoStore(t)
	if _, err := db.ToggleTodo(ctx, 999); !errors.Is(err, ErrTodoNotFound) {
		t.Errorf("toggle missing err = %v, want ErrTodoNotFound", err)
	}
	if err := db.DeleteTodo(ctx, 999); !errors.Is(err, ErrTodoNotFound) {
		t.Errorf("delete missing err = %v, want ErrTodoNotFound", err)
	}
}

// Tags survive an add round-trip after normalisation (trim, drop blanks, dedupe
// case-insensitively), and SetTodoTags replaces them on an existing task.
func TestTodoTags(t *testing.T) {
	db, ctx := openTodoStore(t)

	todo, err := db.AddTodo(ctx, "tagged", "", nil,
		[]string{" day-to-day ", "work", "Work", "", "day-to-day"})
	if err != nil {
		t.Fatalf("AddTodo: %v", err)
	}
	if got := strings.Join(todo.Tags, ","); got != "day-to-day,work" {
		t.Errorf("tags after add = %q, want %q", got, "day-to-day,work")
	}

	// Tags round-trip through a fresh read.
	reread, err := db.GetTodo(ctx, todo.ID)
	if err != nil {
		t.Fatalf("GetTodo: %v", err)
	}
	if strings.Join(reread.Tags, ",") != "day-to-day,work" {
		t.Errorf("tags did not round-trip: %v", reread.Tags)
	}

	// SetTodoTags replaces the whole set.
	updated, err := db.SetTodoTags(ctx, todo.ID, []string{"errand"})
	if err != nil {
		t.Fatalf("SetTodoTags: %v", err)
	}
	if strings.Join(updated.Tags, ",") != "errand" {
		t.Errorf("tags after set = %v, want [errand]", updated.Tags)
	}

	if _, err := db.SetTodoTags(ctx, 999, []string{"x"}); !errors.Is(err, ErrTodoNotFound) {
		t.Errorf("set tags on missing err = %v, want ErrTodoNotFound", err)
	}
}

// normalizeTags caps both the per-tag length and the tag count.
func TestNormalizeTagsCaps(t *testing.T) {
	long := normalizeTags([]string{strings.Repeat("x", maxTodoTag+5)})
	if len(long) != 1 || len([]rune(long[0])) != maxTodoTag {
		t.Errorf("over-long tag not truncated to %d: %v", maxTodoTag, long)
	}

	var many []string
	for i := 0; i < maxTodoTags+5; i++ {
		many = append(many, fmt.Sprintf("t%d", i))
	}
	if got := normalizeTags(many); len(got) != maxTodoTags {
		t.Errorf("tag count = %d, want capped at %d", len(got), maxTodoTags)
	}
}

func boolPtr(b bool) *bool { return &b }
