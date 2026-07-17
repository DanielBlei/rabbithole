package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func openIdeaStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, context.Background()
}

// liveBodies returns the bodies of the live notes in board order — a compact way
// to assert ordering and presence.
func liveBodies(t *testing.T, db *Store, ctx context.Context) []string {
	t.Helper()
	ideas, err := db.ListIdeas(ctx)
	if err != nil {
		t.Fatalf("ListIdeas: %v", err)
	}
	out := make([]string, len(ideas))
	for i, idea := range ideas {
		out[i] = idea.Body
	}
	return out
}

// A note's lifecycle: added (random palette colour, body trimmed), edited
// (body + colour), then soft-deleted — gone from the board but the row survives
// with deleted_at set.
func TestIdeaLifecycle(t *testing.T) {
	db, ctx := openIdeaStore(t)

	idea, err := db.AddIdea(ctx, "  a loose thought  ", "")
	if err != nil {
		t.Fatalf("AddIdea: %v", err)
	}
	if idea.Body != "a loose thought" {
		t.Errorf("body not trimmed: %q", idea.Body)
	}
	if !validColor(idea.Color) {
		t.Errorf("new note got colour %q, not in palette", idea.Color)
	}

	edited, err := db.UpdateIdea(ctx, idea.ID, "sharper thought", "violet")
	if err != nil {
		t.Fatalf("UpdateIdea: %v", err)
	}
	if edited.Body != "sharper thought" || edited.Color != "violet" {
		t.Errorf("edit not applied: %+v", edited)
	}

	if err := db.DeleteIdea(ctx, idea.ID); err != nil {
		t.Fatalf("DeleteIdea: %v", err)
	}
	if got := liveBodies(t, db, ctx); len(got) != 0 {
		t.Errorf("board after delete = %v, want empty", got)
	}
	// Soft delete: GetIdea treats it as gone, but the row is retained with a
	// deleted_at stamp.
	if _, err := db.GetIdea(ctx, idea.ID); !errors.Is(err, ErrIdeaNotFound) {
		t.Errorf("GetIdea after delete err = %v, want ErrIdeaNotFound", err)
	}
	var deletedNotNull bool
	if err := db.db.QueryRowContext(ctx,
		"SELECT deleted_at IS NOT NULL FROM ideas WHERE id = ?", idea.ID).Scan(&deletedNotNull); err != nil {
		t.Fatalf("query tombstone: %v", err)
	}
	if !deletedNotNull {
		t.Error("soft-deleted row should keep its data with deleted_at set")
	}
}

// New notes land at the front of the board (newest-first), and a reorder rewrites
// that order to an arbitrary sequence.
func TestIdeaOrdering(t *testing.T) {
	db, ctx := openIdeaStore(t)

	first, _ := db.AddIdea(ctx, "first", "")
	second, _ := db.AddIdea(ctx, "second", "")
	third, _ := db.AddIdea(ctx, "third", "")

	if got := liveBodies(t, db, ctx); strings.Join(got, ",") != "third,second,first" {
		t.Errorf("default order = %v, want newest-first", got)
	}

	// Reorder to first, third, second.
	if err := db.ReorderIdeas(ctx, []int64{first.ID, third.ID, second.ID}); err != nil {
		t.Fatalf("ReorderIdeas: %v", err)
	}
	if got := liveBodies(t, db, ctx); strings.Join(got, ",") != "first,third,second" {
		t.Errorf("order after reorder = %v", got)
	}
}

// AddIdea rejects empty / over-long bodies with ErrInvalidIdea and writes no row.
func TestAddIdeaValidation(t *testing.T) {
	db, ctx := openIdeaStore(t)

	if _, err := db.AddIdea(ctx, "   ", ""); !errors.Is(err, ErrInvalidIdea) {
		t.Errorf("empty body err = %v, want ErrInvalidIdea", err)
	}
	if _, err := db.AddIdea(ctx, strings.Repeat("x", MaxIdeaBody+1), ""); !errors.Is(err, ErrInvalidIdea) {
		t.Errorf("long body err = %v, want ErrInvalidIdea", err)
	}
	if got := liveBodies(t, db, ctx); len(got) != 0 {
		t.Errorf("invalid adds wrote %d rows, want 0", len(got))
	}
}

// AddIdea honours an explicit palette colour, and falls back to a random palette
// colour for an empty or unknown one (so a note is never colourless).
func TestAddIdeaColor(t *testing.T) {
	db, ctx := openIdeaStore(t)

	picked, err := db.AddIdea(ctx, "chosen", "violet")
	if err != nil {
		t.Fatalf("AddIdea: %v", err)
	}
	if picked.Color != "violet" {
		t.Errorf("explicit colour = %q, want violet", picked.Color)
	}
	fallback, err := db.AddIdea(ctx, "random", "not-a-colour")
	if err != nil {
		t.Fatalf("AddIdea: %v", err)
	}
	if !validColor(fallback.Color) {
		t.Errorf("unknown colour fell back to %q, not a palette colour", fallback.Color)
	}
}

// An unknown colour on edit is ignored (the current colour is kept) rather than
// blanking the note; an empty body is still rejected.
func TestUpdateIdeaColorGuard(t *testing.T) {
	db, ctx := openIdeaStore(t)

	idea, err := db.AddIdea(ctx, "pick a colour for me", "")
	if err != nil {
		t.Fatalf("AddIdea: %v", err)
	}

	kept, err := db.UpdateIdea(ctx, idea.ID, "still here", "not-a-colour")
	if err != nil {
		t.Fatalf("UpdateIdea: %v", err)
	}
	if kept.Color != idea.Color {
		t.Errorf("unknown colour changed it to %q, want kept %q", kept.Color, idea.Color)
	}
	if _, err := db.UpdateIdea(ctx, idea.ID, "  ", "violet"); !errors.Is(err, ErrInvalidIdea) {
		t.Errorf("empty body on edit err = %v, want ErrInvalidIdea", err)
	}
}

// Editing or deleting a missing (or already-deleted) note is ErrIdeaNotFound.
func TestIdeaNotFound(t *testing.T) {
	db, ctx := openIdeaStore(t)
	if _, err := db.UpdateIdea(ctx, 999, "x", "amber"); !errors.Is(err, ErrIdeaNotFound) {
		t.Errorf("update missing err = %v, want ErrIdeaNotFound", err)
	}
	if err := db.DeleteIdea(ctx, 999); !errors.Is(err, ErrIdeaNotFound) {
		t.Errorf("delete missing err = %v, want ErrIdeaNotFound", err)
	}

	idea, _ := db.AddIdea(ctx, "delete me twice", "")
	if err := db.DeleteIdea(ctx, idea.ID); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	if err := db.DeleteIdea(ctx, idea.ID); !errors.Is(err, ErrIdeaNotFound) {
		t.Errorf("second delete err = %v, want ErrIdeaNotFound", err)
	}
}
