// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"strings"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// globalSince is the ingest.since these tests resolve "inherited from global"
// against.
const globalSince = 9 * 24 * time.Hour

// intPtr / boolPtr build the optional per-feed knobs.
func intPtr(n int) *int    { return &n }
func boolPtr(b bool) *bool { return &b }

// durPtr builds a *config.Duration for a per-feed override.
func durPtr(d time.Duration) *config.Duration {
	v := config.Duration(d)
	return &v
}

func TestToFeedStripOrdersOldestFirst(t *testing.T) {
	now := time.Now()
	// store returns Recent newest-first.
	attempts := []store.FeedAttempt{
		{Status: store.FeedStatusError, Error: "newest", At: now.Add(-1 * time.Hour)},
		{Status: store.FeedStatusOK, Items: 2, At: now.Add(-2 * time.Hour)},
		{Status: store.FeedStatusOK, Items: 1, At: now.Add(-3 * time.Hour)},
	}
	strip := toFeedStrip(attempts, now)
	if len(strip) != 3 {
		t.Fatalf("strip = %d ticks, want 3", len(strip))
	}
	// Display is oldest-first, so the newest attempt is last.
	if !strip[0].OK || !strip[1].OK || strip[2].OK {
		t.Errorf("strip OK flags = %v/%v/%v, want ok,ok,failed",
			strip[0].OK, strip[1].OK, strip[2].OK)
	}
	if !strings.Contains(strip[2].Title, "newest") {
		t.Errorf("last tick title = %q, want the newest attempt's error", strip[2].Title)
	}
	if toFeedStrip(nil, now) != nil {
		t.Error("no attempts should produce no strip")
	}
}

func TestShortDur(t *testing.T) {
	cases := []struct {
		name string
		in   time.Duration
		want string
	}{
		{name: "whole days", in: 7 * 24 * time.Hour, want: "7d"},
		{name: "hours rounding to days", in: 48 * time.Hour, want: "2d"},
		{name: "bare hours", in: 36 * time.Hour, want: "36h"},
		{name: "minutes", in: 90 * time.Minute, want: "90m"},
		{name: "sub-minute falls back to Go form", in: 90 * time.Second, want: "1m30s"},
		{name: "zero", in: 0, want: "—"},
		{name: "negative", in: -time.Hour, want: "—"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shortDur(c.in); got != c.want {
				t.Errorf("shortDur(%v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestCapLabel(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want string
	}{
		{name: "uncapped", in: 0, want: "—"},
		{name: "negative treated as uncapped", in: -3, want: "—"},
		{name: "capped", in: 25, want: "25"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := capLabel(c.in); got != c.want {
				t.Errorf("capLabel(%d) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestHostOf(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "https", in: "https://medium.com/feed/tag/ai", want: "medium.com"},
		{name: "http", in: "http://example.test/rss", want: "example.test"},
		{name: "with port", in: "http://localhost:8080/rss", want: "localhost:8080"},
		{name: "no scheme falls back to raw", in: "example.test/rss", want: "example.test/rss"},
		{name: "empty", in: "", want: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hostOf(c.in); got != c.want {
				t.Errorf("hostOf(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestItemsPhrase(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{in: 0, want: "0 items"},
		{in: 1, want: "1 item"},
		{in: 2, want: "2 items"},
	}
	for _, c := range cases {
		if got := itemsPhrase(c.in); got != c.want {
			t.Errorf("itemsPhrase(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDefaultsSummary(t *testing.T) {
	cases := []struct {
		name     string
		defaults config.FeedDefaults
		want     string
	}{
		{
			name: "no defaults names the global fallback",
			want: "since 9d (from ingest.since) · uncapped",
		},
		{
			name:     "defaults set",
			defaults: config.FeedDefaults{Since: durPtr(72 * time.Hour), MaxItems: intPtr(10)},
			want:     "since 3d · max_items 10",
		},
		{
			name:     "disabled by default is called out",
			defaults: config.FeedDefaults{Enabled: boolPtr(false)},
			want:     "since 9d (from ingest.since) · uncapped · disabled by default",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := defaultsSummary(c.defaults, globalSince); got != c.want {
				t.Errorf("defaultsSummary = %q, want %q", got, c.want)
			}
		})
	}
}
