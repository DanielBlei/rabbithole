// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/library"
)

// The library's academic URLs must be shapes the app reads, and no RSS URL
// may be an academic host.
func TestLibraryAcademicURLs(t *testing.T) {
	lib, diags := library.ParseFS(os.DirFS(filepath.Join("..", "..", "library")))
	if err := diags.Err(); err != nil {
		t.Fatal(err)
	}
	if len(lib.Files) == 0 {
		t.Fatal("library has no files")
	}

	for _, f := range lib.Files {
		for _, g := range f.Groups {
			for _, s := range g.Sources {
				src := feeds.Source{Name: s.Name, URL: s.URL}
				switch f.Type {
				case library.TypeAcademic:
					t.Run(s.Name, func(t *testing.T) {
						q, err := academicQueryFromSource(src)
						if err != nil {
							t.Errorf("%s group %q source %q (%s): %v",
								f.Name, g.Name, s.Name, s.URL, err)
							return
						}
						if q.Query == "" && q.ISSN == "" {
							t.Errorf("%s group %q source %q (%s): no query or ISSN",
								f.Name, g.Name, s.Name, s.URL)
						}
					})
				case library.TypeRSS:
					if _, err := academicQueryFromSource(src); err == nil {
						t.Errorf("%s group %q source %q (%s): RSS URL maps to an academic provider",
							f.Name, g.Name, s.Name, s.URL)
					}
				}
			}
		}
	}
}
