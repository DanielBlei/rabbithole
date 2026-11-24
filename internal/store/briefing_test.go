// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBriefingRoundTrip(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	start := time.Date(2026, 1, 6, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, 1)
	b := Briefing{
		ID: "briefing-1", PeriodKind: "daily", PeriodStart: start, PeriodEnd: end,
		Status: BriefingReady, BodyMD: "# Briefing\n",
		ProfileID: "profile-a", ProfileName: "Reader", ProfileHash: "hash",
		Provider: "vllm", Model: "m", ItemCount: 3,
		ReferencedItemIDs: []string{"i1", "i2"},
	}
	saved, err := db.SaveBriefing(ctx, b)
	if err != nil {
		t.Fatalf("SaveBriefing: %v", err)
	}
	if !saved.PeriodStart.Equal(start) || !saved.PeriodEnd.Equal(end) {
		t.Errorf("bounds = %v..%v", saved.PeriodStart, saved.PeriodEnd)
	}
	if saved.CreatedAt.IsZero() || saved.UpdatedAt.IsZero() {
		t.Error("timestamps were not stamped")
	}

	got, err := db.BriefingForPeriod(ctx, "daily", start)
	if err != nil {
		t.Fatalf("BriefingForPeriod: %v", err)
	}
	if got.BodyMD != b.BodyMD || got.ProfileID != b.ProfileID || got.ProfileHash != b.ProfileHash ||
		got.Provider != b.Provider || got.Model != b.Model || got.ItemCount != b.ItemCount {
		t.Errorf("round trip lost fields: %+v", got)
	}
	if len(got.ReferencedItemIDs) != 2 || got.ReferencedItemIDs[1] != "i2" {
		t.Errorf("references = %v", got.ReferencedItemIDs)
	}

	if _, err := db.BriefingForPeriod(ctx, "daily", start.AddDate(0, 0, -1)); !errors.Is(err, ErrBriefingNotFound) {
		t.Fatalf("missing period error = %v, want ErrBriefingNotFound", err)
	}
}

func TestSaveBriefingReplacesThePeriod(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	start := time.Date(2026, 1, 6, 0, 0, 0, 0, time.UTC)
	base := Briefing{
		ID: "briefing-1", PeriodKind: "daily", PeriodStart: start, PeriodEnd: start.AddDate(0, 0, 1),
		Status: BriefingGenerating,
	}
	first, err := db.SaveBriefing(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	base.Status = BriefingReady
	base.BodyMD = "done"
	second, err := db.SaveBriefing(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Errorf("id changed on replace: %q -> %q", first.ID, second.ID)
	}
	if !second.CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("created_at changed on replace: %v -> %v", first.CreatedAt, second.CreatedAt)
	}
	if second.Status != BriefingReady || second.BodyMD != "done" {
		t.Errorf("replacement not stored: %+v", second)
	}
	var n int
	if err := db.queryRow(ctx,
		"SELECT COUNT(*) FROM briefings WHERE period_kind = ? AND period_start = ?",
		"daily", sqlTime(start)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("rows for period = %d, want 1", n)
	}
}

func TestSaveBriefingValidates(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	start := time.Date(2026, 1, 6, 0, 0, 0, 0, time.UTC)
	for _, b := range []Briefing{
		{ID: "x", PeriodKind: "monthly", PeriodStart: start, PeriodEnd: start.AddDate(0, 0, 1), Status: BriefingReady},
		{ID: "", PeriodKind: "daily", PeriodStart: start, PeriodEnd: start.AddDate(0, 0, 1), Status: BriefingReady},
		{ID: "x", PeriodKind: "daily", PeriodStart: start, PeriodEnd: start, Status: BriefingReady},
		{ID: "x", PeriodKind: "daily", PeriodStart: start, PeriodEnd: start.AddDate(0, 0, 1), Status: "weird"},
	} {
		if _, err := db.SaveBriefing(ctx, b); err == nil {
			t.Errorf("SaveBriefing(%+v) accepted invalid input", b)
		}
	}
}

