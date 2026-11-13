// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package classify

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/DanielBlei/rabbithole/internal/feeds"
	tagspkg "github.com/DanielBlei/rabbithole/internal/tags"
)

// ParseClassifications reads the model's JSON verdict and maps indices back to
// items. Validation is deliberately strict: every item needs one entry, every
// label must normalize, and a structural failure fails the whole response so
// the caller can decompose the batch and isolate the item that caused it.
//
// Over-cap lists are trimmed (a verbose answer is not malformed), duplicates
// are removed, and a label present in both lists stays a keyword.
func ParseClassifications(raw string, items []feeds.Item) ([]ItemVerdict, error) {
	if len(items) == 0 {
		return nil, fmt.Errorf("classification needs at least one item")
	}
	jsonStr, err := extractJSONObject(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: raw=%q", err, truncate(raw, 200))
	}
	parsed, err := unmarshalClassifications(jsonStr)
	if err != nil {
		return nil, fmt.Errorf("parse classifications json: %w: raw=%q", err, truncate(jsonStr, 200))
	}
	if len(parsed.Classifications) == 0 {
		return nil, fmt.Errorf("no classifications in response: raw=%q", truncate(jsonStr, 200))
	}

	out := make([]ItemVerdict, len(items))
	seen := make([]bool, len(items))
	for _, entry := range parsed.Classifications {
		idx := entry.Index - 1
		if len(items) == 1 {
			idx = 0
		}
		if idx < 0 || idx >= len(items) {
			return nil, fmt.Errorf("classification index %d out of range 1-%d", entry.Index, len(items))
		}
		if seen[idx] {
			return nil, fmt.Errorf("duplicate classification for item %d", idx+1)
		}
		seen[idx] = true

		keywords, assumptions, err := cleanLabels(entry.Keywords, entry.Assumptions)
		if err != nil {
			return nil, fmt.Errorf("item %d: %w", idx+1, err)
		}
		out[idx] = ItemVerdict{
			Index:       idx + 1,
			ID:          items[idx].ID,
			Keywords:    keywords,
			Assumptions: assumptions,
		}
	}
	for i, ok := range seen {
		if !ok {
			return nil, fmt.Errorf("missing classification for item %d", i+1)
		}
	}
	return out, nil
}

// cleanLabels validates, deduplicates and trims one verdict. Keywords are
// collected first so the same slug is removed from assumptions.
func cleanLabels(keywords, assumptions []string) ([]string, []string, error) {
	keywordSlugs := make(map[string]struct{}, len(keywords))
	kw, err := collectLabels(keywords, MaxKeywords, keywordSlugs)
	if err != nil {
		return nil, nil, err
	}
	as, err := collectLabels(assumptions, MaxAssumptions, keywordSlugs)
	if err != nil {
		return nil, nil, err
	}
	return kw, as, nil
}

func collectLabels(labels []string, max int, seen map[string]struct{}) ([]string, error) {
	out := make([]string, 0, min(len(labels), max))
	for _, raw := range labels {
		label := strings.TrimSpace(raw)
		if label == "" {
			return nil, fmt.Errorf("empty label")
		}
		if utf8.RuneCountInString(label) > MaxLabelLength {
			return nil, fmt.Errorf("label %q exceeds %d characters", label, MaxLabelLength)
		}
		slug, err := tagspkg.Normalize(label)
		if err != nil {
			return nil, fmt.Errorf("label %q: %w", label, err)
		}
		if utf8.RuneCountInString(slug) > MaxLabelLength {
			return nil, fmt.Errorf("label %q normalizes to %q, exceeding %d characters",
				label, slug, MaxLabelLength)
		}
		if _, duplicate := seen[slug]; duplicate {
			continue
		}
		seen[slug] = struct{}{}
		if len(out) >= max {
			break
		}
		out = append(out, label)
	}
	return out, nil
}

type rawClassifications struct {
	Classifications []classificationEntry `json:"classifications"`
}

type classificationEntry struct {
	Index       int
	Keywords    []string
	Assumptions []string
}

