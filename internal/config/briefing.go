// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package config

// Briefing sizing defaults. They bound what one on-demand synthesis may read
// and return, so a briefing can never grow without limit.
const (
	DefaultBriefingMaxArticles      = 50
	DefaultBriefingMaxDevelopments  = 7
	DefaultBriefingRecommendedReads = 5
	MaxBriefingArticles             = 100
	MaxBriefingDevelopments         = 20
	MaxBriefingRecommendedReads     = 20
)

// BriefingConfig sizes the daily/weekly briefing. Zero values take the
// defaults, so an unset block behaves exactly like the documented defaults.
type BriefingConfig struct {
	MaxArticles      int `yaml:"max_articles,omitempty"`
	MaxDevelopments  int `yaml:"max_developments,omitempty"`
	RecommendedReads int `yaml:"recommended_reads,omitempty"`
}

// WithDefaults returns b with every unset or non-positive field replaced by its
// default. Values above the hard caps are pulled back to them.
func (b BriefingConfig) WithDefaults() BriefingConfig {
	if b.MaxArticles <= 0 {
		b.MaxArticles = DefaultBriefingMaxArticles
	}
	if b.MaxDevelopments <= 0 {
		b.MaxDevelopments = DefaultBriefingMaxDevelopments
	}
	if b.RecommendedReads <= 0 {
		b.RecommendedReads = DefaultBriefingRecommendedReads
	}
	b.MaxArticles = min(b.MaxArticles, MaxBriefingArticles)
	b.MaxDevelopments = min(b.MaxDevelopments, MaxBriefingDevelopments)
	b.RecommendedReads = min(b.RecommendedReads, MaxBriefingRecommendedReads)
	return b
}

// briefingField is one sizing knob, used by DropDefaults so an imported full
// export leaves a value equal to its default unpinned.
type briefingField struct {
	name  string
	isSet func(*BriefingConfig) bool
	clear func(*BriefingConfig)
}

var briefingFields = []briefingField{
	{"max_articles",
		func(b *BriefingConfig) bool { return b.MaxArticles != 0 },
		func(b *BriefingConfig) { b.MaxArticles = 0 }},
	{"max_developments",
		func(b *BriefingConfig) bool { return b.MaxDevelopments != 0 },
		func(b *BriefingConfig) { b.MaxDevelopments = 0 }},
	{"recommended_reads",
		func(b *BriefingConfig) bool { return b.RecommendedReads != 0 },
		func(b *BriefingConfig) { b.RecommendedReads = 0 }},
}
