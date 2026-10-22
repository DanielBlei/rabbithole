// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package articletext

import (
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPrepareWhitespaceAndEmptyInput(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		want    string
		words   int
		minutes int
	}{
		{name: "empty"},
		{name: "whitespace only", text: " \t\r\n\r  "},
		{
			name: "normalizes line endings and trims outer whitespace",
			text: " \r\n# Title\r\n\rParagraph\r ", want: "# Title\n\nParagraph",
			words: 2, minutes: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Prepare(tt.text, 128)
			if err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			if got.Text != tt.want {
				t.Errorf("Text = %q, want %q", got.Text, tt.want)
			}
			if got.Truncated || got.WordCount != tt.words || got.ReadingMinutes != tt.minutes {
				t.Errorf("metadata = %+v, want words=%d minutes=%d not truncated", got, tt.words, tt.minutes)
			}
		})
	}
}

func TestPrepareRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		maxBytes int
		wantErr  string
	}{
		{name: "zero byte limit", text: "text", maxBytes: 0, wantErr: "maxBytes must be greater than zero"},
		{name: "negative byte limit", text: "text", maxBytes: -1, wantErr: "maxBytes must be greater than zero"},
		{name: "invalid UTF-8", text: string([]byte{0xff}), maxBytes: 10, wantErr: "article text is not valid UTF-8"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Prepare(tt.text, tt.maxBytes)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Prepare error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestPrepareByteLimitBoundaries(t *testing.T) {
	tests := []struct {
		name      string
		text      string
		maxBytes  int
		wantText  string
		truncated bool
	}{
		{name: "below limit", text: "short text", maxBytes: 11, wantText: "short text"},
		{name: "at limit", text: "short text", maxBytes: 10, wantText: "short text"},
		{name: "above limit", text: "short text", maxBytes: 9, wantText: "short tex", truncated: true},
		{name: "limit inside multibyte rune", text: "ab🙂z", maxBytes: 4, wantText: "ab", truncated: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Prepare(tt.text, tt.maxBytes)
			if err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			if got.Text != tt.wantText {
				t.Errorf("Text = %q, want %q", got.Text, tt.wantText)
			}
			if got.Truncated != tt.truncated {
				t.Errorf("Truncated = %v, want %v", got.Truncated, tt.truncated)
			}
			if len(got.Text) > tt.maxBytes {
				t.Errorf("output has %d bytes, limit is %d", len(got.Text), tt.maxBytes)
			}
			if !utf8.ValidString(got.Text) {
				t.Errorf("output is invalid UTF-8: %q", got.Text)
			}
		})
	}
}

func TestPrepareRejectsLimitSmallerThanFirstRune(t *testing.T) {
	for _, maxBytes := range []int{1, 2, 3} {
		t.Run(strconv.Itoa(maxBytes)+" bytes", func(t *testing.T) {
			_, err := Prepare("🙂article", maxBytes)
			if err == nil || !strings.Contains(err.Error(), "too small to fit the first UTF-8 rune") {
				t.Fatalf("Prepare error = %v, want too-small-rune error", err)
			}
		})
	}
}

func TestPreparePrefersUsefulBoundaries(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		maxBytes int
		want     string
	}{
		{
			name:     "paragraph boundary",
			text:     "First paragraph.\n\nSecond paragraph.\n\nThird paragraph.",
			maxBytes: 38,
			want:     "First paragraph.\n\nSecond paragraph.",
		},
		{
			name:     "line boundary when paragraph boundary fails retention threshold",
			text:     "12345678\n\nab\n12345678TAIL",
			maxBytes: 16,
			want:     "12345678\n\nab",
		},
		{
			name:     "rune prefix when neither boundary meets retention threshold",
			text:     "a\nb\nc",
			maxBytes: 3,
			want:     "a\nb",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Prepare(tt.text, tt.maxBytes)
			if err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			if got.Text != tt.want {
				t.Errorf("Text = %q, want %q", got.Text, tt.want)
			}
			if !got.Truncated {
				t.Error("Truncated = false, want true")
			}
		})
	}
}

func TestPrepareReadingTimeForCJKAndMixedText(t *testing.T) {
	tests := []struct {
		name        string
		text        string
		wantWords   int
		wantMinutes int
	}{
		{name: "English", text: strings.Repeat("word ", 201), wantWords: 201, wantMinutes: 2},
		{name: "Chinese", text: strings.Repeat("汉", 401), wantWords: 401, wantMinutes: 3},
		{name: "Japanese", text: strings.Repeat("あ", 201), wantWords: 201, wantMinutes: 2},
		{name: "mixed", text: "hello世界 world", wantWords: 4, wantMinutes: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Prepare(tt.text, len(tt.text))
			if err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			if got.WordCount != tt.wantWords || got.ReadingMinutes != tt.wantMinutes {
				t.Errorf(
					"metadata = words:%d minutes:%d, want words:%d minutes:%d",
					got.WordCount,
					got.ReadingMinutes,
					tt.wantWords,
					tt.wantMinutes,
				)
			}
		})
	}
}

func TestPreparePreservesStructuredText(t *testing.T) {
	text := "# Article title\n\nIntro paragraph.\n\n- first item\n- second item\n\n```go\nfmt.Println(\"hello\")\n```"
	got, err := Prepare(text, len(text))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got.Text != text {
		t.Errorf("Text = %q, want structure preserved as %q", got.Text, text)
	}
	if got.Truncated {
		t.Error("Truncated = true, want false")
	}
}

func TestPrepareIsDeterministic(t *testing.T) {
	text := "# Title\r\n\r\nFirst paragraph with enough text.\r\nSecond line.\r\n\r\nLast paragraph."
	want, err := Prepare(text, 52)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	for i := 0; i < 20; i++ {
		got, err := Prepare(text, 52)
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		if got != want {
			t.Fatalf("iteration %d: got %+v, want %+v", i, got, want)
		}
	}
}

func TestPrepareReadingTimeUsesCompleteText(t *testing.T) {
	tests := []struct {
		name          string
		words         int
		wantMinutes   int
		maxBytes      int
		wantTruncated bool
	}{
		{name: "one word minimum", words: 1, wantMinutes: 1, maxBytes: 100},
		{name: "exactly one minute", words: 200, wantMinutes: 1, maxBytes: 2000},
		{name: "rounds up", words: 201, wantMinutes: 2, maxBytes: 3000},
		{name: "uses original words after truncation", words: 401, wantMinutes: 3, maxBytes: 10, wantTruncated: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text := strings.TrimSpace(strings.Repeat("word ", tt.words))
			got, err := Prepare(text, tt.maxBytes)
			if err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			if got.WordCount != tt.words {
				t.Errorf("WordCount = %d, want %d", got.WordCount, tt.words)
			}
			if got.ReadingMinutes != tt.wantMinutes {
				t.Errorf("ReadingMinutes = %d, want %d", got.ReadingMinutes, tt.wantMinutes)
			}
			if got.Truncated != tt.wantTruncated {
				t.Errorf("Truncated = %v, want %v", got.Truncated, tt.wantTruncated)
			}
		})
	}
}
