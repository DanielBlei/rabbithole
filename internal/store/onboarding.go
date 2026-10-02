// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"time"
)

// onboardingSchema records the first-run welcome: one row per step taken, and
// one for the welcome itself once it is dismissed. Kept with the data, so a
// new database greets its user again.
//
// Plain TEXT and ON CONFLICT DO NOTHING, so the table ports to Postgres as is.
const onboardingSchema = `
CREATE TABLE IF NOT EXISTS onboarding (
	step    TEXT PRIMARY KEY,
	done_at TEXT NOT NULL
);
`

// Onboarding steps. The first three are the welcome's checklist; the last is
// the welcome being dismissed.
const (
	OnboardingSources   = "sources"
	OnboardingProfile   = "profile"
	OnboardingIngest    = "ingest"
	OnboardingDismissed = "dismissed"
)

// onboardingUpgradeSeed marks the welcome dismissed when the table is added to
// a database that already existed, so the welcome is for new installs only.
const onboardingUpgradeSeed = "INSERT INTO onboarding (step, done_at) VALUES ('" + OnboardingDismissed + "', ?)"

// ValidOnboardingStep reports whether step is one MarkOnboarding accepts.
func ValidOnboardingStep(step string) bool {
	switch step {
	case OnboardingSources, OnboardingProfile, OnboardingIngest, OnboardingDismissed:
		return true
	}
	return false
}

// OnboardingDone returns the steps recorded so far.
func (s *Store) OnboardingDone(ctx context.Context) (map[string]bool, error) {
	rows, err := s.query(ctx, "SELECT step FROM onboarding")
	if err != nil {
		return nil, fmt.Errorf("list onboarding steps: %w", err)
	}
	defer func() { _ = rows.Close() }()
	done := map[string]bool{}
	for rows.Next() {
		var step string
		if err := rows.Scan(&step); err != nil {
			return nil, fmt.Errorf("scan onboarding step: %w", err)
		}
		done[step] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list onboarding steps: %w", err)
	}
	return done, nil
}

// MarkOnboarding records a step. Marking it again keeps the first time.
func (s *Store) MarkOnboarding(ctx context.Context, step string) error {
	if !ValidOnboardingStep(step) {
		return fmt.Errorf("unknown onboarding step %q", step)
	}
	if _, err := s.exec(ctx,
		"INSERT INTO onboarding (step, done_at) VALUES (?, ?) ON CONFLICT (step) DO NOTHING",
		step, sqlTime(time.Now())); err != nil {
		return fmt.Errorf("mark onboarding step: %w", err)
	}
	return nil
}
