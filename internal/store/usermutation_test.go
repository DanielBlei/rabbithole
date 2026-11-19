// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/feeds"
)

func openCollisionStore(t *testing.T) *Store {
	t.Helper()
	s := openTestStore(t)
	items := []feeds.Item{
		{ID: "alias-item", Source: "S", Title: "A", Link: "x"},
		{ID: "x", Source: "S", Title: "B", Link: "https://example.com/x"},
	}
	if err := s.Record(context.Background(), items, nil, time.Now()); err != nil {
		t.Fatalf("Record: %v", err)
	}
	return s
}

func mustGetByID(t *testing.T, s *Store, id string) ItemRow {
	t.Helper()
	row, err := s.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("GetByID(%s): %v", id, err)
	}
	return row
}

func TestUpdateUserStateCollisionUpdatesOneItem(t *testing.T) {
	unread := StatusUnread
	read := StatusRead
	baselineScore, score := 5, 7
	baselineNote, note := "baseline note", "new note"
	no, yes := false, true

	cases := []struct {
		name  string
		patch UserPatch
		check func(ItemRow) bool
	}{
		{"status", UserPatch{Status: &read}, func(r ItemRow) bool { return r.Status == StatusRead }},
		{"score", UserPatch{UserScore: &score}, func(r ItemRow) bool {
			return r.UserScore != nil && *r.UserScore == score
		}},
		{"clear score", UserPatch{ClearUserScore: true}, func(r ItemRow) bool { return r.UserScore == nil }},
		{"note", UserPatch{UserNote: &note}, func(r ItemRow) bool {
			return r.UserNote != nil && *r.UserNote == note
		}},
		{"bookmark", UserPatch{Bookmarked: &yes}, func(r ItemRow) bool { return r.Bookmarked }},
	}
	seed := UserPatch{
		Status: &unread, UserScore: &baselineScore, UserNote: &baselineNote, Bookmarked: &no,
	}
	checkBaseline := func(r ItemRow) bool {
		return r.Status == StatusUnread &&
			r.UserScore != nil && *r.UserScore == baselineScore &&
			r.UserNote != nil && *r.UserNote == baselineNote &&
			!r.Bookmarked
	}

	for _, tc := range cases {
		t.Run(tc.name+" public", func(t *testing.T) {
			s := openCollisionStore(t)
			if err := s.UpdateUserStateByID(t.Context(), "alias-item", seed); err != nil {
				t.Fatalf("seed A: %v", err)
			}
			if err := s.UpdateUserStateByID(t.Context(), "x", seed); err != nil {
				t.Fatalf("seed B: %v", err)
			}
			if err := s.UpdateUserState(t.Context(), "x", tc.patch); err != nil {
				t.Fatalf("UpdateUserState: %v", err)
			}
			if a := mustGetByID(t, s, "alias-item"); !tc.check(a) {
				t.Fatalf("A was not updated as expected: %+v", a)
			}
			if b := mustGetByID(t, s, "x"); !checkBaseline(b) {
				t.Fatalf("B changed through the public alias lookup: %+v", b)
			}
		})

		t.Run(tc.name+" by id", func(t *testing.T) {
			s := openCollisionStore(t)
			if err := s.UpdateUserStateByID(t.Context(), "alias-item", seed); err != nil {
				t.Fatalf("seed A: %v", err)
			}
			if err := s.UpdateUserStateByID(t.Context(), "x", seed); err != nil {
				t.Fatalf("seed B: %v", err)
			}
			if err := s.UpdateUserStateByID(t.Context(), "x", tc.patch); err != nil {
				t.Fatalf("UpdateUserStateByID: %v", err)
			}
			if b := mustGetByID(t, s, "x"); !tc.check(b) {
				t.Fatalf("B was not updated as expected: %+v", b)
			}
			if a := mustGetByID(t, s, "alias-item"); !checkBaseline(a) {
				t.Fatalf("A changed through the exact-ID mutation: %+v", a)
			}
		})
	}
}

func TestUpdateUserStateMissingItem(t *testing.T) {
	s := openCollisionStore(t)
	if err := s.UpdateUserState(t.Context(), "missing", UserPatch{}); !errors.Is(err, ErrItemNotFound) {
		t.Fatalf("UpdateUserState missing err = %v, want ErrItemNotFound", err)
	}
	if err := s.UpdateUserStateByID(t.Context(), "missing", UserPatch{}); !errors.Is(err, ErrItemNotFound) {
		t.Fatalf("UpdateUserStateByID missing err = %v, want ErrItemNotFound", err)
	}
}

func TestUpdateUserStateAfterDeletion(t *testing.T) {
	s := openCollisionStore(t)
	if _, err := s.GetByID(t.Context(), "x"); err != nil {
		t.Fatalf("GetByID before deletion: %v", err)
	}
	if _, err := s.PruneItems(t.Context(), PruneFilter{All: true}); err != nil {
		t.Fatalf("PruneItems: %v", err)
	}
	if err := s.UpdateUserState(t.Context(), "x", UserPatch{}); !errors.Is(err, ErrItemNotFound) {
		t.Fatalf("public update after deletion err = %v, want ErrItemNotFound", err)
	}
	if err := s.UpdateUserStateByID(t.Context(), "x", UserPatch{}); !errors.Is(err, ErrItemNotFound) {
		t.Fatalf("exact update after deletion err = %v, want ErrItemNotFound", err)
	}
}

func TestConcurrentCollisionMutationsStayIndependent(t *testing.T) {
	s := openCollisionStore(t)
	read := StatusRead
	yes := true
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := s.UpdateUserState(context.Background(), "x", UserPatch{Status: &read}); err != nil {
			t.Errorf("UpdateUserState: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := s.UpdateUserStateByID(context.Background(), "x", UserPatch{Bookmarked: &yes}); err != nil {
			t.Errorf("UpdateUserStateByID: %v", err)
		}
	}()
	wg.Wait()

	a := mustGetByID(t, s, "alias-item")
	if a.Status != StatusRead || a.Bookmarked {
		t.Fatalf("A state = %+v, want read and not bookmarked", a)
	}
	b := mustGetByID(t, s, "x")
	if b.Bookmarked != true || b.Status != StatusUnread {
		t.Fatalf("B state = %+v, want bookmarked and unread", b)
	}
	// Duplicate public requests remain one-row updates.
	if err := s.UpdateUserState(t.Context(), "x", UserPatch{Status: &read}); err != nil {
		t.Fatalf("duplicate UpdateUserState: %v", err)
	}
	if b := mustGetByID(t, s, "x"); b.Status != StatusUnread {
		t.Fatalf("duplicate public update touched B: %+v", b)
	}
}
