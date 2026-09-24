// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/profile"
)

func TestProfileCRUDAndActiveState(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()

	if id, set, err := db.ActiveProfileID(ctx); err != nil || set || id != "" {
		t.Fatalf("fresh ActiveProfileID = %q, %v, %v", id, set, err)
	}
	first, err := db.CreateProfile(ctx, "Local", "# Local\nAI systems")
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	if first.ID == "" || first.Name != "Local" || first.Content != "# Local\nAI systems" {
		t.Fatalf("created profile = %+v", first)
	}
	got, err := db.GetProfile(ctx, first.ID)
	if err != nil || got != first {
		t.Fatalf("GetProfile = %+v, %v; want %+v", got, err, first)
	}

	updated, err := db.UpdateProfile(ctx, first.ID, "Local edited", "# Edited")
	if err != nil {
		t.Fatalf("UpdateProfile: %v", err)
	}
	if updated.ID != first.ID || updated.Name != "Local edited" || updated.Content != "# Edited" ||
		updated.UpdatedAt.Before(first.UpdatedAt) {
		t.Fatalf("updated profile = %+v; original %+v", updated, first)
	}

	copy, err := db.DuplicateProfile(ctx, first.ID, "Local copy")
	if err != nil {
		t.Fatalf("DuplicateProfile: %v", err)
	}
	if copy.ID == first.ID || copy.Name != "Local copy" || copy.Content != updated.Content {
		t.Fatalf("duplicate = %+v; source %+v", copy, updated)
	}
	list, err := db.ListProfiles(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("ListProfiles = %+v, %v", list, err)
	}

	if err := db.SetActiveProfile(ctx, first.ID); err != nil {
		t.Fatalf("SetActiveProfile: %v", err)
	}
	if id, set, err := db.ActiveProfileID(ctx); err != nil || !set || id != first.ID {
		t.Fatalf("ActiveProfileID = %q, %v, %v", id, set, err)
	}
	if err := db.DeleteProfile(ctx, first.ID); err != nil {
		t.Fatalf("DeleteProfile: %v", err)
	}
	if id, set, err := db.ActiveProfileID(ctx); err != nil || !set || id != profile.DefaultID {
		t.Fatalf("active after delete = %q, %v, %v; want Default", id, set, err)
	}
	if _, err := db.GetProfile(ctx, first.ID); !errors.Is(err, ErrProfileNotFound) {
		t.Fatalf("GetProfile(deleted) = %v, want ErrProfileNotFound", err)
	}
}

func TestProfileValidationAndDefaultImmutability(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name, content string
	}{
		{"", "# content"},
		{" \n", "# content"},
		{"Name", ""},
		{"Name", "<!-- comments only -->"},
	} {
		if _, err := db.CreateProfile(ctx, tc.name, tc.content); err == nil {
			t.Errorf("CreateProfile(%q, %q) succeeded", tc.name, tc.content)
		}
	}
	if _, err := db.CreateProfile(ctx, strings.Repeat("x", profile.MaxNameBytes+1), "# x"); err == nil {
		t.Error("overlong profile name succeeded")
	}
	if _, err := db.CreateProfile(ctx, "x", strings.Repeat("x", profile.MaxContentBytes+1)); err == nil {
		t.Error("overlong profile content succeeded")
	}
	if _, _, err := db.EnsureProfile(ctx, profile.DefaultID, "Fake Default", "# mutable"); err == nil {
		t.Error("EnsureProfile represented built-in Default as a mutable row")
	}
	if _, err := db.UpdateProfile(
		ctx, profile.DefaultID, "Changed", "# changed",
	); !errors.Is(err, ErrProfileImmutable) {
		t.Fatalf("UpdateProfile(Default) = %v", err)
	}
	if err := db.DeleteProfile(ctx, profile.DefaultID); !errors.Is(err, ErrProfileImmutable) {
		t.Fatalf("DeleteProfile(Default) = %v", err)
	}
	if err := db.SetActiveProfile(ctx, "missing-profile"); !errors.Is(err, ErrProfileNotFound) {
		t.Fatalf("SetActiveProfile(missing) = %v", err)
	}
}

func TestScoreProfileProvenanceIsHistorical(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	p, err := db.CreateProfile(ctx, "Before", "# Before")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := profile.NewSnapshot(p.ID, p.Name, p.Content, false)
	item := feeds.Item{ID: "a", Source: "S", Title: "A", Link: "https://x/a"}
	if err := db.Record(ctx, []feeds.Item{item}, []DigestEntry{{
		Item: item, Score: 8, Reason: "match", Model: "m",
		ProfileID: snapshot.ID, ProfileName: snapshot.Name, ProfileHash: snapshot.Hash,
	}}, time.Now()); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if _, err := db.UpdateProfile(ctx, p.ID, "After", "# Different"); err != nil {
		t.Fatal(err)
	}
	row, err := db.Get(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.LLMProfileID == nil || *row.LLMProfileID != snapshot.ID ||
		row.LLMProfileName == nil || *row.LLMProfileName != "Before" ||
		row.LLMProfileHash == nil || *row.LLMProfileHash != snapshot.Hash {
		t.Fatalf("historical provenance changed: %+v", row)
	}
}

const legacyV3ItemsSchema = `
CREATE TABLE items (
	id               TEXT PRIMARY KEY,
	source           TEXT NOT NULL,
	title            TEXT NOT NULL,
	link             TEXT NOT NULL UNIQUE,
	summary          TEXT,
	published_at     TIMESTAMP,
	created_at       TIMESTAMP NOT NULL,
	updated_at       TIMESTAMP NOT NULL,
	llm_score        INTEGER,
	llm_score_reason TEXT,
	llm_score_model  TEXT,
	digested_on      DATE,
	status           TEXT NOT NULL DEFAULT 'unread',
	user_score       INTEGER,
	user_note        TEXT,
	bookmarked       BOOLEAN NOT NULL DEFAULT 0,
	tags             TEXT
);
`

func TestOpenMigratesV3ProfilesAndProvenance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v3.db")
	raw, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	oldSchemas := []string{
		legacyV3ItemsSchema, todoSchema, ideaSchema, ingestSchema, ingestLogSchema,
		feedFetchSchema, feedConfigSchema,
	}
	for _, stmt := range oldSchemas {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("create v3 schema: %v", err)
		}
	}
	if _, err := raw.Exec(fmt.Sprintf("PRAGMA user_version = %d", 3)); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO items
		(id, source, title, link, created_at, updated_at, llm_score, llm_score_reason, llm_score_model)
		VALUES ('old', 'S', 'Old', 'https://x/old',
		'2026-09-01T00:00:00.000000000Z', '2026-09-01T00:00:00.000000000Z', 7, 'old reason', 'old model')`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open migrated v3: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var version int
	if err := db.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("user_version = %d, %v; want %d", version, err, schemaVersion)
	}
	old, err := db.Get(context.Background(), "old")
	if err != nil {
		t.Fatalf("Get historical row: %v", err)
	}
	if old.LLMScore == nil || *old.LLMScore != 7 || old.LLMProfileID != nil ||
		old.LLMProfileName != nil || old.LLMProfileHash != nil {
		t.Fatalf("historical row after migration = %+v", old)
	}
	if _, err := db.CreateProfile(context.Background(), "New", "# New"); err != nil {
		t.Fatalf("profile table unavailable after migration: %v", err)
	}
}
