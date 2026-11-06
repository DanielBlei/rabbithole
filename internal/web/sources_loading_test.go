// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"os"
	"strings"
	"testing"

	"github.com/DanielBlei/rabbithole/internal/config"
)

// Favicon images load lazily, so a long list does not fetch every icon at once.
func TestFaviconImagesAreLazy(t *testing.T) {
	src, err := os.ReadFile("templates/partials/favicon.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), `<img class="fav__i"`) || !strings.Contains(string(src), `loading="lazy"`) {
		t.Errorf("favicon.html has no lazy image: %s", src)
	}
}

// failingMix seeds a failing, a flaky, a healthy, a never-fetched and a
// disabled source whose history would read as failing were it on.
func failingMix(t *testing.T) *Web {
	t.Helper()
	off := false
	feeds := []config.Feed{
		{Name: "Failing", URL: "https://failing.test/feed"},
		{Name: "Flaky", URL: "https://flaky.test/feed"},
		{Name: "Healthy", URL: "https://healthy.test/feed"},
		{Name: "Never", URL: "https://never.test/feed"},
		{Name: "Parked", URL: "https://parked.test/feed", Enabled: &off},
	}
	w, db := sourcesTestWeb(t, config.FeedsDoc{Feeds: feeds})
	recordFetches(t, db, feeds[0], 1, 3)
	recordFetches(t, db, feeds[1], 2, 1)
	recordFetches(t, db, feeds[2], 3, 0)
	recordFetches(t, db, feeds[4], 0, 5)
	return w
}

// Pages that draw no source rows count the same as the row-building read.
func TestNavCountsMatchTheFullRead(t *testing.T) {
	w := failingMix(t)
	full, err := w.sourcesData(t.Context(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	lite, err := w.sourcesNavData(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if full.Counts.Failing != 1 || full.Counts.Enabled != 4 || full.Counts.Total != 5 {
		t.Fatalf("seed counts = %+v, want 1 failing of 4 enabled, 5 in all", full.Counts)
	}
	if lite.Counts.Failing != full.Counts.Failing || lite.Counts.Enabled != full.Counts.Enabled ||
		lite.Counts.Total != full.Counts.Total {
		t.Errorf("nav counts = %+v, want %+v", lite.Counts, full.Counts)
	}
	if n, err := w.failingCount(t.Context()); err != nil || n != full.Counts.Failing {
		t.Errorf("failingCount = %d, %v, want %d", n, err, full.Counts.Failing)
	}
	if (lite.last == nil) != (full.last == nil) {
		t.Errorf("last run = %v, want %v", lite.last, full.last)
	}
}

// Library and Ingestion show the same Manage badge as Manage does.
func TestBadgeOnLibraryAndIngestionPages(t *testing.T) {
	w := failingMix(t)
	for _, path := range []string{"/sources/manage", "/sources/library", "/ingestion"} {
		out := get(t, w, path)
		wantAttrs(
			t,
			path+" badge",
			tagWith(t, out, "title", "1 failing"),
			map[string]string{"class": "sh-ni__n sh-ni__n--bad"},
		)
	}
}

// The Ingestion bar counts the enabled sources the way the live refresh does.
func TestIngestionBarEnabledCountMatchesLive(t *testing.T) {
	w := failingMix(t)
	data, err := w.sourcesNavData(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	got := w.ingestBarKnown(data.Counts.Enabled, data.last, false, false)
	want, err := w.ingestBarView(t.Context(), false, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Enabled != 4 || got != want {
		t.Errorf("bar from the page read = %+v, want %+v", got, want)
	}
}
