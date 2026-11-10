// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package config

// Decision Brief sizing bounds. Every value is a ceiling on what one explicit
// deep dive may read or return; none of them affects feed ingestion or scoring.
const (
	DefaultDecisionBriefMaxArticleBytes = 5 << 20
	DefaultDecisionBriefMaxTextBytes    = 32 << 10
	DefaultDecisionBriefMaxOutputTokens = 1024

	MaxDecisionBriefMaxArticleBytes = 16 << 20
	MaxDecisionBriefMaxTextBytes    = 256 << 10
	MaxDecisionBriefMaxOutputTokens = 4096
	MinDecisionBriefMaxOutputTokens = 256
)

// DecisionBriefConfig sizes one on-demand Decision Brief. Zero values take the
// documented defaults, so an unset block behaves exactly like the defaults.
type DecisionBriefConfig struct {
	MaxArticleBytes int `yaml:"max_article_bytes,omitempty"`
	MaxTextBytes    int `yaml:"max_text_bytes,omitempty"`
	MaxOutputTokens int `yaml:"max_output_tokens,omitempty"`
}

// WithDefaults returns b with every unset or non-positive field replaced by its
// default, and values above the hard caps pulled back to them.
func (b DecisionBriefConfig) WithDefaults() DecisionBriefConfig {
	if b.MaxArticleBytes <= 0 {
		b.MaxArticleBytes = DefaultDecisionBriefMaxArticleBytes
	}
	if b.MaxTextBytes <= 0 {
		b.MaxTextBytes = DefaultDecisionBriefMaxTextBytes
	}
	if b.MaxOutputTokens <= 0 {
		b.MaxOutputTokens = DefaultDecisionBriefMaxOutputTokens
	}
	b.MaxArticleBytes = min(b.MaxArticleBytes, MaxDecisionBriefMaxArticleBytes)
	b.MaxTextBytes = min(b.MaxTextBytes, MaxDecisionBriefMaxTextBytes)
	b.MaxOutputTokens = min(b.MaxOutputTokens, MaxDecisionBriefMaxOutputTokens)
	b.MaxOutputTokens = max(b.MaxOutputTokens, MinDecisionBriefMaxOutputTokens)
	return b
}

// decisionBriefField is one sizing knob, used by DropDefaults so an imported
// full export leaves a value equal to its default unpinned.
type decisionBriefField struct {
	name  string
	isSet func(*DecisionBriefConfig) bool
	clear func(*DecisionBriefConfig)
}

var decisionBriefFields = []decisionBriefField{
	{"max_article_bytes",
		func(b *DecisionBriefConfig) bool { return b.MaxArticleBytes != 0 },
		func(b *DecisionBriefConfig) { b.MaxArticleBytes = 0 }},
	{"max_text_bytes",
		func(b *DecisionBriefConfig) bool { return b.MaxTextBytes != 0 },
		func(b *DecisionBriefConfig) { b.MaxTextBytes = 0 }},
	{"max_output_tokens",
		func(b *DecisionBriefConfig) bool { return b.MaxOutputTokens != 0 },
		func(b *DecisionBriefConfig) { b.MaxOutputTokens = 0 }},
}
