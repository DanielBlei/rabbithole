// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"errors"
	"strconv"
)

// Decision Brief sizing bounds. Every value is a ceiling on what one explicit
// deep dive may read or return; none of them affects feed ingestion or scoring.
const (
	DefaultDecisionBriefMaxArticleBytes = 5 << 20
	DefaultDecisionBriefMaxTextBytes    = 32 << 10
	DefaultDecisionBriefMaxOutputTokens = 1024

	MaxDecisionBriefMaxArticleBytes = 16 << 20
	MaxDecisionBriefMaxTextBytes    = 256 << 10
	// minDecisionBriefContentChars mirrors articleextract.MinContentChars
	// without importing the extraction package.
	minDecisionBriefContentChars = 400
	// MinDecisionBriefMaxTextBytes is the smallest positive max_text_bytes that
	// can carry the extractor's minimum usable article text. max_text_bytes is a
	// byte budget while the extractor floor counts Unicode characters, and one
	// UTF-8 character can occupy up to 4 bytes. This is a conservative floor,
	// not a guarantee that every article succeeds.
	MinDecisionBriefMaxTextBytes    = 4 * minDecisionBriefContentChars
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

// validateRaw checks values as written, before defaulting or clamping. Negative
// values are invalid; zero means unset. Upper bounds are intentionally
// normalized by WithDefaults, so an over-cap value is not an error here.
func (b DecisionBriefConfig) validateRaw() error {
	var errs []error
	add := func(field, msg string) {
		errs = append(errs, FieldError{Field: field, Msg: msg})
	}
	if b.MaxArticleBytes < 0 {
		add("decision_brief.max_article_bytes",
			"decision_brief.max_article_bytes must not be negative")
	}
	if b.MaxTextBytes < 0 {
		add("decision_brief.max_text_bytes",
			"decision_brief.max_text_bytes must not be negative")
	} else if b.MaxTextBytes > 0 && b.MaxTextBytes < MinDecisionBriefMaxTextBytes {
		add("decision_brief.max_text_bytes",
			"decision_brief.max_text_bytes must be at least "+strconv.Itoa(MinDecisionBriefMaxTextBytes))
	}
	if b.MaxOutputTokens < 0 {
		add("decision_brief.max_output_tokens",
			"decision_brief.max_output_tokens must not be negative")
	}
	return errors.Join(errs...)
}

// WithDefaults returns b with every unset or non-positive field replaced by its
// default, values above the hard caps pulled back to them, and a positive
// output-token value floored to its documented minimum.
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
