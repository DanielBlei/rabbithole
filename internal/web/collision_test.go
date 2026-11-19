// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/store"
)

func collisionWeb(t *testing.T) *Web {
	t.Helper()
	w := newTestWeb(t)
	items := []feeds.Item{
		{ID: "alias-item", Source: "S1", Title: "A", Link: "x"},
		{ID: "x", Source: "S1", Title: "B", Link: "https://example.com/x"},
	}
	if err := w.db.Record(context.Background(), items, nil, time.Now()); err != nil {
		t.Fatalf("Record: %v", err)
	}
	return w
}

func collisionRow(t *testing.T, w *Web, id string) store.ItemRow {
	t.Helper()
	row, err := w.db.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("GetByID(%s): %v", id, err)
	}
	return row
}

func assertCollisionLinkOwnerUnchanged(t *testing.T, w *Web) {
	t.Helper()
	a := collisionRow(t, w, "alias-item")
	if a.Status != store.StatusUnread || a.UserScore != nil || a.UserNote != nil || a.Bookmarked {
		t.Fatalf("link owner A changed: %+v", a)
	}
}

func TestCanonicalRowMutationsDoNotTouchCollidingLink(t *testing.T) {
	w := collisionWeb(t)

	postForm(t, w, "/items/x/seen", nil)
	b := collisionRow(t, w, "x")
	if b.Status != store.StatusRead {
		t.Fatalf("seen status = %q, want read", b.Status)
	}
	assertCollisionLinkOwnerUnchanged(t, w)

	postForm(t, w, "/items/x/hide", nil)
	b = collisionRow(t, w, "x")
	if b.Status != store.StatusSkipped {
		t.Fatalf("hide status = %q, want skipped", b.Status)
	}
	assertCollisionLinkOwnerUnchanged(t, w)

	postForm(t, w, "/items/x/hide", nil)
	b = collisionRow(t, w, "x")
	if b.Status != store.StatusUnread {
		t.Fatalf("unhide status = %q, want unread", b.Status)
	}
	assertCollisionLinkOwnerUnchanged(t, w)

	postForm(t, w, "/items/x/note", url.Values{"note": {"first note"}})
	b = collisionRow(t, w, "x")
	if b.UserNote == nil || *b.UserNote != "first note" {
		t.Fatalf("note = %v, want first note", b.UserNote)
	}
	assertCollisionLinkOwnerUnchanged(t, w)

	postForm(t, w, "/items/x/note", url.Values{"note": {"updated note"}})
	b = collisionRow(t, w, "x")
	if b.UserNote == nil || *b.UserNote != "updated note" {
		t.Fatalf("updated note = %v, want updated note", b.UserNote)
	}
	assertCollisionLinkOwnerUnchanged(t, w)

	postForm(t, w, "/items/x/bookmark", nil)
	b = collisionRow(t, w, "x")
	if !b.Bookmarked {
		t.Fatal("bookmark was not set")
	}
	assertCollisionLinkOwnerUnchanged(t, w)

	postForm(t, w, "/items/x/bookmark", nil)
	b = collisionRow(t, w, "x")
	if b.Bookmarked {
		t.Fatal("bookmark was not cleared")
	}
	assertCollisionLinkOwnerUnchanged(t, w)

	postForm(t, w, "/items/x/rate", url.Values{"score": {"7"}})
	b = collisionRow(t, w, "x")
	if b.UserScore == nil || *b.UserScore != 7 {
		t.Fatalf("rating = %v, want 7", b.UserScore)
	}
	assertCollisionLinkOwnerUnchanged(t, w)

	postForm(t, w, "/items/x/rate", url.Values{"score": {"7"}})
	b = collisionRow(t, w, "x")
	if b.UserScore != nil {
		t.Fatalf("re-posted rating = %v, want cleared", b.UserScore)
	}
	assertCollisionLinkOwnerUnchanged(t, w)
}
