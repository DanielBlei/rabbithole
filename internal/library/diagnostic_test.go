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
		{
			"file line path",
			Diagnostic{File: "rss.yaml", Line: 12, Path: "blogs", Message: "bad"},
			"rss.yaml:12: blogs: bad",
		},
		{
			"file without line",
			Diagnostic{File: "rss.yaml", Line: 0, Path: "blogs", Message: "bad"},
			"rss.yaml: blogs: bad",
		},
		{"no path", Diagnostic{File: "rss.yaml", Line: 3, Path: "", Message: "bad"}, "rss.yaml:3: bad"},
		{"empty file with path", Diagnostic{File: "", Line: 0, Path: "blogs", Message: "bad"}, "blogs: bad"},
		{"empty file", Diagnostic{File: "", Line: 0, Path: "", Message: "bad"}, "bad"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.d.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}
