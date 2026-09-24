// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

// Package profile owns interest-profile text, including the immutable built-in
// default and the canonical format produced by the friendly web editor.
package profile

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const (
	// DefaultID is the stable reserved identity of the application-owned
	// profile. Store implementations must never use it for a mutable row.
	DefaultID = "builtin-default"
	// DefaultName is the human-readable name of the built-in profile.
	DefaultName = "Default"

	// MaxNameBytes and MaxContentBytes bound profile input at both the HTTP and
	// store layers. They are intentionally generous for detailed profiles.
	MaxNameBytes    = 120
	MaxContentBytes = 128 * 1024
)

// DefaultContent is the semantic content formerly shipped only as
// configs/prompts/profile.example.md. Keeping it in Go makes a fresh install
// independent of a writable runtime profile file.
const DefaultContent = `# Reading interest profile

I'm an AI enthusiast. Score articles by how interesting they'd be to me, and for keeping up
with where things are going.

## Interested
- Running open-source language models, ideally on my own machine
- Practical write-ups: how something was built, what worked, what didn't
- Kubernetes and cloud-native infrastructure
- New models and tools worth trying out
- The systems work underneath inference: kernels, quantisation, memory, schedulers

## Somewhat interested
- Machine learning research with a clear practical angle
- Developer tooling
- Cost/performance trade-off analyses

## Not so interested
- Beginner tutorials and "what is an LLM" explainers
- Anything outside the lists above, however well written

## Not for me
- Marketing/press releases with no technical details
- Listicles and clickbait ("X will change everything")
- Business and strategy takes with no technical content

Prefer depth and substance over popularity. A beginner tutorial stays a beginner tutorial
even on a topic I like.`

var (
	htmlComment = regexp.MustCompile(`(?s)<!--.*?-->`)
	validID     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

	// ErrInvalidName, ErrInvalidContent and ErrInvalidID let HTTP callers map
	// validation failures to 400 without parsing error strings.
	ErrInvalidName    = errors.New("invalid profile name")
	ErrInvalidContent = errors.New("invalid profile content")
	ErrInvalidID      = errors.New("invalid profile id")
)

// Snapshot is the immutable profile identity and exact text used by one
// scoring run. Hash fingerprints Content so edits under the same ID remain
// distinguishable in score provenance.
type Snapshot struct {
	ID      string
	Name    string
	Content string
	Hash    string
	Builtin bool
}

// Default returns a fresh value for the immutable built-in profile.
func Default() Snapshot {
	return NewSnapshot(DefaultID, DefaultName, DefaultContent, true)
}

// NewSnapshot constructs an immutable scoring snapshot.
func NewSnapshot(id, name, content string, builtin bool) Snapshot {
	content = Clean(content)
	return Snapshot{
		ID: id, Name: name, Content: content, Hash: ContentHash(content), Builtin: builtin,
	}
}

// ContentHash returns a stable SHA-256 fingerprint of the exact profile text.
func ContentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// Clean strips HTML comments exactly as legacy profile loading historically
// did, then trims outer whitespace. Comments are notes to the user, not model
// input.
func Clean(content string) string {
	return strings.TrimSpace(htmlComment.ReplaceAllString(content, ""))
}

// ValidateID checks the storage-safe local profile identity.
func ValidateID(id string) error {
	if !validID.MatchString(id) {
		return fmt.Errorf("%w: must use letters, numbers, '.', '_' or '-'", ErrInvalidID)
	}
	if id == DefaultID {
		return fmt.Errorf("%w: %q is reserved for the built-in profile", ErrInvalidID, id)
	}
	return nil
}

// ValidateName normalizes and validates a profile's display name.
func ValidateName(name string) (string, error) {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return "", fmt.Errorf("%w: cannot be blank", ErrInvalidName)
	case len(name) > MaxNameBytes:
		return "", fmt.Errorf("%w: must be at most %d bytes", ErrInvalidName, MaxNameBytes)
	default:
		return name, nil
	}
}

// ValidateContent validates stored profile Markdown while preserving it for the
// raw editor. Clean is used only to decide whether anything semantic remains;
// scoring snapshots strip comments before reaching a model.
func ValidateContent(content string) (string, error) {
	if len(content) > MaxContentBytes {
		return "", fmt.Errorf("%w: must be at most %d bytes", ErrInvalidContent, MaxContentBytes)
	}
	content = strings.TrimSpace(content)
	if Clean(content) == "" {
		return "", fmt.Errorf("%w: cannot be empty or comments only", ErrInvalidContent)
	}
	return content, nil
}

// EditorFields are the three friendly fields rendered by the profile editor.
type EditorFields struct {
	Interested string
	Less       string
	Context    string
}

const (
	canonicalTitle    = "# Reading interest profile"
	interestedHeading = "## I'm interested in"
	lessHeading       = "## I'm less interested in"
	contextHeading    = "## Additional context"
)

// RenderCanonical deterministically turns the friendly editor fields into the
// free-form Markdown consumed by scorers.
func RenderCanonical(f EditorFields) string {
	return strings.Join([]string{
		canonicalTitle,
		"",
		interestedHeading,
		strings.TrimSpace(f.Interested),
		"",
		lessHeading,
		strings.TrimSpace(f.Less),
		"",
		contextHeading,
		strings.TrimSpace(f.Context),
	}, "\n")
}

// ParseCanonical recognizes only the exact format RenderCanonical emits.
// Arbitrary Markdown deliberately falls back to the raw editor rather than
// risking a lossy bidirectional conversion.
func ParseCanonical(content string) (EditorFields, bool) {
	content = strings.TrimSpace(content)
	if !strings.HasPrefix(content, canonicalTitle+"\n") {
		return EditorFields{}, false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(content, canonicalTitle))
	parts := strings.Split(rest, "\n"+lessHeading+"\n")
	if len(parts) != 2 || !strings.HasPrefix(parts[0], interestedHeading+"\n") {
		return EditorFields{}, false
	}
	last := strings.Split(parts[1], "\n"+contextHeading+"\n")
	if len(last) != 2 {
		return EditorFields{}, false
	}
	return EditorFields{
		Interested: strings.TrimSpace(strings.TrimPrefix(parts[0], interestedHeading)),
		Less:       strings.TrimSpace(last[0]),
		Context:    strings.TrimSpace(last[1]),
	}, true
}
