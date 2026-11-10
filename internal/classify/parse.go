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

// UnmarshalJSON reads an entry leniently, for models that ignore the schema.
func (e *classificationEntry) UnmarshalJSON(b []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		return err
	}
	for k, v := range fields {
		switch normalizeKey(k) {
		case "index":
			n, _ := asNumber(v)
			e.Index = int(math.Round(n))
		case "keywords":
			e.Keywords = asStringList(v)
		case "assumptions":
			e.Assumptions = asStringList(v)
		}
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
func asStringList(v json.RawMessage) []string {
	var list []string
	if err := json.Unmarshal(v, &list); err == nil {
		return list
	}
	var single string
	if err := json.Unmarshal(v, &single); err == nil {
		return []string{single}
	}
	var rawList []json.RawMessage
	if err := json.Unmarshal(v, &rawList); err != nil {
		return nil
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
		if err := json.Unmarshal(raw, &object); err == nil {
			if object.Label != "" {
				out = append(out, object.Label)
			} else if object.Name != "" {
				out = append(out, object.Name)
			}
		}
	}
	return out
}

// repairAttempts bounds how many trailing entries unmarshalClassifications discards.
const repairAttempts = 3

// unmarshalClassifications decodes the verdict, keeping the entries a truncated
// response did complete.
func unmarshalClassifications(s string) (rawClassifications, error) {
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
		open := strings.LastIndexByte(s, '{')
		if open <= 0 {
			break
		}
		s = strings.TrimRight(s[:open], " \t\r\n,")
	}
	return rawClassifications{}, firstErr
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

// repairJSON closes a truncated fragment: it terminates an open string and
// appends the brackets still on the stack. Balanced input is returned as-is.
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
