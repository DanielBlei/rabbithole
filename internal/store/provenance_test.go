// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/feeds"
)

func TestFeedbackProvenanceRoundTrip(t *testing.T) {
	db := openTestStore(t)
	ctx := t.Context()
	item := feeds.Item{ID: "p", Source: "S", Title: "P", Link: "https://x/p"}
	if err := db.Record(ctx, []feeds.Item{item}, []DigestEntry{{
		Item: item, Score: 8, Reason: "match", Model: "m",
		ProfileID: "prof", ProfileName: "Profile", ProfileHash: "profile-hash",
		FeedbackPositive: 3, FeedbackNegative: 2, FeedbackHash: "feedback-hash",
	}}, time.Now()); err != nil {
		t.Fatalf("Record: %v", err)
	}
	row, err := db.Get(ctx, item.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.LLMFeedbackPositive == nil || *row.LLMFeedbackPositive != 3 ||
		row.LLMFeedbackNegative == nil || *row.LLMFeedbackNegative != 2 ||
		row.LLMFeedbackHash == nil || *row.LLMFeedbackHash != "feedback-hash" {
		t.Fatalf("provenance = +%v -%v @%v, want 3/2/feedback-hash",
			row.LLMFeedbackPositive, row.LLMFeedbackNegative, row.LLMFeedbackHash)
	}
}

func TestReplaceItemScoresSetsAndClearsFeedbackProvenance(t *testing.T) {
	db := openTestStore(t)
	ctx := t.Context()
	item := feeds.Item{ID: "p", Source: "S", Title: "P", Link: "https://x/p"}
	if err := db.Record(ctx, []feeds.Item{item}, []DigestEntry{{
		Item: item, Score: 8, Reason: "match", Model: "m",
		ProfileID: "prof", ProfileName: "Profile", ProfileHash: "profile-hash",
		FeedbackPositive: 3, FeedbackNegative: 0, FeedbackHash: "old-hash",
	}}, time.Now()); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// A rescore without feedback must replace the whole score, stale
	// provenance included.
	if _, err := db.ReplaceItemScores(ctx, []ScoreReplacement{{
		ItemID: "p", Score: 6, Reason: "rescored", Model: "m2",
		ProfileID: "prof", ProfileName: "Profile", ProfileHash: "profile-hash",
	}}); err != nil {
		t.Fatalf("ReplaceItemScores(no feedback): %v", err)
	}
	row, err := db.Get(ctx, item.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.LLMFeedbackHash != nil || row.LLMFeedbackPositive != nil || row.LLMFeedbackNegative != nil {
		t.Fatalf("cleared rescore kept provenance = +%v -%v @%v",
			row.LLMFeedbackPositive, row.LLMFeedbackNegative, row.LLMFeedbackHash)
	}

	if _, err := db.ReplaceItemScores(ctx, []ScoreReplacement{{
		ItemID: "p", Score: 9, Reason: "feedback rescore", Model: "m3",
		ProfileID: "prof", ProfileName: "Profile", ProfileHash: "profile-hash",
		FeedbackPositive: 1, FeedbackNegative: 4, FeedbackHash: "new-hash",
	}}); err != nil {
		t.Fatalf("ReplaceItemScores(feedback): %v", err)
	}
	row, err = db.Get(ctx, item.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.LLMFeedbackPositive == nil || *row.LLMFeedbackPositive != 1 ||
		row.LLMFeedbackNegative == nil || *row.LLMFeedbackNegative != 4 ||
		row.LLMFeedbackHash == nil || *row.LLMFeedbackHash != "new-hash" {
		t.Fatalf("new provenance = +%v -%v @%v, want 1/4/new-hash",
			row.LLMFeedbackPositive, row.LLMFeedbackNegative, row.LLMFeedbackHash)
	}
}

func TestCountExplicitFeedback(t *testing.T) {
	db := openTestStore(t)
	ctx := t.Context()
	items := []feeds.Item{
		{ID: "up", Source: "S", Title: "Up", Link: "https://x/up"},
		{ID: "down", Source: "S", Title: "Down", Link: "https://x/down"},
		{ID: "mid", Source: "S", Title: "Mid", Link: "https://x/mid"},
		{ID: "unrated", Source: "S", Title: "Unrated", Link: "https://x/unrated"},
	}
	if err := db.Record(ctx, items, nil, time.Now()); err != nil {
		t.Fatalf("Record: %v", err)
	}
	for id, score := range map[string]int{"up": 10, "down": 0, "mid": 5} {
		value := score
		if err := db.UpdateUserState(ctx, id, UserPatch{UserScore: &value}); err != nil {
			t.Fatalf("rate %s: %v", id, err)
		}
	}
	got, err := db.CountExplicitFeedback(ctx)
	if err != nil {
		t.Fatalf("CountExplicitFeedback: %v", err)
	}
	if got != 2 {
		t.Errorf("count = %d, want 2 explicit ratings (intermediate ignored)", got)
	}
}

func TestOpenMigratesV4FeedbackProvenance(t *testing.T) {
	db := openTestStore(t)
	ctx := t.Context()
	item := feeds.Item{ID: "old", Source: "S", Title: "Old", Link: "https://x/old"}
	if err := db.Record(ctx, []feeds.Item{item}, []DigestEntry{{
		Item: item, Score: 7, Reason: "old", Model: "m",
		ProfileID: "prof", ProfileName: "Profile", ProfileHash: "profile-hash",
	}}, time.Now()); err != nil {
		t.Fatalf("Record: %v", err)
	}
	for _, column := range []string{
		"llm_feedback_positive", "llm_feedback_negative", "llm_feedback_hash",
	} {
		if _, err := db.exec(ctx, "ALTER TABLE items DROP COLUMN "+column); err != nil {
			t.Fatalf("drop %s: %v", column, err)
		}
	}
	if onPostgres() {
		if _, err := db.exec(ctx, "UPDATE schema_version SET version = 4 WHERE id = 1"); err != nil {
			t.Fatalf("set version 4: %v", err)
		}
	} else {
		if _, err := db.exec(ctx, "PRAGMA user_version = 4"); err != nil {
			t.Fatalf("set version 4: %v", err)
		}
	}
	if err := initSchema(ctx, db.db, db.d, "test"); err != nil {
		t.Fatalf("initSchema from v4: %v", err)
	}
	row, err := db.Get(ctx, item.ID)
	if err != nil {
		t.Fatalf("Get after migration: %v", err)
	}
	if row.LLMScore == nil || *row.LLMScore != 7 || row.LLMFeedbackHash != nil {
		t.Fatalf("historical row after migration = %+v", row)
	}
	if err := db.Record(ctx, []feeds.Item{item}, []DigestEntry{{
		Item: item, Score: 8, Reason: "new", Model: "m",
		ProfileID: "prof", ProfileName: "Profile", ProfileHash: "profile-hash",
		FeedbackPositive: 2, FeedbackNegative: 1, FeedbackHash: "migrated-hash",
	}}, time.Now()); err != nil {
		t.Fatalf("Record after migration: %v", err)
	}
	row, err = db.Get(ctx, item.ID)
	if err != nil {
		t.Fatalf("Get after migration write: %v", err)
	}
	if row.LLMFeedbackHash == nil || *row.LLMFeedbackHash != "migrated-hash" {
		t.Fatalf("migrated columns unavailable: %+v", row)
	}
}
