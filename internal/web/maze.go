// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/DanielBlei/rabbithole/internal/store"
)

// mazeData is the template model for the Maze page — the day-to-day tools home.
// It backs both the full page (via layout) and the todoWidget fragment that the
// mutation routes swap back, so it carries everything the widget renders: the
// open list, the completed list grouped by day, and the header counts.
type mazeData struct {
	Title      string
	Active     string
	Chrome     chromeData // shared topbar/rail state (ingest chip + rail action)
	PromptUser string     // shell-prompt user in the widgets' pane title bars
	Open       []todoView
	Due        []todoView // the subset of Open due today or overdue — its own tab
	DoneGroups []todoGroup
	OpenCount  int
	DueCount   int
	DoneCount  int
	AllTags    []string   // every tag in use, sorted — drives the dynamic tag filter
	Ideas      []ideaView // the idea board (sticky notes), manual order
	IdeaColors []string   // the note palette — drives the compose card's colour picker
}

// ideaView is the per-note view model for the idea board. Ago is a precomputed
// relative time; Palette is the colour swatches the edit panel renders (one per
// IdeaColors entry, with On marking the note's current colour).
type ideaView struct {
	ID      int64
	Body    string
	Color   string
	Ago     string
	Palette []ideaSwatch
}

// ideaSwatch is one colour option in a note's edit-mode palette.
type ideaSwatch struct {
	Name string
	On   bool
}

// todoView is the per-task view model. DueClass is precomputed (rather than
// branched in the template) so the red "due today / overdue" colouring lives in
// one place; Red mirrors it for the header count.
type todoView struct {
	ID       int64
	Title    string
	Done     bool
	HasNote  bool
	NoteHTML template.HTML
	HasDue   bool
	Due      string
	DueClass string
	Red      bool
	Tags     []string // labels as stored (preserves casing)
	TagsAttr string   // lowercased, comma-joined — the row's data-tags for the JS filter
}

// todoGroup is a day's worth of completed tasks under a heading ("Today",
// "Yesterday", or a date).
type todoGroup struct {
	Label string
	Todos []todoView
}

func (s *Web) handleMaze(w http.ResponseWriter, r *http.Request) {
	data, err := s.mazeData(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := mazeTmpl.ExecuteTemplate(w, "layout", data); err != nil {
		// Status is likely already written; log rather than double-write.
		log.Error().Err(err).Msg("render maze")
	}
}

// mazeData loads the open and completed task lists and builds the page model:
// open tasks as a flat list (already due-ordered by the store), completed tasks
// grouped by the day they were finished.
func (s *Web) mazeData(ctx context.Context) (mazeData, error) {
	open, err := s.db.ListTodos(ctx, store.TodoFilter{Done: ptrBool(false)})
	if err != nil {
		return mazeData{}, err
	}
	done, err := s.db.ListTodos(ctx, store.TodoFilter{Done: ptrBool(true)})
	if err != nil {
		return mazeData{}, err
	}
	ideas, err := s.db.ListIdeas(ctx)
	if err != nil {
		return mazeData{}, err
	}

	today := startOfDay(time.Now())
	openViews := make([]todoView, len(open))
	var due []todoView
	for i, t := range open {
		v := toTodoView(t, today)
		if v.Red {
			due = append(due, v)
		}
		openViews[i] = v
	}

	return mazeData{
		Title:      "The Rabbit Hole",
		Active:     "maze",
		Chrome:     s.chrome(ctx),
		PromptUser: s.user,
		Open:       openViews,
		Due:        due,
		DoneGroups: groupByDay(done, today),
		OpenCount:  len(open),
		DueCount:   len(due),
		DoneCount:  len(done),
		AllTags:    collectTags(open, done),
		Ideas:      toIdeaViews(ideas, time.Now()),
		IdeaColors: store.IdeaColors,
	}, nil
}

// toIdeaViews builds the idea board view models, precomputing each note's
// relative-time label and colour palette so the template stays declarative.
func toIdeaViews(ideas []store.Idea, now time.Time) []ideaView {
	views := make([]ideaView, len(ideas))
	for i, idea := range ideas {
		palette := make([]ideaSwatch, len(store.IdeaColors))
		for j, c := range store.IdeaColors {
			palette[j] = ideaSwatch{Name: c, On: c == idea.Color}
		}
		views[i] = ideaView{
			ID:      idea.ID,
			Body:    idea.Body,
			Color:   idea.Color,
			Ago:     relTime(idea.UpdatedAt, now),
			Palette: palette,
		}
	}
	return views
}

// relTime renders a compact "how long ago" label for a note's last change:
// "just now", "5m", "2h", "3d", then a calendar date once it's over a week old.
func relTime(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	default:
		return t.Format("2 Jan")
	}
}

