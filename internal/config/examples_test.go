// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"path/filepath"
	"testing"
)

func exampleFile(t *testing.T, parts ...string) string {
	t.Helper()
	return filepath.Join(append([]string{"..", "..", "docs", "examples"}, parts...)...)
}

// The showcase is the file users copy: it must import cleanly, drop nothing,
// and its active values must be the defaults a bare run applies.
func TestConfigExampleMatchesDefaults(t *testing.T) {
	f, err := ReadFile(exampleFile(t, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if f.Store != (StoreConfig{}) || f.Profile != "" || f.Ingest.Feeds != "" {
		t.Errorf("example sets bootstrap or import keys: store=%+v profile=%q feeds=%q",
			f.Store, f.Profile, f.Ingest.Feeds)
	}
	got, _, _, dropped := f.Split()
	if len(dropped) != 0 {
		t.Errorf("example sets fields its provider ignores: %v", dropped)
	}
	if _, err := got.Prepare(); err != nil {
		t.Fatalf("example does not validate: %v", err)
	}

	want := RunDefaults()
	got.Inference.ModelTuning = got.Inference.ModelTuning.Normalize()
	want.Inference.ModelTuning = want.Inference.ModelTuning.Normalize()
	if got.Hash() != want.Hash() {
		t.Errorf("example values differ from the defaults:\n got %+v\nwant %+v", got, want)
	}
}

func TestOtherExamplesLoad(t *testing.T) {
	if _, found, err := ReadFeedsFile(exampleFile(t, "feeds.yaml")); err != nil || !found {
		t.Fatalf("feeds.yaml: found=%v err=%v", found, err)
	}
	if _, err := LoadProfileFile(exampleFile(t, "prompts", "profile.md")); err != nil {
		t.Errorf("prompts/profile.md: %v", err)
	}
	// The file the example's system_prompt line points at imports as text.
	f := File{}
	f.Inference.SystemPrompt.Text = exampleFile(t, "prompts", "system.md")
	if err := f.InlineSystemPrompt(""); err != nil || f.Inference.SystemPrompt.Text == "" {
		t.Errorf("prompts/system.md: %v", err)
	}
}
