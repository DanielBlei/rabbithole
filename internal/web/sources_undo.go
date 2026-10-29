// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"github.com/DanielBlei/rabbithole/internal/store"
)

// Undo log limits: an entry is good for undoTTL and the log keeps the newest
// undoCap, dropping the oldest.
const (
	undoTTL = 15 * time.Minute
	undoCap = 32
)

// undoEntry is one reversible Manage change: the operation, the tag it used
// and the rows as they were before it.
type undoEntry struct {
	op     store.BulkOp
	tag    string
	states []store.FeedState
	at     time.Time
	seq    uint64
}

// undoLog holds the recent bulk and single changes so their Undo toast can
// reverse them. It lives in memory: a restart forgets it. A token is single
// use.
type undoLog struct {
	mu      sync.Mutex
	entries map[string]undoEntry
	seq     uint64
}

// put records an entry and returns the token that takes it back.
func (l *undoLog) put(e undoEntry, now time.Time) string {
	var raw [16]byte
	// crypto/rand.Read does not fail on the supported platforms.
	_, _ = rand.Read(raw[:])
	token := hex.EncodeToString(raw[:])
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.entries == nil {
		l.entries = make(map[string]undoEntry)
	}
	l.prune(now)
	l.seq++
	e.at, e.seq = now, l.seq
	l.entries[token] = e
	for len(l.entries) > undoCap {
		l.evictOldest()
	}
	return token
}

// take removes and returns the entry for token. It reports false for an
// unknown, spent or expired token.
func (l *undoLog) take(token string, now time.Time) (undoEntry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune(now)
	e, ok := l.entries[token]
	delete(l.entries, token)
	return e, ok
}

// putBack returns an entry taken for an undo that failed, keeping its age and
// order so its Undo can be tried again.
func (l *undoLog) putBack(token string, e undoEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.entries == nil {
		l.entries = make(map[string]undoEntry)
	}
	l.entries[token] = e
}

// prune drops the expired entries. The caller holds mu.
func (l *undoLog) prune(now time.Time) {
	for token, e := range l.entries {
		if now.Sub(e.at) >= undoTTL {
			delete(l.entries, token)
		}
	}
}

// evictOldest drops the entry recorded earliest. The caller holds mu.
func (l *undoLog) evictOldest() {
	var oldest string
	var seq uint64
	for token, e := range l.entries {
		if oldest == "" || e.seq < seq {
			oldest, seq = token, e.seq
		}
	}
	delete(l.entries, oldest)
}
