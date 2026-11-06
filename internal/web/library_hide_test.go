// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"net/url"
	"strings"
	"testing"

	"github.com/DanielBlei/rabbithole/internal/config"
)

func TestLibraryHideKeepsTheEntryInTheLibrary(t *testing.T) {
	w, db := libraryTestWeb(t, config.FeedsDoc{})
	out := postLibrary(t, w, "/sources/library/hide", url.Values{"pick": {libKey(libGoURL)}}, nil)

	if !strings.Contains(out, "Hid 1 source") {
		t.Errorf("banner missing; body=%s", out)
	}
	if !strings.Contains(libRow(t, out, libGoURL), `data-hidden="1"`) {
		t.Errorf("hidden row is not flagged")
	}
	// The View menu sits outside the list the reply repaints; a fresh page carries the count.
	if page := get(t, w, "/sources/library"); !strings.Contains(page, `<span class="stp-menu__n">Hidden (1)</span>`) {
		t.Errorf("Hidden view count missing from the page")
	}
	if hidden, err := db.HiddenLibrary(t.Context()); err != nil || !hidden[libKey(libGoURL)] {
		t.Errorf("HiddenLibrary = %v, %v; want the Go blog", hidden, err)
	}
	if got := libraryTotal(t, w); got == 0 {
		t.Errorf("hiding removed the entry from the remote library")
	}
}

func TestLibraryUnhideShowsTheEntryAgain(t *testing.T) {
	w, _ := libraryTestWeb(t, config.FeedsDoc{})
	postLibrary(t, w, "/sources/library/hide", url.Values{"pick": {libKey(libGoURL)}}, nil)

	out := postLibrary(t, w, "/sources/library/unhide", url.Values{"pick": {libKey(libGoURL)}}, nil)
	if !strings.Contains(out, "Unhid 1 source") {
		t.Errorf("banner missing; body=%s", out)
	}
	if !strings.Contains(libRow(t, out, libGoURL), `data-hidden="0"`) {
		t.Errorf("unhidden row is still flagged hidden")
	}
}

func TestLibraryHideChecksThePicks(t *testing.T) {
	w, db := libraryTestWeb(t, config.FeedsDoc{})
	out := postLibrary(t, w, "/sources/library/hide", url.Values{
		"pick": {"https://evil.example/feed.xml"},
	}, nil)

	if !strings.Contains(out, "Pick at least one source first.") {
		t.Errorf("unknown key not refused; body=%s", out)
	}
	if hidden, _ := db.HiddenLibrary(t.Context()); len(hidden) != 0 {
		t.Errorf("unknown key was stored: %v", hidden)
	}
}

func TestLibraryAllHiddenSectionSortsLast(t *testing.T) {
	w, _ := libraryTestWeb(t, config.FeedsDoc{})
	snap := w.library.Get(t.Context())
	entries := libraryEntries(snap)
	first := entries[0].Group.Name
	var picks []string
	for _, e := range entries {
		if e.Group.Name == first {
			picks = append(picks, libKey(e.Source.URL))
		}
	}
	out := postLibrary(t, w, "/sources/library/hide", url.Values{"pick": picks}, nil)

	last := strings.LastIndex(out, `data-lib-sec data-title="`)
	if last < 0 {
		t.Fatalf("no sections in the page")
	}
	if !strings.Contains(strings.SplitN(out[last:], ">", 2)[0], "data-lib-sec-gone") {
		t.Errorf("the section whose entries are all hidden is not last")
	}
	if !strings.Contains(out, "data-lib-show-hidden") {
		t.Errorf("hidden section has no count to open the Hidden view")
	}
}

func TestLibraryCoverageSkipsHiddenEntries(t *testing.T) {
	w, _ := libraryTestWeb(t, config.FeedsDoc{})
	snap := w.library.Get(t.Context())
	states, err := w.libraryStates(t.Context(), snap)
	if err != nil {
		t.Fatalf("libraryStates: %v", err)
	}
	before := libraryCoverage(snap, states, nil).Total
	after := libraryCoverage(snap, states, map[string]bool{libKey(libGoURL): true}).Total
	if after != before-1 {
		t.Errorf("coverage total = %d with one hidden, want %d", after, before-1)
	}
}
