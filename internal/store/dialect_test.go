// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// dialect.go is the translator: it owns the database handle, and its rebind
// carries '?' as a Go rune. Both checks below skip it. Open and initSchema
// work on a raw *sql.DB before a Store exists, which no pattern here matches.
var exemptFromSeamChecks = map[string]bool{"dialect.go": true}

var rawAccess = regexp.MustCompile(`\b(?:s\.db|tx)\.(?:Query|QueryRow|Exec|Prepare)Context\(`)

// TestNoDirectDatabaseAccess fails when a query bypasses the wrappers in
// dialect.go. A bypassed statement keeps its `?` placeholders and so works on
// SQLite and fails on Postgres, which is exactly the bug a reviewer cannot see.
func TestNoDirectDatabaseAccess(t *testing.T) {
	for _, file := range packageFiles(t) {
		if exemptFromSeamChecks[filepath.Base(file)] {
			continue
		}
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if rawAccess.MatchString(line) {
				t.Errorf("%s:%d reaches the database directly; use s.query/s.queryRow/s.exec or the tx wrappers\n\t%s",
					filepath.Base(file), i+1, strings.TrimSpace(line))
			}
		}
	}
}

// TestNoLiteralQuestionMarkInSQL guards the assumption rebind rests on: every
// `?` in a query is a placeholder, so renumbering them left to right is safe.
// A `?` inside a string literal would break that, and it is how a jsonb column
// would arrive, since `?` is an operator there.
func TestNoLiteralQuestionMarkInSQL(t *testing.T) {
	for _, file := range packageFiles(t) {
		if exemptFromSeamChecks[filepath.Base(file)] {
			continue
		}
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "//") {
				continue
			}
			if questionMarkInsideQuotes(line) {
				t.Errorf("%s:%d has a ? inside a SQL string literal, which rebind would renumber\n\t%s",
					filepath.Base(file), i+1, strings.TrimSpace(line))
			}
		}
	}
}

// questionMarkInsideQuotes reports whether a ? falls between SQL single
// quotes. A line mixing literals and placeholders, as the tag filter does, has
// every ? outside them.
func questionMarkInsideQuotes(line string) bool {
	inString := false
	for _, r := range line {
		switch {
		case r == '\'':
			inString = !inString
		case r == '?' && inString:
			return true
		}
	}
	return false
}

// packageFiles lists the package's non-test Go sources.
func packageFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var files []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
			files = append(files, name)
		}
	}
	if len(files) == 0 {
		t.Fatal("no package sources found")
	}
	return files
}
