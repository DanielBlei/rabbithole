// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import "testing"

func TestTtl(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"spaces become spans", "Model tuning", `Model<span class="sp"> </span>tuning`},
		{"single word unchanged", "Session", "Session"},
		{"less-than is escaped", "a <b> c", `a<span class="sp"> </span>&lt;b&gt;<span class="sp"> </span>c`},
		{"ampersand is escaped", "R & D", `R<span class="sp"> </span>&amp;<span class="sp"> </span>D`},
		{"empty is empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(ttl(tt.in)); got != tt.want {
				t.Errorf("ttl(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
