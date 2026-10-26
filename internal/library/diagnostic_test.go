// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package library

import "testing"

func TestDiagnosticString(t *testing.T) {
	tests := []struct {
		name string
		d    Diagnostic
		want string
	}{
		{"file line path", Diagnostic{"rss.yaml", 12, "blogs", "bad"}, "rss.yaml:12: blogs: bad"},
		{"file without line", Diagnostic{"rss.yaml", 0, "blogs", "bad"}, "rss.yaml: blogs: bad"},
		{"no path", Diagnostic{"rss.yaml", 3, "", "bad"}, "rss.yaml:3: bad"},
		{"empty file with path", Diagnostic{"", 0, "blogs", "bad"}, "blogs: bad"},
		{"empty file", Diagnostic{"", 0, "", "bad"}, "bad"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.d.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}
