// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package config_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/library"
)

func exampleFile(t *testing.T, parts ...string) string {
	t.Helper()
	return filepath.Join(append([]string{"..", "..", "docs", "examples"}, parts...)...)
}

// The showcase is the file users copy: it must import cleanly, drop nothing,
// and its active values must be the defaults a bare run applies.
func TestConfigExampleMatchesDefaults(t *testing.T) {
	f, err := config.ReadFile(exampleFile(t, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if f.Store != (config.StoreConfig{}) || f.Profile != "" || f.Ingest.Feeds != "" {
		t.Errorf("example sets bootstrap or import keys: store=%+v profile=%q feeds=%q",
			f.Store, f.Profile, f.Ingest.Feeds)
	}
	got, _, _, dropped := f.Split()
	if len(dropped) != 0 {
		t.Errorf("example sets fields its provider ignores: %v", dropped)
	}
	if _, err := got.Prepare(); err != nil {
		t.Fatalf("example does not validate: %v", err)
	}

	want := config.RunDefaults()
	got.Inference.ModelTuning = got.Inference.ModelTuning.Normalize()
	want.Inference.ModelTuning = want.Inference.ModelTuning.Normalize()
	if got.Hash() != want.Hash() {
		t.Errorf("example values differ from the defaults:\n got %+v\nwant %+v", got, want)
	}
}

func TestOtherExamplesLoad(t *testing.T) {
	if _, found, err := config.ReadFeedsFile(exampleFile(t, "feeds.yaml")); err != nil || !found {
		t.Fatalf("feeds.yaml: found=%v err=%v", found, err)
	}
	if _, err := config.LoadProfileFile(exampleFile(t, "prompts", "profile.md")); err != nil {
		t.Errorf("prompts/profile.md: %v", err)
	}
	// The file the example's system_prompt line points at imports as text.
	f := config.File{}
	f.Inference.SystemPrompt.Text = exampleFile(t, "prompts", "system.md")
	if err := f.InlineSystemPrompt(""); err != nil || f.Inference.SystemPrompt.Text == "" {
		t.Errorf("prompts/system.md: %v", err)
	}
}

// libraryExampleParity compares the library with the example feeds. A source and a feed overlap when
// they share a name or a URL (case-insensitive); an overlap is a mismatch unless both match.
func libraryExampleParity(lib *library.Library, feeds []config.Feed) (mismatches []string, overlaps int) {
	for _, f := range lib.Files {
		for _, g := range f.Groups {
			for _, s := range g.Sources {
				for _, feed := range feeds {
					sameName := strings.EqualFold(strings.TrimSpace(s.Name), strings.TrimSpace(feed.Name))
					sameURL := strings.EqualFold(s.URL, feed.URL)
					if !sameName && !sameURL {
						continue
					}
					overlaps++
					switch {
					case sameName && s.URL != feed.URL:
						mismatches = append(mismatches,
							fmt.Sprintf("%s group %q source %q: library URL %q, example URL %q",
								f.Name, g.Name, s.Name, s.URL, feed.URL))
					case sameURL && strings.TrimSpace(s.Name) != strings.TrimSpace(feed.Name):
						mismatches = append(mismatches,
							fmt.Sprintf("%s group %q source %q (%s): example name %q",
								f.Name, g.Name, s.Name, s.URL, feed.Name))
					}
				}
			}
		}
	}
	return mismatches, overlaps
}

// A source shown in docs/examples/feeds.yaml keeps the library's name and URL
// (library/README.md rule 8).
func TestLibraryMatchesExampleFeeds(t *testing.T) {
	doc, found, err := config.ReadFeedsFile(exampleFile(t, "feeds.yaml"))
	if !found || err != nil {
		t.Fatalf("feeds.yaml: found=%v err=%v", found, err)
	}
	lib, diags := library.ParseFS(os.DirFS(filepath.Join("..", "..", "library")))
	if err := diags.Err(); err != nil {
		t.Fatal(err)
	}

	mismatches, overlaps := libraryExampleParity(lib, doc.Feeds)
	for _, m := range mismatches {
		t.Error(m)
	}
	if overlaps == 0 {
		t.Error("no library source overlaps docs/examples/feeds.yaml; the parity check is vacuous")
	}
}

func TestLibraryExampleParity(t *testing.T) {
	lib := &library.Library{Files: []library.File{{Name: "rss.yaml", Groups: []library.Group{{
		Name:    "blogs",
		Sources: []library.Source{{Name: "Go blog", URL: "https://go.dev/blog/feed.atom"}},
	}}}}}
	tests := []struct {
		name       string
		feed       config.Feed
		mismatches int
		overlaps   int
	}{
		{name: "name match, different URL",
			feed: config.Feed{Name: "go blog", URL: "https://example.org/other.xml"}, mismatches: 1, overlaps: 1},
		{name: "URL match in another case, different name",
			feed: config.Feed{Name: "Gophers", URL: "https://GO.dev/blog/feed.atom"}, mismatches: 1, overlaps: 1},
		{name: "exact match",
			feed: config.Feed{Name: "Go blog", URL: "https://go.dev/blog/feed.atom"}, overlaps: 1},
		{name: "no overlap",
			feed: config.Feed{Name: "Other blog", URL: "https://example.org/feed.xml"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mismatches, overlaps := libraryExampleParity(lib, []config.Feed{tc.feed})
			if len(mismatches) != tc.mismatches || overlaps != tc.overlaps {
				t.Fatalf("mismatches %q, overlaps %d, want %d mismatches and %d overlaps",
					mismatches, overlaps, tc.mismatches, tc.overlaps)
			}
		})
	}
}
