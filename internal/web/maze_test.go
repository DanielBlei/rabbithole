// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/DanielBlei/rabbithole/internal/store"
)

// postForm sends a urlencoded form POST and asserts a 200, returning the body.
func postForm(t *testing.T, w *Web, path string, form url.Values) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST %s: status = %d, want 200; body=%s", path, rec.Code, rec.Body)
	}
	return rec.Body.String()
}

// The Maze page renders at "/maze" with the todo widget and all three tab labels.
func TestMazePageRenders(t *testing.T) {
	w := newTestWeb(t)
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/maze", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /maze: status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`id="todoWidget"`, "Open", "Due / Overdue", "Completed", "Add a task"} {
		if !strings.Contains(body, want) {
			t.Errorf("maze page missing %q", want)
		}
	}
}

// The landing page "/" is the feed, not the maze.
func TestRootServesFeed(t *testing.T) {
	w := newTestWeb(t)
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /: status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, `id="todoWidget"`) {
		t.Error("GET / should render the feed, not the maze todo widget")
	}
}

// Adding a task via the form persists it and returns the widget fragment showing
// the new title.
func TestAddTodoFlow(t *testing.T) {
	w := newTestWeb(t)

	body := postForm(t, w, "/todos", url.Values{"title": {"buy carrots"}})
	if !strings.Contains(body, "buy carrots") {
		t.Errorf("add response missing the new task; body=%s", body)
	}
	open, err := w.db.ListTodos(context.Background(), store.TodoFilter{Done: ptrBool(false)})
	if err != nil || len(open) != 1 || open[0].Title != "buy carrots" {
		t.Fatalf("store after add = %v (%v), want one 'buy carrots'", open, err)
	}
}

// An empty title is rejected at the form boundary as a 400, not written.
func TestAddTodoEmptyTitle(t *testing.T) {
	w := newTestWeb(t)
	req := httptest.NewRequest(http.MethodPost, "/todos", strings.NewReader("title=+++"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// Toggling completes a task (it leaves the open list), and deleting removes it.
func TestToggleAndDeleteTodo(t *testing.T) {
	w := newTestWeb(t)
	todo, err := w.db.AddTodo(context.Background(), "a task", "", nil, nil)
	if err != nil {
		t.Fatalf("AddTodo: %v", err)
	}
	id := strconv.FormatInt(todo.ID, 10)

	postForm(t, w, "/todos/"+id+"/toggle", nil)
	got, _ := w.db.GetTodo(context.Background(), todo.ID)
	if !got.Done {
		t.Error("task should be done after toggle")
	}

	req := httptest.NewRequest(http.MethodDelete, "/todos/"+id, nil)
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE: status = %d, want 200", rec.Code)
	}
	if all, _ := w.db.ListTodos(context.Background(), store.TodoFilter{}); len(all) != 0 {
		t.Errorf("task list after delete = %d, want 0", len(all))
	}
}

// A mutation on an unknown task id is a 404.
func TestTodoMutationUnknown(t *testing.T) {
	w := newTestWeb(t)
	req := httptest.NewRequest(http.MethodDelete, "/todos/999", nil)
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}
