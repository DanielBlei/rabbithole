// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package tags

import (
	"reflect"
	"testing"
)

func TestNormalize(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "uppercase", input: "MACHINE LEARNING", want: "machine-learning"},
		{name: "mixed case", input: "NeUraL Networks", want: "neural-networks"},
		{name: "outer whitespace", input: " \tAI and ML\n", want: "ai-and-ml"},
		{name: "consecutive separators", input: "a  \t--__b", want: "a-b"},
		{name: "punctuation", input: "C++: Rust / Go!", want: "c-rust-go"},
		{name: "unicode letters and digits", input: "  ČEŠTINA 研究 42 ", want: "čeština-研究-42"},
		{name: "decomposed unicode", input: "Cafe\u0301", want: "café"},
		{name: "composed unicode", input: "Café", want: "café"},
		{name: "hindi vowel sign", input: "किताब", want: "किताब"},
		{name: "non-composable combining mark", input: "شّ", want: "شّ"},
		{name: "leading combining mark", input: "\u0301cafe", wantErr: true},
		{name: "combining mark after separator", input: "a-\u093f", wantErr: true},
		{name: "emoji and symbols are separators", input: "AI 🤖 + research", want: "ai-research"},
		{name: "emoji only", input: "🤖✨", wantErr: true},
		{name: "C++ and C# collision", input: "C++", want: "c"},
		{name: "C# and C++ collision", input: "C#", want: "c"},
		{name: "empty", input: " \t\n", wantErr: true},
		{name: "punctuation only", input: "...---!!!", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Normalize(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Normalize() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("Normalize() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNormalizeIsIdempotent(t *testing.T) {
	inputs := []string{"machine-learning", "café", "किताब", "شّ", "研究-42"}
	for _, input := range inputs {
		t.Run(input, func(t *testing.T) {
			first, err := Normalize(input)
			if err != nil {
				t.Fatalf("Normalize(first) error = %v", err)
			}
			second, err := Normalize(first)
			if err != nil {
				t.Fatalf("Normalize(second) error = %v", err)
			}
			if second != first {
				t.Errorf("Normalize(%q) = %q, want idempotent %q", first, second, first)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		slug    string
		wantErr bool
	}{
		{name: "valid ascii", slug: "machine-learning"},
		{name: "valid unicode", slug: "čeština-研究-42"},
		{name: "valid hindi mark", slug: "किताब"},
		{name: "valid non-composable mark", slug: "شّ"},
		{name: "empty", slug: "", wantErr: true},
		{name: "uppercase", slug: "AI", wantErr: true},
		{name: "leading hyphen", slug: "-ai", wantErr: true},
		{name: "trailing hyphen", slug: "ai-", wantErr: true},
		{name: "repeated hyphen", slug: "ai--ml", wantErr: true},
		{name: "space", slug: "machine learning", wantErr: true},
		{name: "leading combining mark", slug: "\u0301cafe", wantErr: true},
		{name: "combining mark after hyphen", slug: "a-\u093f", wantErr: true},
		{name: "decomposed non-NFC", slug: "cafe\u0301", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := Validate(tt.slug); (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestDeduplicate(t *testing.T) {
	keywords := []string{"Machine Learning", "AI", "machine-learning", " Ai "}
	assumptions := []string{"AI", "Neural Networks", "neural-networks", "GPU"}
	wantKeywords := []string{"machine-learning", "ai"}
	wantAssumptions := []string{"neural-networks", "gpu"}

	gotKeywords, gotAssumptions, err := Deduplicate(keywords, assumptions)
	if err != nil {
		t.Fatalf("Deduplicate() error = %v", err)
	}
	if !reflect.DeepEqual(gotKeywords, wantKeywords) {
		t.Errorf("keywords = %v, want %v", gotKeywords, wantKeywords)
	}
	if !reflect.DeepEqual(gotAssumptions, wantAssumptions) {
		t.Errorf("assumptions = %v, want %v", gotAssumptions, wantAssumptions)
	}
}

func TestDeduplicateStableAndDeterministic(t *testing.T) {
	keywords := []string{"B", "a", "B", "C"}
	assumptions := []string{"c", "D", "d", "A"}
	wantKeywords := []string{"b", "a", "c"}
	wantAssumptions := []string{"d"}
	for i := 0; i < 5; i++ {
		gotKeywords, gotAssumptions, err := Deduplicate(keywords, assumptions)
		if err != nil {
			t.Fatalf("Deduplicate() error = %v", err)
		}
		if !reflect.DeepEqual(gotKeywords, wantKeywords) || !reflect.DeepEqual(gotAssumptions, wantAssumptions) {
			t.Fatalf("Deduplicate() = (%v, %v), want (%v, %v)",
				gotKeywords, gotAssumptions, wantKeywords, wantAssumptions)
		}
	}
}

func TestDeduplicateRejectsInvalidWithoutPartialResults(t *testing.T) {
	keywords, assumptions, err := Deduplicate([]string{"AI", "!!!"}, []string{"ML"})
	if err == nil {
		t.Fatal("Deduplicate() error = nil, want invalid-label error")
	}
	if keywords != nil || assumptions != nil {
		t.Errorf("Deduplicate() returned partial results (%v, %v)", keywords, assumptions)
	}
}
