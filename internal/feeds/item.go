// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

// Package feeds fetches and normalizes RSS/Atom items.
package feeds

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// Item is a normalized feed entry, independent of feed format.
type Item struct {
	ID        string    // stable identifier: sha256 of guid or link
	Source    string    // configured feed name
	Title     string    // entry title
	Link      string    // canonical URL
	Summary   string    // description/summary text (plain-ish)
	Published time.Time // publish time, zero if unknown
	Tags      []string  // the source feed's configured tags, nil when it has none
}

// makeID derives a stable ID from the most reliable available key.
// GUID is preferred; link is the fallback.
func makeID(guid, link string) string {
	key := guid
	if key == "" {
		key = link
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}
