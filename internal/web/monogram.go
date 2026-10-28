// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"net/url"
	"strings"
	"unicode"
	"unicode/utf16"
)

// monogramData is the input to the favicon partial: the tile's letter, its
// palette index (0-7) and its size in px.
type monogramData struct {
	Letter string
	Color  int
	Size   int
}

// monogram builds the letter tile that stands in for a source's favicon.
func monogram(name, rawURL string, size int) monogramData {
	host := monogramHost(rawURL)
	letter := firstAlnum(name)
	if letter == "" {
		letter = firstAlnum(host)
	}
	if letter == "" {
		letter = "?"
	}
	return monogramData{Letter: letter, Color: colorIndex(monogramKey(name, rawURL)), Size: size}
}

// monogramHost returns the lowercased host of rawURL, or "" if it does not parse.
func monogramHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// firstAlnum returns the first letter or digit in s, uppercased, or "".
func firstAlnum(s string) string {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return string(unicode.ToUpper(r))
		}
	}
	return ""
}

// monogramKey is the string the colour is hashed from: the host without a
// leading "www.", or the name for the Crossref API host (which serves every
// Crossref source) and for sources with no host.
func monogramKey(name, rawURL string) string {
	host := monogramHost(rawURL)
	key := strings.TrimPrefix(host, "www.")
	if host == "" || host == "api.crossref.org" {
		key = name
	}
	key = strings.ToLower(strings.TrimSpace(key))
	if key == "" {
		return "?"
	}
	return key
}

// colorIndex maps a key to a palette index in 0-7, stable across runs. It
// hashes UTF-16 code units, as the script and the prototype do, so a name with
// characters outside the basic plane lands on the same colour everywhere.
func colorIndex(key string) int {
	var h uint32
	for _, u := range utf16.Encode([]rune(key)) {
		h = h*31 + uint32(u)
	}
	return int(h % 8)
}