// collectTags returns the sorted, deduped union of tags across every task — the
// universe the filter chips and the add-form suggestions draw from. Dedup is
// case-insensitive (first-seen casing wins) to match the store's normalisation.
func collectTags(lists ...[]store.Todo) []string {
	var tags []string
	seen := make(map[string]bool)
	for _, list := range lists {
		for _, t := range list {
			for _, tag := range t.Tags {
				key := strings.ToLower(tag)
				if seen[key] {
					continue
				}
				seen[key] = true
				tags = append(tags, tag)
			}
		}
	}
	sort.Slice(tags, func(i, j int) bool {
		return strings.ToLower(tags[i]) < strings.ToLower(tags[j])
	})
	return tags
}

// toTodoView builds a task's view model. The due chip turns red the day a task is
// due and stays red while overdue (both states the user should act on); a future
// due date renders dim. A completed task's due date is shown neutral — it's no
// longer a deadline.
func toTodoView(t store.Todo, today time.Time) todoView {
	v := todoView{ID: t.ID, Title: t.Title, Done: t.Done, Tags: t.Tags}
	if len(t.Tags) > 0 {
		v.TagsAttr = strings.ToLower(strings.Join(t.Tags, ","))
	}
	if t.Note != "" {
		v.HasNote = true
		v.NoteHTML = renderMarkdown(t.Note)
	}
	if t.DueOn != nil {
		v.HasDue = true
		v.Due = t.DueOn.Format("2 Jan")
		v.DueClass = "todo__due"
		if !t.Done {
			switch d := startOfDay(*t.DueOn); {
			case d.Before(today):
				v.DueClass += " todo__due--overdue"
				v.Red = true
			case d.Equal(today):
				v.DueClass += " todo__due--today"
				v.Red = true
			default:
				v.DueClass += " todo__due--upcoming"
			}
		}
	}
	return v
}

// groupByDay splits completed tasks (already ordered most-recent-first by the
// store) into per-day groups, preserving that order.
func groupByDay(todos []store.Todo, today time.Time) []todoGroup {
	var groups []todoGroup
	var curKey string
	for _, t := range todos {
		when := today
		if t.CompletedAt != nil {
			when = startOfDay(*t.CompletedAt)
		}
		key := when.Format("2006-01-02")
		if len(groups) == 0 || key != curKey {
			groups = append(groups, todoGroup{Label: dayLabel(when, today)})
			curKey = key
		}
		last := len(groups) - 1
		groups[last].Todos = append(groups[last].Todos, toTodoView(t, today))
	}
	return groups
}

// dayLabel names a day relative to today for the completed-by-date headings.
func dayLabel(day, today time.Time) string {
	switch {
	case day.Equal(today):
		return "Today"
	case day.Equal(today.AddDate(0, 0, -1)):
		return "Yesterday"
	default:
		return day.Format("2 Jan 2006")
	}
}