// UnmarshalJSON reads an entry leniently for models that ignore the schema —
// lenient about spelling and list shapes, strict about presence: every entry
// needs a usable index and both label lists. A missing or malformed field is an
// error, never a silent empty verdict, so the worker retries the item instead
// of recording an empty classification it would never revisit. An explicitly
// supplied empty array stays valid: "no keywords" is an answer.
func (e *classificationEntry) UnmarshalJSON(b []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		return err
	}
	var haveIndex, haveKeywords, haveAssumptions bool
	for k, v := range fields {
		switch normalizeKey(k) {
		case "index":
			n, ok := asNumber(v)
			if !ok {
				return fmt.Errorf("index is not a number: %s", truncate(string(v), 40))
			}
			e.Index = int(math.Round(n))
			haveIndex = true
		case "keywords":
			list, ok := asStringList(v)
			if !ok {
				return fmt.Errorf("keywords is not a list of labels: %s", truncate(string(v), 40))
			}
			e.Keywords = list
			haveKeywords = true
		case "assumptions":
			list, ok := asStringList(v)
			if !ok {
				return fmt.Errorf("assumptions is not a list of labels: %s", truncate(string(v), 40))
			}
			e.Assumptions = list
			haveAssumptions = true
		}
	}
	if !haveIndex {
		return fmt.Errorf("classification entry is missing index")
	}
	if !haveKeywords {
		return fmt.Errorf("classification entry is missing keywords")
	}
	if !haveAssumptions {
		return fmt.Errorf("classification entry is missing assumptions")
	}
	return nil
}

