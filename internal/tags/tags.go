// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

// Package tags normalizes and validates classifier tag slugs.
package tags

import (
	"errors"
	"fmt"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

var errInvalidSlug = errors.New("invalid tag slug")

// Normalize converts a raw label into a lowercase, hyphen-separated slug.
// Canonically equivalent Unicode labels produce the same slug through NFC.
// Marks are preserved after a letter or digit in the same segment. Other
// non-alphanumeric characters are separators; consequently, distinct labels
// such as "C++" and "C#" can normalize to the same slug.
// It returns an error when the label contains no letters or digits, or has a
// combining mark without a preceding letter or digit in its segment.
func Normalize(label string) (string, error) {
	label = norm.NFC.String(strings.ToLower(norm.NFC.String(strings.TrimSpace(label))))
	var b strings.Builder
	b.Grow(len(label))
	separator := false
	segmentHasBase := false
	for _, r := range label {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if separator && b.Len() > 0 {
				b.WriteByte('-')
			}
			b.WriteRune(r)
			separator = false
			segmentHasBase = true
			continue
		}
		if unicode.IsMark(r) {
			if !segmentHasBase {
				return "", fmt.Errorf("normalize tag %q: %w", label, errInvalidSlug)
			}
			b.WriteRune(r)
			continue
		}
		// Whitespace, punctuation, symbols, and other non-alphanumeric runes
		// are separators.
		separator = true
		segmentHasBase = false
	}
	slug := strings.Trim(b.String(), "-")
	if err := Validate(slug); err != nil {
		return "", fmt.Errorf("normalize tag %q: %w", label, err)
	}
	return slug, nil
}

// Validate reports whether slug is a canonical non-empty tag slug.
func Validate(slug string) error {
	if slug == "" || slug != strings.ToLower(slug) || !norm.NFC.IsNormalString(slug) || strings.HasPrefix(slug, "-") ||
		strings.HasSuffix(slug, "-") || strings.Contains(slug, "--") {
		return errInvalidSlug
	}
	segmentHasBase := false
	for _, r := range slug {
		switch {
		case r == '-':
			segmentHasBase = false
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if unicode.IsUpper(r) {
				return errInvalidSlug
			}
			segmentHasBase = true
		case unicode.IsMark(r) && segmentHasBase:
			// Keep subsequent marks attached to the same base sequence.
		default:
			return errInvalidSlug
		}
	}
	return nil
}

// Deduplicate normalizes both categories, preserving first occurrence order.
// A normalized tag present in keywords is removed from assumptions. Invalid
// labels return an error and no partial results.
func Deduplicate(keywords, assumptions []string) ([]string, []string, error) {
	keywordSlugs, keywordSet, err := normalizeUnique(keywords, "keyword")
	if err != nil {
		return nil, nil, err
	}
	assumptionSlugs, _, err := normalizeUnique(assumptions, "assumption")
	if err != nil {
		return nil, nil, err
	}
	filtered := make([]string, 0, len(assumptionSlugs))
	for _, slug := range assumptionSlugs {
		if _, exists := keywordSet[slug]; !exists {
			filtered = append(filtered, slug)
		}
	}
	return keywordSlugs, filtered, nil
}

func normalizeUnique(labels []string, category string) ([]string, map[string]struct{}, error) {
	out := make([]string, 0, len(labels))
	seen := make(map[string]struct{}, len(labels))
	for i, label := range labels {
		slug, err := Normalize(label)
		if err != nil {
			return nil, nil, fmt.Errorf("%s %d: %w", category, i, err)
		}
		if _, exists := seen[slug]; exists {
			continue
		}
		seen[slug] = struct{}{}
		out = append(out, slug)
	}
	return out, seen, nil
}