// handleAddTodo creates a task from the add form (title, optional note and due
// date), then re-renders the whole widget so the new task lands in the open list.
func (s *Web) handleAddTodo(w http.ResponseWriter, r *http.Request) {
	title := r.FormValue("title")
	note := r.FormValue("note")
	due := parseDue(r.FormValue("due"))
	tags := parseTags(r.FormValue("tags"))

	if _, err := s.db.AddTodo(r.Context(), title, note, due, tags); err != nil {
		if errors.Is(err, store.ErrInvalidTodo) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderTodoWidget(w, r)
}

// handleToggleTodo flips a task between open and completed; handleDeleteTodo
// removes it. Both re-render the whole widget — a toggle moves a task between the
// open and completed lists, so re-rendering both keeps them consistent without
// any client-side bookkeeping.
func (s *Web) handleToggleTodo(w http.ResponseWriter, r *http.Request) {
	id, err := todoID(r)
	if err != nil {
		http.Error(w, "invalid todo id", http.StatusBadRequest)
		return
	}
	if _, err := s.db.ToggleTodo(r.Context(), id); err != nil {
		httpTodoError(w, err)
		return
	}
	s.renderTodoWidget(w, r)
}

// handleSetTodoTags replaces a task's tags from the inline tag editor, then
// re-renders the whole widget so the row chips and the tag filter reflect the
// change (a brand-new tag has to appear in the filter immediately).
func (s *Web) handleSetTodoTags(w http.ResponseWriter, r *http.Request) {
	id, err := todoID(r)
	if err != nil {
		http.Error(w, "invalid todo id", http.StatusBadRequest)
		return
	}
	if _, err := s.db.SetTodoTags(r.Context(), id, parseTags(r.FormValue("tags"))); err != nil {
		httpTodoError(w, err)
		return
	}
	s.renderTodoWidget(w, r)
}

func (s *Web) handleDeleteTodo(w http.ResponseWriter, r *http.Request) {
	id, err := todoID(r)
	if err != nil {
		http.Error(w, "invalid todo id", http.StatusBadRequest)
		return
	}
	if err := s.db.DeleteTodo(r.Context(), id); err != nil {
		httpTodoError(w, err)
		return
	}
	s.renderTodoWidget(w, r)
}

// renderTodoWidget re-renders the todo widget fragment (open + completed lists
// and the header counts) — the swap response shared by every todo mutation.
func (s *Web) renderTodoWidget(w http.ResponseWriter, r *http.Request) {
	data, err := s.mazeData(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := mazeTmpl.ExecuteTemplate(w, "todoWidget", data); err != nil {
		// Status is likely already written; log rather than double-write.
		log.Error().Err(err).Msg("render todo widget")
	}
}

// handleAddIdea creates a sticky note from the composer (just a body — the store
// assigns a random colour), then re-renders the whole idea widget so the new note
// lands at the front of the board.
func (s *Web) handleAddIdea(w http.ResponseWriter, r *http.Request) {
	if _, err := s.db.AddIdea(r.Context(), r.FormValue("body"), r.FormValue("color")); err != nil {
		if errors.Is(err, store.ErrInvalidIdea) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderIdeaWidget(w, r)
}

// handleUpdateIdea saves an edited note's body and colour, then re-renders the
// widget (which returns the note to read mode — the fresh DOM has no edit state).
func (s *Web) handleUpdateIdea(w http.ResponseWriter, r *http.Request) {
	id, err := ideaID(r)
	if err != nil {
		http.Error(w, "invalid idea id", http.StatusBadRequest)
		return
	}
	if _, err := s.db.UpdateIdea(r.Context(), id, r.FormValue("body"), r.FormValue("color")); err != nil {
		httpIdeaError(w, err)
		return
	}
	s.renderIdeaWidget(w, r)
}

// handleDeleteIdea soft-deletes a note (the row stays in the DB, just hidden),
// then re-renders the widget so the note drops out of the board.
func (s *Web) handleDeleteIdea(w http.ResponseWriter, r *http.Request) {
	id, err := ideaID(r)
	if err != nil {
		http.Error(w, "invalid idea id", http.StatusBadRequest)
		return
	}
	if err := s.db.DeleteIdea(r.Context(), id); err != nil {
		httpIdeaError(w, err)
		return
	}
	s.renderIdeaWidget(w, r)
}

// handleReorderIdeas persists a drag-and-drop reorder. The client posts the new
// left-to-right id sequence as a comma-joined "ids" field; the DOM is already in
// that order, so there's nothing to swap back — a 204 keeps htmx from touching
// the board mid-interaction.
func (s *Web) handleReorderIdeas(w http.ResponseWriter, r *http.Request) {
	ids := parseIDs(r.FormValue("ids"))
	if err := s.db.ReorderIdeas(r.Context(), ids); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// renderIdeaWidget re-renders the idea widget fragment — the swap response shared
// by the add/edit/delete idea mutations.
func (s *Web) renderIdeaWidget(w http.ResponseWriter, r *http.Request) {
	data, err := s.mazeData(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := mazeTmpl.ExecuteTemplate(w, "ideaWidget", data); err != nil {
		// Status is likely already written; log rather than double-write.
		log.Error().Err(err).Msg("render idea widget")
	}
}

// ideaID reads the {id} path value as an int64.
func ideaID(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}

// parseIDs splits the comma-joined id list from the reorder request, dropping any
// non-numeric entries rather than failing the whole reorder.
func parseIDs(v string) []int64 {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	var ids []int64
	for _, part := range strings.Split(v, ",") {
		if n, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64); err == nil {
			ids = append(ids, n)
		}
	}
	return ids
}

// httpIdeaError maps a store error to a response: a missing note is a 404, a bad
// body a 400, anything else a 500.
func httpIdeaError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrIdeaNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, store.ErrInvalidIdea):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// todoID reads the {id} path value as an int64.
func todoID(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}

// parseDue turns the date input value ("2006-01-02" or empty) into a due date at
// local midnight, or nil. A malformed value is treated as "no due date" rather
// than an error — the field is optional and the browser already constrains it.
func parseDue(v string) *time.Time {
	if v == "" {
		return nil
	}
	if d, err := time.ParseInLocation("2006-01-02", v, time.Local); err == nil {
		return &d
	}
	return nil
}

// parseTags splits the comma-joined "tags" form value (built by the tag-chip
// input) into a slice; the store normalises it. Empty yields nil.
func parseTags(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return strings.Split(v, ",")
}

// httpTodoError maps a store error to a response: a missing task is a 404,
// anything else a 500.
func httpTodoError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrTodoNotFound) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

func ptrBool(b bool) *bool { return &b }
