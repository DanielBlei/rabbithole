// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

// Package library holds the curated source library under library/: one YAML
// file per ingestion type, sources grouped for display.
package library

// SchemaVersion is the newest library file format this package reads.
const SchemaVersion = 1

// Type is the ingestion type, equal to the file stem.
type Type string

const (
	TypeRSS      Type = "rss"
	TypeAcademic Type = "academic"
)

// Library is every readable file, ordered rss then academic.
type Library struct {
	Files []File
}

// File is one library file.
type File struct {
	Name          string // file name, e.g. "rss.yaml"
	SchemaVersion int
	Type          Type
	Description   string
	Groups        []Group
}

// Group is an ordered set of sources; file order is display order.
type Group struct {
	Name        string
	Description string
	Sources     []Source
}

// Source is one curated source. URL is its identity.
type Source struct {
	Name        string
	URL         string
	Description string
	Labels      []string // display form; each label's slug is tags.Normalize(label)
	MaxItems    int      // 0 when unset
}

// typeOrder ranks known types for Library.Files ordering; ok is false for an unknown stem.
func typeOrder(t Type) (rank int, ok bool) {
	switch t {
	case TypeRSS:
		return 0, true
	case TypeAcademic:
		return 1, true
	}
	return 0, false
}