func TestBriefingItemsAreBoundedAndScored(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	start := time.Date(2026, 1, 6, 0, 0, 0, 0, time.UTC)
	seed := func(id, title string, score any, created time.Time, summary string) {
		t.Helper()
		if _, err := db.exec(ctx, `INSERT INTO items
			(id, source, title, link, summary, created_at, updated_at, llm_score)
			VALUES (?, 'S', ?, ?, ?, ?, ?, ?)`,
			id, title, "https://example.com/"+id, summary, sqlTime(created), sqlTime(created), score); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	seed("a", "Low", 3, start.Add(time.Hour), "low summary")
	seed("b", "High", 9, start.Add(2*time.Hour), "high summary")
	seed("c", "Mid", 6, start.Add(3*time.Hour), "mid summary")
	seed("d", "Unscored", nil, start.Add(4*time.Hour), "ignored")
	seed("e", "Outside", 10, start.AddDate(0, 0, -1), "ignored")

	got, err := db.BriefingItems(ctx, start, start.AddDate(0, 0, 1), 2)
	if err != nil {
		t.Fatalf("BriefingItems: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("items = %d, want 2", len(got))
	}
	if got[0].ID != "b" || got[1].ID != "c" {
		t.Errorf("order = %s,%s; want b,c (score descending, unscored/outside excluded)",
			got[0].ID, got[1].ID)
	}
	if got[0].Summary != "high summary" {
		t.Errorf("summary = %q", got[0].Summary)
	}

	if _, err := db.BriefingItems(ctx, start, start, 5); err == nil {
		t.Error("empty window accepted")
	}
	if _, err := db.BriefingItems(ctx, start, start.AddDate(0, 0, 1), 0); err == nil {
		t.Error("zero limit accepted")
	}
	if _, err := db.BriefingItems(ctx, start, start.AddDate(0, 0, 1), MaxBriefingItems+1); err == nil {
		t.Error("over-cap limit accepted")
	}
}

// TestRecoverInterruptedBriefings pins the startup recovery policy: only rows
// still generating are settled (failed with the retry-hint error, or ready when
// a body survives), terminal rows are untouched, and a second sweep is a no-op.
func TestRecoverInterruptedBriefings(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	start := time.Date(2026, 1, 6, 0, 0, 0, 0, time.UTC)
	save := func(id string, offset int, status string, body ...string) {
		t.Helper()
		s := start.AddDate(0, 0, offset)
		b := Briefing{
			ID: id, PeriodKind: "daily", PeriodStart: s, PeriodEnd: s.AddDate(0, 0, 1),
			Status: status,
		}
		if len(body) > 0 {
			b.BodyMD = body[0]
		}
		if _, err := db.SaveBriefing(ctx, b); err != nil {
			t.Fatalf("SaveBriefing(%s): %v", id, err)
		}
	}
	save("briefing-generating", 0, BriefingGenerating)
	save("briefing-generating-2", 1, BriefingGenerating)
	save("briefing-ready", 2, BriefingReady)
	save("briefing-failed", 3, BriefingFailed)
	save("briefing-regenerating", 4, BriefingGenerating, "# previous body")

	n, err := db.RecoverInterruptedBriefings(ctx)
	if err != nil {
		t.Fatalf("RecoverInterruptedBriefings: %v", err)
	}
	if n != 3 {
		t.Fatalf("recovered = %d, want 3", n)
	}
	kept, err := db.BriefingForPeriod(ctx, "daily", start.AddDate(0, 0, 4))
	if err != nil {
		t.Fatalf("BriefingForPeriod(regenerating): %v", err)
	}
	if kept.Status != BriefingReady || kept.BodyMD != "# previous body" {
		t.Errorf("interrupted row with a body = %q %q, want ready with body kept", kept.Status, kept.BodyMD)
	}

	recovered, err := db.BriefingForPeriod(ctx, "daily", start)
	if err != nil {
		t.Fatalf("BriefingForPeriod: %v", err)
	}
	if recovered.Status != BriefingFailed {
		t.Errorf("recovered status = %q, want failed", recovered.Status)
	}
	if recovered.Error != interruptedBriefingError {
		t.Errorf("recovered error = %q, want the retry hint", recovered.Error)
	}
	ready, err := db.BriefingForPeriod(ctx, "daily", start.AddDate(0, 0, 2))
	if err != nil {
		t.Fatalf("BriefingForPeriod(ready): %v", err)
	}
	if ready.Status != BriefingReady || ready.Error != "" {
		t.Errorf("ready row was touched: %+v", ready)
	}

	if n, err := db.RecoverInterruptedBriefings(ctx); err != nil || n != 0 {
		t.Errorf("second sweep recovered = (%d, %v), want (0, nil)", n, err)
	}
}

func TestBriefingMarks(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	day := func(d int) time.Time { return time.Date(2026, 3, d, 0, 0, 0, 0, time.UTC) }
	save := func(id, kind string, start time.Time, status string) {
		t.Helper()
		end := start.AddDate(0, 0, 1)
		if kind == "weekly" {
			end = start.AddDate(0, 0, 7)
		}
		if _, err := db.SaveBriefing(ctx, Briefing{ID: id, PeriodKind: kind,
			PeriodStart: start, PeriodEnd: end, Status: status, BodyMD: "body"}); err != nil {
			t.Fatalf("SaveBriefing %s: %v", id, err)
		}
	}

	got, err := db.BriefingMarks(ctx, "daily", day(1), day(31))
	if err != nil || len(got) != 0 {
		t.Fatalf("empty range = %v, %v", got, err)
	}

	save("d1", "daily", day(1), BriefingReady)
	save("d5", "daily", day(5), BriefingFailed)
	save("d10", "daily", day(10), BriefingReady)
	save("w5", "weekly", day(2), BriefingReady)

	got, err = db.BriefingMarks(ctx, "daily", day(1), day(10))
	if err != nil {
		t.Fatalf("BriefingMarks: %v", err)
	}
	if len(got) != 2 || !got[0].PeriodStart.Equal(day(1)) || got[0].Status != BriefingReady ||
		!got[1].PeriodStart.Equal(day(5)) || got[1].Status != BriefingFailed {
		t.Fatalf("marks = %+v, want day 1 ready and day 5 failed (end exclusive)", got)
	}

	got, err = db.BriefingMarks(ctx, "weekly", day(1), day(31))
	if err != nil || len(got) != 1 || !got[0].PeriodStart.Equal(day(2)) {
		t.Fatalf("weekly marks = %+v, %v", got, err)
	}

	if _, err := db.BriefingMarks(ctx, "daily", day(5), day(5)); err == nil {
		t.Fatal("empty bounds should error")
	}
}
