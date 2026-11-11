// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

// TestBriefingCapsRejectOverLimitValues pins the review fix: defaulting no
// longer clamps, so a value above a hard cap reaches validate and the config
// is rejected instead of silently shrunk.
func TestBriefingCapsRejectOverLimitValues(t *testing.T) {
	for _, tc := range []struct {
		name  string
		set   func(*BriefingConfig)
		field string
	}{
		{"max_articles over cap", func(b *BriefingConfig) { b.MaxArticles = MaxBriefingArticles + 1 }, "briefing.max_articles"},
		{"max_developments over cap", func(b *BriefingConfig) { b.MaxDevelopments = MaxBriefingDevelopments + 1 }, "briefing.max_developments"},
		{"recommended_reads over cap", func(b *BriefingConfig) { b.RecommendedReads = MaxBriefingRecommendedReads + 1 }, "briefing.recommended_reads"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := BriefingConfig{}
			tc.set(&b)
			r := Config{Inference: InferenceConfig{Provider: "heuristic"}, Briefing: b}
			_, err := r.Prepare()
			if err == nil {
				t.Fatalf("Prepare() accepted %s over its cap", tc.field)
			}
			fe := FieldErrors(err)
			if len(fe) != 1 || fe[0].Field != tc.field {
				t.Fatalf("field errors = %v, want exactly one naming %s", fe, tc.field)
			}
			if !strings.Contains(fe[0].Error(), "must be at most") {
				t.Errorf("error = %q, want the cap message", fe[0].Error())
			}
		})
	}

	t.Run("values at the caps still pass", func(t *testing.T) {
		r := Config{Inference: InferenceConfig{Provider: "heuristic"}, Briefing: BriefingConfig{
			MaxArticles:      MaxBriefingArticles,
			MaxDevelopments:  MaxBriefingDevelopments,
			RecommendedReads: MaxBriefingRecommendedReads,
		}}
		if _, err := r.Prepare(); err != nil {
			t.Fatalf("Prepare() error = %v", err)
		}
	})
}

// TestBriefingDefaultsForUnset covers the defaulting half of the split: unset
// and non-positive values still take the documented defaults.
func TestBriefingDefaultsForUnset(t *testing.T) {
	for _, b := range []BriefingConfig{
		{},
		{MaxArticles: -3, MaxDevelopments: 0, RecommendedReads: -1},
	} {
		d := b.WithDefaults()
		if d.MaxArticles != DefaultBriefingMaxArticles ||
			d.MaxDevelopments != DefaultBriefingMaxDevelopments ||
			d.RecommendedReads != DefaultBriefingRecommendedReads {
			t.Errorf("WithDefaults(%+v) = %+v, want the documented defaults", b, d)
		}
	}
}
