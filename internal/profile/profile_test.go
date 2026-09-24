// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"errors"
	"strings"
	"testing"
)

func TestDefaultProfile(t *testing.T) {
	got := Default()
	if got.ID != DefaultID || got.Name != DefaultName || !got.Builtin {
		t.Fatalf("Default() = %+v", got)
	}
	if got.Content == "" || got.Hash != ContentHash(got.Content) {
		t.Fatalf("Default() content/hash invalid: %+v", got)
	}
}

func TestValidateContentPreservesCommentsButRejectsCommentOnly(t *testing.T) {
	got, err := ValidateContent("<!-- note -->\n# Profile\nAI")
	if err != nil {
		t.Fatalf("ValidateContent: %v", err)
	}
	if got != "<!-- note -->\n# Profile\nAI" {
		t.Fatalf("ValidateContent = %q", got)
	}
	if _, err := ValidateContent("<!-- only -->"); !errors.Is(err, ErrInvalidContent) {
		t.Fatalf("comment-only error = %v, want ErrInvalidContent", err)
	}
}

func TestCanonicalEditorRoundTrip(t *testing.T) {
	want := EditorFields{
		Interested: "Local models\nPractical systems",
		Less:       "Press releases",
		Context:    "Prefer depth.",
	}
	raw := RenderCanonical(want)
	got, ok := ParseCanonical(raw)
	if !ok || got != want {
		t.Fatalf("ParseCanonical(RenderCanonical()) = %+v, %v; want %+v", got, ok, want)
	}
	if _, ok := ParseCanonical("# Arbitrary\n\nFree form"); ok {
		t.Fatal("arbitrary markdown was treated as canonical")
	}
}

func TestValidationLimits(t *testing.T) {
	if _, err := ValidateName(" \n "); !errors.Is(err, ErrInvalidName) {
		t.Errorf("blank name error = %v", err)
	}
	if _, err := ValidateName(strings.Repeat("x", MaxNameBytes+1)); !errors.Is(err, ErrInvalidName) {
		t.Errorf("long name error = %v", err)
	}
	if err := ValidateID(DefaultID); !errors.Is(err, ErrInvalidID) {
		t.Errorf("reserved ID error = %v", err)
	}
	if err := ValidateID("../bad"); !errors.Is(err, ErrInvalidID) {
		t.Errorf("malformed ID error = %v", err)
	}
}
