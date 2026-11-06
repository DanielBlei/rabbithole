// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package library

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// libraryDir is the checked-in library, relative to this package.
func libraryDir() string {
	return filepath.Join("..", "..", "library")
}

// The checked-in files must parse with no findings, and hold both ingestion types.
func TestLibraryFiles(t *testing.T) {
	lib, diags := ParseFS(os.DirFS(libraryDir()))
	for _, d := range diags {
		t.Error(d.String())
	}

	types := map[Type]bool{}
	for _, f := range lib.Files {
		types[f.Type] = true
		if len(f.Groups) == 0 {
			t.Errorf("%s: no groups", f.Name)
		}
		for _, g := range f.Groups {
			if len(g.Sources) == 0 {
				t.Errorf("%s: group %q has no sources", f.Name, g.Name)
			}
		}
	}
	for _, want := range []Type{TypeRSS, TypeAcademic} {
		if !types[want] {
			t.Errorf("no %s file parsed; is the library directory missing?", want)
		}
	}
}

// dashLines returns a finding for each em or en dash in data.
func dashLines(data []byte) []string {
	var out []string
	for i, line := range strings.Split(string(data), "\n") {
		for _, r := range []rune{'\u2013', '\u2014'} {
			if strings.ContainsRune(line, r) {
				out = append(out, fmt.Sprintf("line %d: U+%04X", i+1, r))
			}
		}
	}
	return out
}

func TestDashLines(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{name: "clean", in: "plain - hyphen text\nsecond line\n"},
		{name: "em dash", in: "ok\nbad \u2014 here\n", want: []string{"line 2: U+2014"}},
		{name: "en dash", in: "1\u20132\n", want: []string{"line 1: U+2013"}},
		{name: "both on one line", in: "a \u2013 b \u2014 c", want: []string{"line 1: U+2013", "line 1: U+2014"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := dashLines([]byte(tc.in)); !slices.Equal(got, tc.want) {
				t.Fatalf("dashLines() = %q, want %q", got, tc.want)
			}
		})
	}
}

// Library files take no em or en dashes (README rule 10).
func TestLibraryNoDashes(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(libraryDir(), "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no yaml files found in the library directory")
	}
	files = append(files, filepath.Join(libraryDir(), "README.md"))

	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, finding := range dashLines(data) {
			t.Errorf("%s: %s", path, finding)
		}
	}
}
