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
