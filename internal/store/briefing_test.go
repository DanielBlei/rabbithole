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
