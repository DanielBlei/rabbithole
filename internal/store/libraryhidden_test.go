// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import "testing"

func TestLibraryHideUnhide(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()

	if got, err := s.HiddenLibrary(ctx); err != nil || len(got) != 0 {
		t.Fatalf("hidden before any hide = %v, %v; want empty", got, err)
	}
	if err := s.HideLibrary(ctx, nil); err != nil {
		t.Fatalf("hide none: %v", err)
	}

	if err := s.HideLibrary(ctx, []string{"a", "b"}); err != nil {
		t.Fatalf("hide: %v", err)
	}
	// Hiding again keeps the row and does not fail.
	if err := s.HideLibrary(ctx, []string{"b", "c"}); err != nil {
		t.Fatalf("hide again: %v", err)
	}
	got, err := s.HiddenLibrary(ctx)
	if err != nil || len(got) != 3 || !got["a"] || !got["b"] || !got["c"] {
		t.Fatalf("hidden = %v, %v; want a, b, c", got, err)
	}

	if err := s.UnhideLibrary(ctx, []string{"a", "missing"}); err != nil {
		t.Fatalf("unhide: %v", err)
	}
	got, err = s.HiddenLibrary(ctx)
	if err != nil || len(got) != 2 || got["a"] || !got["b"] || !got["c"] {
		t.Fatalf("hidden after unhide = %v, %v; want b, c", got, err)
	}
}
