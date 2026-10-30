// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// seedRun inserts a history row started at the given time and returns its id.
func seedRun(t *testing.T, s *Store, started time.Time, status string) int64 {
	t.Helper()
	var id int64
	if err := s.queryRow(t.Context(), sqlInsertIngestRun,
		sqlTime(started), status, IngestTriggerManual).Scan(&id); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return id
}

func rowCount(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	if err := s.queryRow(t.Context(), "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// runIDs lists the ids still present in a table keyed by run, ascending.
func runIDs(t *testing.T, s *Store, query string) []int64 {
	t.Helper()
	rows, err := s.query(t.Context(), query)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return ids
}

func oneFetch(feedID, name string, at time.Time) []FeedFetch {
	return []FeedFetch{{FeedID: feedID, Name: name, URL: "http://" + name + ".test/feed", Items: 3, At: at}}
}

// seedRuns inserts n finished runs, oldest first, each with a log body and one
// linked fetch row, and returns their ids in that order.
func seedRuns(t *testing.T, s *Store, n int) []int64 {
	t.Helper()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ids := make([]int64, 0, n)
	for i := range n {
		at := base.Add(time.Duration(i) * time.Minute)
		id := seedRun(t, s, at, IngestStatusOK)
		if err := s.SaveIngestRunLog(t.Context(), id, []string{"line"}); err != nil {
			t.Fatalf("save log: %v", err)
		}
		if err := s.RecordRunFetches(t.Context(), id, oneFetch("feed", "feed", at)); err != nil {
			t.Fatalf("record run fetches: %v", err)
		}
		ids = append(ids, id)
	}
	return ids
}

func TestPruneIngestRunsKeepsNewestRunsAndLogs(t *testing.T) {
	s := openTestStore(t)
	ids := seedRuns(t, s, 120)

	if err := s.PruneIngestRuns(t.Context(), 0, 0); err != nil {
		t.Fatalf("PruneIngestRuns: %v", err)
	}

	history := runIDs(t, s, "SELECT id FROM ingest_history ORDER BY id")
	if !slices.Equal(history, ids[20:]) {
		t.Errorf("history ids = %v, want the newest 100 %v", history, ids[20:])
	}
	logs := runIDs(t, s, "SELECT run_id FROM ingest_run_logs ORDER BY run_id")
	if !slices.Equal(logs, ids[95:]) {
		t.Errorf("log run ids = %v, want the newest 25 %v", logs, ids[95:])
	}
	links := runIDs(t, s, "SELECT run_id FROM ingest_run_fetches ORDER BY run_id")
	if !slices.Equal(links, ids[20:]) {
		t.Errorf("link run ids = %v, want the links of the 100 kept runs", links)
	}
}

func TestPruneIngestRunsHonoursExplicitBounds(t *testing.T) {
	s := openTestStore(t)
	ids := seedRuns(t, s, 10)

	if err := s.PruneIngestRuns(t.Context(), 4, 2); err != nil {
		t.Fatalf("PruneIngestRuns: %v", err)
	}
	if got := runIDs(t, s, "SELECT id FROM ingest_history ORDER BY id"); !slices.Equal(got, ids[6:]) {
		t.Errorf("history ids = %v, want %v", got, ids[6:])
	}
	if got := runIDs(t, s, "SELECT run_id FROM ingest_run_logs ORDER BY run_id"); !slices.Equal(got, ids[8:]) {
		t.Errorf("log run ids = %v, want %v", got, ids[8:])
	}
}

func TestPruneIngestRunsKeepsHistoryWhenOnlyLogsAreOld(t *testing.T) {
	s := openTestStore(t)
	ids := seedRuns(t, s, 30)

	if err := s.PruneIngestRuns(t.Context(), 0, 0); err != nil {
		t.Fatalf("PruneIngestRuns: %v", err)
	}
	if got := rowCount(t, s, "ingest_history"); got != 30 {
		t.Errorf("history rows = %d, want 30", got)
	}
	if got := runIDs(t, s, "SELECT run_id FROM ingest_run_logs ORDER BY run_id"); !slices.Equal(got, ids[5:]) {
		t.Errorf("log run ids = %v, want the newest 25", got)
	}
	if got := rowCount(t, s, "ingest_run_fetches"); got != 30 {
		t.Errorf("link rows = %d, want 30: only log bodies are trimmed", got)
	}
}

func TestPruneIngestRunsNeverDeletesARunningRow(t *testing.T) {
	s := openTestStore(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	running := seedRun(t, s, base.Add(-time.Hour), IngestStatusRunning)
	if err := s.RecordRunFetches(t.Context(), running, oneFetch("feed", "feed", base)); err != nil {
		t.Fatalf("record: %v", err)
	}
	var finished []int64
	for i := range 5 {
		finished = append(finished, seedRun(t, s, base.Add(time.Duration(i)*time.Minute), IngestStatusOK))
	}

	if err := s.PruneIngestRuns(t.Context(), 2, 2); err != nil {
		t.Fatalf("PruneIngestRuns: %v", err)
	}

	want := append([]int64{running}, finished[3:]...)
	if got := runIDs(t, s, "SELECT id FROM ingest_history ORDER BY id"); !slices.Equal(got, want) {
		t.Errorf("history ids = %v, want %v", got, want)
	}
	if got := runIDs(t, s, "SELECT run_id FROM ingest_run_fetches"); !slices.Equal(got, []int64{running}) {
		t.Errorf("link run ids = %v, want the running run's link kept", got)
	}
}

func TestPruneIngestRunsOnEmptyStoreIsANoOp(t *testing.T) {
	s := openTestStore(t)
	if err := s.PruneIngestRuns(t.Context(), 0, 0); err != nil {
		t.Fatalf("PruneIngestRuns: %v", err)
	}
	if got := rowCount(t, s, "ingest_history"); got != 0 {
		t.Errorf("history rows = %d, want 0", got)
	}
}

func TestPruneIngestRunsRemovesLinksToPrunedFetches(t *testing.T) {
	s := openTestStore(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	older := seedRun(t, s, base, IngestStatusOK)
	newer := seedRun(t, s, base.Add(time.Minute), IngestStatusOK)
	for _, id := range []int64{older, newer} {
		at := base.Add(time.Duration(id) * time.Second)
		if err := s.RecordRunFetches(t.Context(), id, oneFetch("feed", "feed", at)); err != nil {
			t.Fatalf("record: %v", err)
		}
	}

	// The per-feed cap keeps one fetch row, so the older run's link dangles.
	if err := s.PruneFeedFetches(t.Context(), 1); err != nil {
		t.Fatalf("PruneFeedFetches: %v", err)
	}
	if got, err := s.RunFeedResults(t.Context(), older); err != nil || len(got) != 0 {
		t.Fatalf("RunFeedResults(older) = %v, %v; want none for a pruned fetch", got, err)
	}
	if got := rowCount(t, s, "ingest_run_fetches"); got != 2 {
		t.Fatalf("link rows before prune = %d, want 2", got)
	}

	if err := s.PruneIngestRuns(t.Context(), 0, 0); err != nil {
		t.Fatalf("PruneIngestRuns: %v", err)
	}
	if got := runIDs(t, s, "SELECT run_id FROM ingest_run_fetches"); !slices.Equal(got, []int64{newer}) {
		t.Errorf("link run ids = %v, want only the run whose fetch row survives", got)
	}
	if got := rowCount(t, s, "ingest_history"); got != 2 {
		t.Errorf("history rows = %d, want 2: dangling links do not prune runs", got)
	}
}

func TestDeletingARunCascadesItsLinks(t *testing.T) {
	s := openTestStore(t)
	id := seedRun(t, s, time.Now(), IngestStatusOK)
	if err := s.RecordRunFetches(t.Context(), id, oneFetch("feed", "feed", time.Now())); err != nil {
		t.Fatalf("record: %v", err)
	}
	if _, err := s.exec(t.Context(), "DELETE FROM ingest_history WHERE id = ?", id); err != nil {
		t.Fatalf("delete run: %v", err)
	}
	if got := rowCount(t, s, "ingest_run_fetches"); got != 0 {
		t.Errorf("link rows = %d, want 0 after the run is deleted", got)
	}
	if got := rowCount(t, s, "feed_fetches"); got != 1 {
		t.Errorf("feed_fetches rows = %d, want the history row to stay", got)
	}
}

func TestRecordRunFetchesLinksOnlyForARun(t *testing.T) {
	s := openTestStore(t)
	run := seedRun(t, s, time.Now(), IngestStatusOK)
	at := time.Now().Truncate(time.Millisecond)
	fetches := []FeedFetch{
		{FeedID: "a", Name: "alpha", URL: "http://a.test/feed", Items: 2, Elapsed: 250 * time.Millisecond, At: at},
		{FeedID: "b", Name: "bravo", URL: "http://b.test/feed", Error: "dial tcp", At: at},
	}

	for _, id := range []int64{0, -1} {
		if err := s.RecordRunFetches(t.Context(), id, fetches); err != nil {
			t.Fatalf("RecordRunFetches(%d): %v", id, err)
		}
	}
	if got := rowCount(t, s, "ingest_run_fetches"); got != 0 {
		t.Errorf("link rows = %d, want 0 without a run id", got)
	}
	if got := rowCount(t, s, "feed_fetches"); got != 4 {
		t.Errorf("feed_fetches rows = %d, want 4", got)
	}

	if err := s.RecordFeedFetches(t.Context(), fetches); err != nil {
		t.Fatalf("RecordFeedFetches: %v", err)
	}
	if got := rowCount(t, s, "ingest_run_fetches"); got != 0 {
		t.Errorf("link rows = %d, want 0 after RecordFeedFetches", got)
	}

	if err := s.RecordRunFetches(t.Context(), run, fetches); err != nil {
		t.Fatalf("RecordRunFetches: %v", err)
	}
	if got := rowCount(t, s, "ingest_run_fetches"); got != 2 {
		t.Errorf("link rows = %d, want 2", got)
	}
	if err := s.RecordRunFetches(t.Context(), run, nil); err != nil {
		t.Errorf("RecordRunFetches(nil) = %v, want nil", err)
	}

	got, err := s.RunFeedResults(t.Context(), run)
	if err != nil {
		t.Fatalf("RunFeedResults: %v", err)
	}
	want := []RunFeedResult{
		{FeedID: "b", Name: "bravo", URL: "http://b.test/feed", Status: FeedStatusError, Error: "dial tcp", At: at},
		{
			FeedID: "a", Name: "alpha", URL: "http://a.test/feed", Status: FeedStatusOK,
			Items: 2, Elapsed: 250 * time.Millisecond, At: at,
		},
	}
	if len(got) != len(want) {
		t.Fatalf("results = %+v, want %+v", got, want)
	}
	for i := range want {
		g := got[i]
		if !g.At.Equal(want[i].At) || g.At.Location() != time.UTC {
			t.Errorf("result %d At = %v (%v), want %v in UTC", i, g.At, g.At.Location(), want[i].At)
		}
		g.At, want[i].At = time.Time{}, time.Time{}
		if g != want[i] {
			t.Errorf("result %d = %+v, want %+v", i, g, want[i])
		}
	}
	if got[0].OK() || !got[1].OK() {
		t.Errorf("OK() = %v, %v; want false, true", got[0].OK(), got[1].OK())
	}
}

func TestRecordRunFetchesIsOneTransaction(t *testing.T) {
	s := openTestStore(t)
	// No such run: the link insert violates the foreign key after the fetch
	// row was inserted, so nothing may persist.
	err := s.RecordRunFetches(t.Context(), 999, oneFetch("feed", "feed", time.Now()))
	if err == nil {
		t.Fatal("RecordRunFetches for an unknown run = nil, want an error")
	}
	if got := rowCount(t, s, "feed_fetches"); got != 0 {
		t.Errorf("feed_fetches rows = %d, want 0 after a failed link", got)
	}
	if got := rowCount(t, s, "ingest_run_fetches"); got != 0 {
		t.Errorf("link rows = %d, want 0", got)
	}
}

func TestRunFeedResultsOrderAndScope(t *testing.T) {
	s := openTestStore(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	run := seedRun(t, s, base, IngestStatusOK)
	other := seedRun(t, s, base.Add(time.Minute), IngestStatusOK)

	err := s.RecordRunFetches(t.Context(), run, []FeedFetch{
		{FeedID: "1", Name: "delta", At: base},
		{FeedID: "2", Name: "bravo", Error: "boom", At: base},
		{FeedID: "3", Name: "alpha", At: base},
		{FeedID: "4", Name: "charlie", Error: "timeout", At: base},
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := s.RecordRunFetches(t.Context(), other, oneFetch("5", "echo", base)); err != nil {
		t.Fatalf("record other: %v", err)
	}

	got, err := s.RunFeedResults(t.Context(), run)
	if err != nil {
		t.Fatalf("RunFeedResults: %v", err)
	}
	var names []string
	for _, r := range got {
		names = append(names, r.Name)
	}
	if want := []string{"bravo", "charlie", "alpha", "delta"}; !slices.Equal(names, want) {
		t.Errorf("order = %v, want failed first then by name %v", names, want)
	}

	for _, id := range []int64{0, 12345} {
		none, err := s.RunFeedResults(t.Context(), id)
		if err != nil || len(none) != 0 {
			t.Errorf("RunFeedResults(%d) = %v, %v; want empty", id, none, err)
		}
	}
}

func TestIngestRunFetchTableCreatedOnExistingDatabase(t *testing.T) {
	if onPostgres() {
		t.Skip("simulates an upgrade by reopening a SQLite file")
	}
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.exec(ctx, "DROP TABLE ingest_run_fetches"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	run := seedRun(t, s, time.Now(), IngestStatusOK)
	if err := s.RecordRunFetches(ctx, run, oneFetch("feed", "feed", time.Now())); err != nil {
		t.Fatalf("record after upgrade: %v", err)
	}
	if got, err := s.RunFeedResults(ctx, run); err != nil || len(got) != 1 {
		t.Fatalf("RunFeedResults after upgrade = %v, %v; want one row", got, err)
	}
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != schemaVersion {
		t.Errorf("user_version = %d, %v; want %d", version, err, schemaVersion)
	}
}
