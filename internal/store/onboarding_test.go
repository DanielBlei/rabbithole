// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"maps"
	"path/filepath"
	"slices"
	"testing"
)

// A database from before the welcome gains the table already dismissed, so
// existing users are never greeted; a new one starts with nothing done.
func TestOnboardingSeededOnUpgrade(t *testing.T) {
	if onPostgres() {
		t.Skip("simulates an upgrade by reopening a SQLite file")
	}
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := db.exec(ctx, "DROP TABLE onboarding"); err != nil {
		t.Fatalf("drop onboarding: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	done, err := db.OnboardingDone(ctx)
	if err != nil {
		t.Fatalf("OnboardingDone: %v", err)
	}
	if !done[OnboardingDismissed] || len(done) != 1 {
		t.Errorf("upgraded store onboarding = %v, want only %q", done, OnboardingDismissed)
	}
}

// failingSeedDialect is a dialect whose onboarding seed breaks the table it
// just created, standing in for an upgrade interrupted after the DDL ran.
type failingSeedDialect struct {
	dialect
	seeded []additive
}

func (f failingSeedDialect) additiveTables() []additive { return f.seeded }

// A seed that fails must not leave its table behind: the next open would see
// the table and skip both statements, and a long-time user would be greeted as
// a first run. The seed here violates the table's NOT NULL, after a DDL that
// succeeds, which is the interrupted upgrade a transaction has to undo.
func TestAdditiveSeedFailureRollsBack(t *testing.T) {
	db, ctx := openTestStore(t), context.Background()
	if _, err := db.exec(ctx, "DROP TABLE onboarding"); err != nil {
		t.Fatalf("drop onboarding: %v", err)
	}

	base := db.d
	additives := base.additiveTables()
	for i := range additives {
		if additives[i].table == "onboarding" {
			additives[i].upgradeSeed = "INSERT INTO onboarding (step, done_at) VALUES ('" + OnboardingDismissed + "', NULL)"
		}
	}
	failing := failingSeedDialect{dialect: base, seeded: additives}

	if err := initSchema(ctx, db.db, failing, "test"); err == nil {
		t.Fatal("initSchema: want the seed's error")
	}
	exists, err := db.d.tableExists(ctx, db.db, "onboarding")
	if err != nil {
		t.Fatalf("check the table: %v", err)
	}
	if exists {
		t.Error("the onboarding table survived its failed seed; the create must roll back with it")
	}

	// The next open finds no table and runs both statements again, which now
	// succeed: the upgrade is retried, not lost.
	if err := initSchema(ctx, db.db, db.d, "test"); err != nil {
		t.Fatalf("retry initSchema: %v", err)
	}
	done, err := db.OnboardingDone(ctx)
	if err != nil {
		t.Fatalf("OnboardingDone: %v", err)
	}
	if !done[OnboardingDismissed] {
		t.Errorf("onboarding after retry = %v, want %q seeded", done, OnboardingDismissed)
	}
}

func TestOnboarding(t *testing.T) {
	tests := []struct {
		name    string
		marks   []string
		want    []string
		wantErr bool
	}{
		{name: "fresh store has nothing done"},
		{name: "a step is recorded", marks: []string{OnboardingSources}, want: []string{OnboardingSources}},
		{
			name:  "marking twice keeps one row",
			marks: []string{OnboardingIngest, OnboardingIngest, OnboardingDismissed},
			want:  []string{OnboardingDismissed, OnboardingIngest},
		},
		{name: "unknown step is refused", marks: []string{"elsewhere"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, ctx := openTestStore(t), context.Background()
			for _, step := range tt.marks {
				err := db.MarkOnboarding(ctx, step)
				if (err != nil) != tt.wantErr {
					t.Fatalf("MarkOnboarding(%q) error = %v, wantErr %v", step, err, tt.wantErr)
				}
			}
			done, err := db.OnboardingDone(ctx)
			if err != nil {
				t.Fatalf("OnboardingDone: %v", err)
			}
			if got := slices.Sorted(maps.Keys(done)); !slices.Equal(got, tt.want) {
				t.Errorf("OnboardingDone = %v, want %v", got, tt.want)
			}
		})
	}
}
