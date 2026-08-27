// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"strings"
	"testing"
)

// A link in a why or a note leaves for a new tab, so following a reference
// doesn't cost you your place in the feed — but only when it actually leads out
// of the app, and never at the expense of the sanitiser.
func TestRenderMarkdownLinks(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		want    []string
		notWant []string
	}{
		{
			name: "outbound link opens in a new tab",
			src:  "see [the paper](https://arxiv.org/abs/1234)",
			want: []string{`href="https://arxiv.org/abs/1234"`, `target="_blank"`, `noopener`},
		},
		{
			name:    "relative link stays in this tab",
			src:     "back to [the feed](/feed)",
			want:    []string{`href="/feed"`},
			notWant: []string{`target="_blank"`},
		},
		{
			// Inline rather than on its own line: a bare tag opens an HTML
			// block, and the sanitiser drops the block whole, prose included —
			// which would prove nothing about the tag itself. The tag goes and
			// its text is left behind as prose, which is the policy's contract:
			// nothing can execute, and no content is silently lost.
			name:    "script tags are still stripped",
			src:     "text <script>alert(1)</script> more",
			want:    []string{"text", "more"},
			notWant: []string{"<script"},
		},
		{
			name:    "javascript: url is still dropped",
			src:     "[click](javascript:alert(1))",
			want:    []string{"click"},
			notWant: []string{"javascript:"},
		},
		{
			name: "empty source renders nothing",
			src:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(renderMarkdown(tt.src))
			if tt.src == "" && got != "" {
				t.Fatalf("renderMarkdown(%q) = %q, want empty", tt.src, got)
			}
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("renderMarkdown(%q) = %q, missing %q", tt.src, got, want)
				}
			}
			for _, notWant := range tt.notWant {
				if strings.Contains(got, notWant) {
					t.Errorf("renderMarkdown(%q) = %q, should not contain %q", tt.src, got, notWant)
				}
			}
		})
	}
}
