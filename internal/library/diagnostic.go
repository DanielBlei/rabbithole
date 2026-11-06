// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package library

import (
	"cmp"
	"errors"
	"slices"
	"strconv"
	"strings"
)

// CodeNewerSchema marks a file whose schema_version is newer than this package reads.
const CodeNewerSchema = "newer_schema"

// Diagnostic is one finding about the library files.
type Diagnostic struct {
	File    string // file name, e.g. "rss.yaml"
	Line    int    // 1-based, 0 when unknown
	Path    string // "group" or "group/source name", empty at file level
	Message string
	Code    string // machine-readable kind for findings callers act on, empty otherwise
}

// String formats the finding as "file:line: path: message", omitting the line when 0, the path when empty and
// the location prefix when File is empty.
func (d Diagnostic) String() string {
	var b strings.Builder
	if d.File != "" {
		b.WriteString(d.File)
		if d.Line != 0 {
			b.WriteString(":" + strconv.Itoa(d.Line))
		}
		b.WriteString(": ")
	}
	if d.Path != "" {
		b.WriteString(d.Path + ": ")
	}
	b.WriteString(d.Message)
	return b.String()
}

// Diagnostics is every finding from one parse, sorted by file then line.
type Diagnostics []Diagnostic

// Err joins every finding into one error, nil when there are none.
func (d Diagnostics) Err() error {
	errs := make([]error, 0, len(d))
	for _, diag := range d {
		errs = append(errs, errors.New(diag.String()))
	}
	return errors.Join(errs...)
}

// sort orders findings by file, then line, keeping the order of equal keys.
func (d Diagnostics) sort() {
	slices.SortStableFunc(d, func(a, b Diagnostic) int {
		if c := cmp.Compare(a.File, b.File); c != 0 {
			return c
		}
		return cmp.Compare(a.Line, b.Line)
	})
}
