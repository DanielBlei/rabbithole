// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package feeds

import "testing"

func TestIsCommentsFeed(t *testing.T) {
	tests := []struct {
		ref  feedRef
		want bool
	}{
		{feedRef{title: "Comments on: A post"}, true},
		{feedRef{url: "https://example.com/comments/feed/"}, true},
		{feedRef{title: "Daily Commentary"}, false},
		{feedRef{url: "https://example.com/feed", title: "Example"}, false},
	}
	for _, tt := range tests {
		if got := isCommentsFeed(tt.ref); got != tt.want {
			t.Errorf("isCommentsFeed(%+v) = %v, want %v", tt.ref, got, tt.want)
		}
	}
}