// normalizeKey reduces "Keywords", "keywords:" and " keywords " to "keywords".
func normalizeKey(k string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(k) {
		if r >= 'a' && r <= 'z' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// asNumber reads a JSON number or a quoted one ("9"); ok is false for anything else.
func asNumber(v json.RawMessage) (float64, bool) {
	s := strings.Trim(strings.TrimSpace(string(v)), `"`)
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// asStringList reads an array of labels, a single label, or label objects.
// ok is false for anything else — a number, a boolean, null, an object, or an
// array whose elements are neither strings nor label objects — so a malformed
// field is rejected rather than becoming a silently empty list.
func asStringList(v json.RawMessage) ([]string, bool) {
	if len(v) == 0 || string(v) == "null" {
		return nil, false
	}
	var list []string
	if err := json.Unmarshal(v, &list); err == nil {
		return list, true
	}
	var single string
	if err := json.Unmarshal(v, &single); err == nil {
		return []string{single}, true
	}
	var rawList []json.RawMessage
	if err := json.Unmarshal(v, &rawList); err != nil {
		return nil, false
	}
	out := make([]string, 0, len(rawList))
	for _, raw := range rawList {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			out = append(out, s)
			continue
		}
		var object struct {
			Label string `json:"label"`
			Name  string `json:"name"`
		}
		if err := json.Unmarshal(raw, &object); err != nil {
			return nil, false
		}
		switch {
		case object.Label != "":
			out = append(out, object.Label)
		case object.Name != "":
			out = append(out, object.Name)
		default:
			return nil, false
		}
	}
	return out, true
}

// repairAttempts bounds how many trailing entries unmarshalClassifications discards.
const repairAttempts = 3

// unmarshalClassifications decodes the verdict. A response the server cut off
// mid-generation is salvaged, never repaired into data the model did not write:
// only entries that closed before the cut are kept. The trailing entry — the
// one holding an unterminated string, an open array, or an unfinished key — is
// discarded whole, so a truncated label prefix cannot become a stored tag and
// an open bracket cannot close into a fabricated empty list. The missing entry
// then fails the one-entry-per-item check, and the caller retries the item.
func unmarshalClassifications(s string) (rawClassifications, error) {
	s = salvageCompleteEntries(s)
	var firstErr error
	for range repairAttempts {
		var parsed rawClassifications
		err := json.Unmarshal([]byte(repairJSON(s)), &parsed)
		if err == nil {
			return parsed, nil
		}
		if firstErr == nil {
			firstErr = err
		}
		// Discard the trailing entry and retry. The cut is structural — the
		// last `{` outside any string — so the remainder never ends inside a
		// label and repairJSON cannot turn a prefix into a verdict.
		open := lastStructuralObjectOpen(s)
		if open <= 0 {
			break
		}
		s = strings.TrimRight(s[:open], " \t\r\n,")
	}
	return rawClassifications{}, firstErr
}

// salvageCompleteEntries cuts a truncated fragment back to the last
// classification entry that closed before the cut. Balanced input is returned
// as-is. A fragment with no complete entry is emptied, so repairJSON has
// nothing to fabricate from and the caller sees a parse failure.
func salvageCompleteEntries(s string) string {
	_, _, _, end := scanJSON(s)
	if end >= 0 {
		return s // the outer object closed; nothing was cut off
	}
	if cut := lastCompleteEntryEnd(s); cut > 0 {
		return s[:cut]
	}
	return ""
}

// lastStructuralObjectOpen returns the offset of the last `{` that sits
// outside any string, or -1 when there is none. Unlike strings.LastIndexByte
// it cannot land inside a label, so cutting there leaves balanced strings.
func lastStructuralObjectOpen(s string) int {
	open, inString, escaped := -1, false, false
	for i := range len(s) {
		c := s[i]
		switch {
		case escaped:
			escaped = false
		case inString && c == '\\':
			escaped = true
		case c == '"':
			inString = !inString
		case inString:
			// A `{` inside a string is content, not structure.
		case c == '{':
			open = i
		}
	}
	return open
}

// lastCompleteEntryEnd returns the offset just past the last `}` that closed a
// classification entry — an object sitting directly inside the classifications
// array, which itself sits in the outer object, so an entry closes when the
// stack shrinks from three brackets to two. String contents are skipped, so a
// `{` inside a label never reads as structure. It returns -1 when no entry
// completed.
func lastCompleteEntryEnd(s string) int {
	var stack []byte
	inString, escaped := false, false
	last := -1
	for i := range len(s) {
		c := s[i]
		switch {
		case escaped:
			escaped = false
		case inString && c == '\\':
			escaped = true
		case c == '"':
			inString = !inString
		case inString:
			// Brackets inside a string are content, not structure.
		case c == '{' || c == '[':
			stack = append(stack, c)
		case c == '}' || c == ']':
			if len(stack) > 0 && stack[len(stack)-1] == '{' && len(stack) == 3 {
				last = i + 1
			}
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}
	return last
}

// extractJSONObject returns the outermost JSON object in raw, dropping prose or
// code fences around it. A truncated one is returned for repairJSON.
func extractJSONObject(raw string) (string, error) {
	start := strings.IndexByte(raw, '{')
	if start < 0 {
		return "", fmt.Errorf("no JSON object found in response")
	}
	if _, _, _, end := scanJSON(raw[start:]); end >= 0 {
		return raw[start : start+end], nil
	}
	return raw[start:], nil
}

// repairJSON closes a truncated fragment by appending the brackets still on
// the stack. Balanced input is returned as-is. salvageCompleteEntries has
// already discarded any entry with an unterminated string, so the string
// branch below is defense in depth for callers that skip salvage; the parse
// path itself never turns a label prefix into a verdict.
func repairJSON(s string) string {
	stack, inString, escaped, _ := scanJSON(s)
	if len(stack) == 0 && !inString {
		return s
	}
	var b strings.Builder
	if inString {
		if escaped {
			s = s[:len(s)-1]
		}
		b.WriteString(s)
		b.WriteByte('"')
	} else {
		b.WriteString(strings.TrimRight(s, " \t\r\n,:"))
	}
	for i := len(stack) - 1; i >= 0; i-- {
		if stack[i] == '{' {
			b.WriteByte('}')
		} else {
			b.WriteByte(']')
		}
	}
	return b.String()
}

// scanJSON reports the parser state at the end of s: brackets still open,
// whether it ended inside a string or on an escape, and the offset past the
// outermost value (-1 if it never closed).
func scanJSON(s string) (stack []byte, inString, escaped bool, end int) {
	end = -1
	for i := range len(s) {
		c := s[i]
		switch {
		case escaped:
			escaped = false
		case inString && c == '\\':
			escaped = true
		case c == '"':
			inString = !inString
		case inString:
			// Brackets inside a string are content, not structure.
		case c == '{' || c == '[':
			stack = append(stack, c)
		case c == '}' || c == ']':
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
				if len(stack) == 0 && end < 0 {
					end = i + 1
				}
			}
		}
	}
	return stack, inString, escaped, end
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
