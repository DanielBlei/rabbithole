// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"testing"
	"time"
)

func TestResolveDigest(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		period   string
		limit    int
		limitSet bool
		want     int
		wantErr  bool
	}{
		{name: "daily default", period: "daily", want: 10},
		{name: "weekly default", period: "weekly", want: 20},
		{name: "explicit limit", period: "daily", limit: 5, limitSet: true, want: 5},
		{name: "unknown period", period: "monthly", wantErr: true},
		{name: "explicit zero", period: "daily", limitSet: true, wantErr: true},
		{name: "negative limit", period: "daily", limit: -1, limitSet: true, wantErr: true},
		{name: "over hard maximum", period: "daily", limit: 51, limitSet: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start, end, limit, err := resolveDigest(tc.period, tc.limit, tc.limitSet, now)
			if tc.wantErr {
				if err == nil {
					t.Fatal("resolveDigest() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveDigest(): %v", err)
			}
			if limit != tc.want {
				t.Errorf("limit = %d, want %d", limit, tc.want)
			}
			if start.Location() != time.UTC || end.Location() != time.UTC || !start.Before(end) {
				t.Errorf("bounds = [%s, %s), want valid UTC period", start, end)
			}
		})
	}
}
