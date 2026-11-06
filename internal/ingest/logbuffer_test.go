// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/profile"
	"github.com/DanielBlei/rabbithole/internal/store"
)

func writeLines(b *logBuffer, from, to int) {
	for i := from; i <= to; i++ {
		_, _ = fmt.Fprintf(b, "line %d\n", i)
	}
}

// Sequence numbers count every line ever written, so the ring dropping its
// oldest lines never moves the newest line's number.
func TestLogBufferSeqSurvivesWrap(t *testing.T) {
	b := newLogBuffer(5)
	writeLines(b, 1, 3)
	if lines, seq := b.snapshotSeq(); len(lines) != 3 || seq != 3 {
		t.Fatalf("before wrap: %d lines, seq %d, want 3 and 3", len(lines), seq)
	}
	writeLines(b, 4, 12)
	lines, seq := b.snapshotSeq()
	if len(lines) != 5 || seq != 12 {
		t.Fatalf("after wrap: %d lines, seq %d, want 5 and 12", len(lines), seq)
	}
	if lines[0] != "line 8" || lines[4] != "line 12" {
		t.Errorf("ring holds %v, want line 8 to line 12", lines)
	}
}

func TestLogBufferSinceReturnsOnlyNewerLines(t *testing.T) {
	b := newLogBuffer(5)
	writeLines(b, 1, 4)

	c := b.since(2)
	if c.Last != 4 || c.Dropped != 0 || len(c.Lines) != 2 || c.Lines[0] != "line 3" || c.Lines[1] != "line 4" {
		t.Errorf("since(2) = %+v, want line 3 and line 4 up to 4", c)
	}
	if c := b.since(4); len(c.Lines) != 0 || c.Last != 4 || c.Dropped != 0 {
		t.Errorf("since(4) = %+v, want nothing new", c)
	}
	// A cursor past the end or below zero is clamped, not an error.
	if c := b.since(99); len(c.Lines) != 0 || c.Last != 4 || c.Dropped != 0 {
		t.Errorf("since(99) = %+v, want nothing new", c)
	}
	if c := b.since(-3); len(c.Lines) != 4 || c.Dropped != 0 {
		t.Errorf("since(-3) = %+v, want all 4 lines", c)
	}
}

// A cursor older than the ring's oldest line gets the oldest onward, and the
// count of lines it missed.
func TestLogBufferSinceReportsDroppedLines(t *testing.T) {
	b := newLogBuffer(5)
	writeLines(b, 1, 12) // the ring holds lines 8 to 12

	c := b.since(2)
	if c.Dropped != 5 {
		t.Errorf("Dropped = %d, want 5 (lines 3 to 7)", c.Dropped)
	}
	if len(c.Lines) != 5 || c.Lines[0] != "line 8" || c.Last != 12 {
		t.Errorf("since(2) = %+v, want line 8 to line 12", c)
	}
	// A cursor exactly at the edge lost nothing.
	if c := b.since(7); c.Dropped != 0 || len(c.Lines) != 5 {
		t.Errorf("since(7) = %+v, want all 5 lines and none dropped", c)
	}
	// Returned lines are a copy: later writes do not alias them.
	c = b.since(10)
	writeLines(b, 13, 20)
	if c.Lines[0] != "line 11" {
		t.Errorf("chunk changed under a later write: %v", c.Lines)
	}
}

// LogSince answers only for the run in flight.
func TestManagerLogSinceOnlyForLiveRun(t *testing.T) {
	m, release := newManagerForTest(t, Outcome{}, nil)
	if _, ok := m.LogSince(1, 0); ok {
		t.Fatal("LogSince answered with no run in flight")
	}
	if err := m.Start(t.Context(), "manual"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	st := m.Status()
	if !st.Running {
		t.Fatal("run is not live")
	}
	if _, ok := m.LogSince(st.RunID+1, 0); ok {
		t.Error("LogSince answered for another run id")
	}
	if _, ok := m.LogSince(st.RunID, 0); !ok {
		t.Error("LogSince refused the live run")
	}
	close(release)
	waitIdle(t, m)
	if _, ok := m.LogSince(st.RunID, 0); ok {
		t.Error("LogSince answered after the run finished")
	}
}

// logEvent is one captured line, decoded.
func logEvent(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("bad log line %q: %v", raw, err)
	}
	return m
}

// Every run opens with "starting ..." and closes with "... finished", whatever
// the trigger and however it ends, in the buffer and in the stored log.
func TestManagerRunLogOpensAndCloses(t *testing.T) {
	cases := []struct {
		name        string
		rescore     bool
		runErr      error
		cancel      bool
		first, last string
		status      string
		beforeLast  string // the line that says why, if any
		wantTrigger string
	}{
		{name: "ok", first: "starting ingestion", last: "ingestion finished", status: "ok",
			wantTrigger: store.IngestTriggerManual},
		{name: "failed", runErr: errors.New("boom"), first: "starting ingestion",
			last: "ingestion finished", status: "error", beforeLast: "ingest failed",
			wantTrigger: store.IngestTriggerManual},
		{name: "cancelled", cancel: true, first: "starting ingestion",
			last: "ingestion finished", status: "cancelled", beforeLast: "ingest cancelled",
			wantTrigger: store.IngestTriggerManual},
		{name: "rescore", rescore: true, first: "starting rescore", last: "rescore finished",
			status: "ok", wantTrigger: store.IngestTriggerProfileRescore},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, release := newManagerForTest(t, Outcome{}, c.runErr)
			m.rescore = func(
				context.Context, config.Config, profile.Snapshot, *store.Store, time.Time, time.Duration,
			) (RescoreOutcome, error) {
				return RescoreOutcome{}, nil
			}
			var err error
			if c.rescore {
				err = m.StartRescore(t.Context(), ProfileRescoreWindow)
			} else {
				err = m.Start(t.Context(), store.IngestTriggerManual)
			}
			if err != nil {
				t.Fatalf("start: %v", err)
			}
			if c.cancel {
				m.Cancel()
			} else {
				close(release)
			}
			waitIdle(t, m)

			check := func(what string, lines []string) {
				t.Helper()
				if len(lines) < 2 {
					t.Fatalf("%s: %d lines, want at least 2: %q", what, len(lines), lines)
				}
				first, last := logEvent(t, lines[0]), logEvent(t, lines[len(lines)-1])
				if first["level"] != "info" || first["message"] != c.first {
					t.Errorf("%s first = %v, want INF %q", what, first, c.first)
				}
				if first["trigger"] != c.wantTrigger || first["profile"] == nil {
					t.Errorf("%s first fields = %v, want trigger %q and a profile", what, first, c.wantTrigger)
				}
				if last["level"] != "info" || last["message"] != c.last || last["status"] != c.status ||
					last["elapsed"] == nil {
					t.Errorf("%s last = %v, want INF %q status=%s with elapsed", what, last, c.last, c.status)
				}
				if c.beforeLast != "" {
					if got := logEvent(t, lines[len(lines)-2])["message"]; got != c.beforeLast {
						t.Errorf("%s line before last = %v, want %q", what, got, c.beforeLast)
					}
				}
			}
			check("buffer", m.Status().Lines)
			_, stored, err := m.db.GetIngestRunWithLog(t.Context(), 1)
			if err != nil {
				t.Fatalf("GetIngestRunWithLog: %v", err)
			}
			check("stored log", stored)
		})
	}
}
