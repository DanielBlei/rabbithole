// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

// Package articletext prepares already-extracted article text for bounded
// downstream use. Article text remains untrusted content; this package does
// not sanitize instructions embedded in it.
package articletext

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const wordsPerMinute = 200

// Prepared is normalized article text and metadata about the complete input.
// Truncated reports that Text contains only part of the normalized input.
type Prepared struct {
	Text           string
	Truncated      bool
	WordCount      int
	ReadingMinutes int
}

// Prepare normalizes line endings and outer whitespace, then limits text to
// maxBytes. The limit counts UTF-8 bytes, not characters or model tokens.
// Internal newlines and article structure are preserved.
func Prepare(text string, maxBytes int) (Prepared, error) {
	if maxBytes <= 0 {
		return Prepared{}, fmt.Errorf("maxBytes must be greater than zero")
	}
	if !utf8.ValidString(text) {
		return Prepared{}, fmt.Errorf("article text is not valid UTF-8")
	}

	normalized := strings.ReplaceAll(text, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")
	normalized = strings.TrimSpace(normalized)
	words := countWords(normalized)
	prepared := Prepared{
		Text:           normalized,
		WordCount:      words,
		ReadingMinutes: readingMinutes(words),
	}
	if len(normalized) <= maxBytes {
		return prepared, nil
	}

	end := runePrefixEnd(normalized, maxBytes)
	if end == 0 {
		return Prepared{}, fmt.Errorf("maxBytes %d is too small to fit the first UTF-8 rune", maxBytes)
	}
	if boundary := preferredBoundary(normalized, end); boundary >= 0 {
		end = boundary
	}
	prepared.Text = strings.TrimRightFunc(normalized[:end], unicode.IsSpace)
	prepared.Truncated = true
	return prepared, nil
}

// preferredBoundary prefers a paragraph break, then a line break. A boundary
// is used only when it retains at least three quarters of the available
// prefix; otherwise a rune-safe prefix preserves more usable text.
func preferredBoundary(text string, end int) int {
	minimum := end - end/4
	prefix := text[:end]
	if at := strings.LastIndex(prefix, "\n\n"); at >= 0 && usefulPrefix(text[:at], minimum) {
		return at
	}
	if at := strings.LastIndexByte(prefix, '\n'); at >= 0 && usefulPrefix(text[:at], minimum) {
		return at
	}
	return -1
}

func usefulPrefix(text string, minimum int) bool {
	return len(strings.TrimRightFunc(text, unicode.IsSpace)) >= minimum
}

// runePrefixEnd returns the greatest byte offset no larger than maxBytes that
// does not split a rune. text has already been validated as UTF-8.
func runePrefixEnd(text string, maxBytes int) int {
	end := maxBytes
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return end
}

// countWords estimates words by counting whitespace-delimited fields, while
// treating each Han, Hiragana, or Katakana rune as one word unit. This keeps
// whitespace-delimited text close to ordinary word counts and avoids treating
// an entire CJK passage as one word. Reading time remains an approximation.
func countWords(text string) int {
	count := 0
	inField := false
	for _, r := range text {
		if isCJK(r) {
			count++
			inField = false
			continue
		}
		if unicode.IsSpace(r) {
			inField = false
			continue
		}
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			if !inField {
				count++
				inField = true
			}
		} else {
			inField = false
		}
	}
	return count
}

func isCJK(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana)
}

func readingMinutes(words int) int {
	if words == 0 {
		return 0
	}
	minutes := (words-1)/wordsPerMinute + 1
	if minutes < 1 {
		return 1
	}
	return minutes
}
