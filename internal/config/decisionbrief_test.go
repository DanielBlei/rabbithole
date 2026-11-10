// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package config

import "testing"

func TestDecisionBriefDefaultsAndCaps(t *testing.T) {
	got := DecisionBriefConfig{}.WithDefaults()
	if got.MaxArticleBytes != DefaultDecisionBriefMaxArticleBytes ||
		got.MaxTextBytes != DefaultDecisionBriefMaxTextBytes ||
		got.MaxOutputTokens != DefaultDecisionBriefMaxOutputTokens {
		t.Fatalf("defaults = %+v", got)
	}
	capped := DecisionBriefConfig{
		MaxArticleBytes: MaxDecisionBriefMaxArticleBytes + 1,
		MaxTextBytes:    MaxDecisionBriefMaxTextBytes + 1,
		MaxOutputTokens: MaxDecisionBriefMaxOutputTokens + 1,
	}.WithDefaults()
	if capped.MaxArticleBytes != MaxDecisionBriefMaxArticleBytes ||
		capped.MaxTextBytes != MaxDecisionBriefMaxTextBytes ||
		capped.MaxOutputTokens != MaxDecisionBriefMaxOutputTokens {
		t.Fatalf("caps = %+v", capped)
	}
	floored := DecisionBriefConfig{MaxOutputTokens: 1}.WithDefaults()
	if floored.MaxOutputTokens != MinDecisionBriefMaxOutputTokens {
		t.Fatalf("floor = %d, want %d", floored.MaxOutputTokens, MinDecisionBriefMaxOutputTokens)
	}
}

func TestDecisionBriefParseAndSplit(t *testing.T) {
	f, err := ParseFile(
		[]byte("decision_brief:\n  max_article_bytes: 1048576\n  max_text_bytes: 8192\n  max_output_tokens: 512\n"),
	)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	run, _, _, _ := f.Split()
	if run.DecisionBrief.MaxArticleBytes != 1048576 ||
		run.DecisionBrief.MaxTextBytes != 8192 ||
		run.DecisionBrief.MaxOutputTokens != 512 {
		t.Fatalf("split = %+v", run.DecisionBrief)
	}
	if _, err := run.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if run.DecisionBrief.MaxOutputTokens != 512 {
		t.Fatalf("explicit value was rewritten: %+v", run.DecisionBrief)
	}
}

func TestDecisionBriefValidation(t *testing.T) {
	cfg := Config{DecisionBrief: DecisionBriefConfig{
		MaxArticleBytes: MaxDecisionBriefMaxArticleBytes + 1,
		MaxTextBytes:    MaxDecisionBriefMaxTextBytes + 1,
		MaxOutputTokens: MinDecisionBriefMaxOutputTokens - 1,
	}}
	err := cfg.validate()
	fields := map[string]bool{}
	for _, fe := range FieldErrors(err) {
		fields[fe.Field] = true
	}
	for _, want := range []string{
		"decision_brief.max_article_bytes",
		"decision_brief.max_text_bytes",
		"decision_brief.max_output_tokens",
	} {
		if !fields[want] {
			t.Errorf("missing FieldError for %s (got %v, err=%v)", want, fields, err)
		}
	}
}

func TestDecisionBriefDropDefaults(t *testing.T) {
	r := Config{DecisionBrief: DecisionBriefConfig{
		MaxArticleBytes: DefaultDecisionBriefMaxArticleBytes,
		MaxTextBytes:    DefaultDecisionBriefMaxTextBytes,
		MaxOutputTokens: DefaultDecisionBriefMaxOutputTokens,
	}}
	dropped := r.DropDefaults()
	want := map[string]bool{
		"decision_brief.max_article_bytes": true,
		"decision_brief.max_text_bytes":    true,
		"decision_brief.max_output_tokens": true,
	}
	for _, name := range dropped {
		delete(want, name)
	}
	if len(want) != 0 {
		t.Fatalf("DropDefaults missed %v (dropped=%v)", want, dropped)
	}
	if r.DecisionBrief != (DecisionBriefConfig{}) {
		t.Fatalf("defaults were not cleared: %+v", r.DecisionBrief)
	}
	// A non-default value is kept.
	r2 := Config{DecisionBrief: DecisionBriefConfig{MaxOutputTokens: 512}}
	_ = r2.DropDefaults()
	if r2.DecisionBrief.MaxOutputTokens != 512 {
		t.Fatalf("non-default value dropped: %+v", r2.DecisionBrief)
	}
}

func TestDecisionBriefUnknownKeyIgnored(t *testing.T) {
	f, err := ParseFile([]byte("decision_brief:\n  unexpected: true\n"))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	run, _, _, _ := f.Split()
	if _, err := run.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
}
