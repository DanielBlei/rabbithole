// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package digest

import (
	"strings"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/feeds"
)

func TestPeriodBounds(t *testing.T) {
	tests := []struct {
		name, period string
		now          time.Time
		start, end   time.Time
	}{
		{
			name: "previous UTC day despite local offset", period: "daily",
			now:   time.Date(2026, 10, 22, 3, 30, 0, 0, time.FixedZone("local", 2*60*60)),
			start: time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC),
			end:   time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "previous complete week from Monday", period: "weekly",
			now:   time.Date(2026, 10, 19, 11, 0, 0, 0, time.UTC),
			start: time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC),
			end:   time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "previous complete week midweek", period: "weekly",
			now:   time.Date(2026, 10, 22, 16, 0, 0, 0, time.UTC),
			start: time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC),
			end:   time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, end, err := PeriodBounds(tt.period, tt.now)
			if err != nil {
				t.Fatal(err)
			}
			if !start.Equal(tt.start) || !end.Equal(tt.end) {
				t.Fatalf("bounds = [%s, %s), want [%s, %s)", start, end, tt.start, tt.end)
			}
		})
	}
}

func TestPeriodBoundsRejectsUnknownPeriod(t *testing.T) {
	if _, _, err := PeriodBounds("monthly", time.Now()); err == nil {
		t.Fatal("PeriodBounds(monthly) error = nil")
	}
}

func TestRenderPeriod(t *testing.T) {
	start := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, 1)
	out := RenderPeriod("daily", start, end, []Article{{
		Item: feeds.Item{
			Title: "A *title*\n# <script>alert(1)</script>", Source: "Feed",
			Link: "https://example.test/a?x=1&y=2", Published: start.Add(time.Hour),
		},
		Score: 9, Reason: "Useful\n> injected quote\x1b[31m", EffectiveDate: start.Add(time.Hour),
	}})
	for _, want := range []string{
		"# Daily Reading Digest", "2026-10-18T00:00:00Z", "LLM relevance: 9/10",
		`A \*title\* \# \<script\>alert\(1\)\</script\>`, "Source: Feed",
		`Reason: Useful \> injected quote\[31m`,
		"Published: 18 Oct 2026", "<https://example.test/a?x=1&y=2>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b") || strings.Contains(out, "<script>") ||
		strings.Contains(out, "alert(1)</script>") {
		t.Errorf("untrusted content retained terminal/Markdown control structure:\n%s", out)
	}
}

func TestRenderPeriodUsesFirstSeenLabelAndEmptyMessage(t *testing.T) {
	start := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	article := Article{
		Item:  feeds.Item{Title: "Undated", Source: "S", Link: "https://example.test"},
		Score: 4, EffectiveDate: start.Add(time.Hour),
	}
	out := RenderPeriod("weekly", start, start.AddDate(0, 0, 7), []Article{article})
	if !strings.Contains(out, "First seen: 18 Oct 2026") {
		t.Errorf("missing first-seen date:\n%s", out)
	}
	out = RenderPeriod("daily", start, start.AddDate(0, 0, 1), nil)
	if !strings.Contains(out, "No scored articles found for this period.") {
		t.Errorf("empty report message missing:\n%s", out)
	}
}

func TestRenderPeriodSanitizesUnsafeLinks(t *testing.T) {
	start := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	out := RenderPeriod("daily", start, start.AddDate(0, 0, 1), []Article{
		{Item: feeds.Item{Title: "Markup", Link: "https://example.test/a><script>"}, EffectiveDate: start},
		{Item: feeds.Item{Title: "Scheme", Link: "javascript:alert(1)"}, EffectiveDate: start},
	})
	if strings.Contains(out, "<script>") || strings.Contains(out, "<javascript:") {
		t.Errorf("unsafe link became Markdown markup:\n%s", out)
	}
	if !strings.Contains(out, "https://example.test/a%3E%3Cscript%3E") ||
		!strings.Contains(out, "URL: javascript:alert\\(1\\)") {
		t.Errorf("unsafe links were not safely rendered:\n%s", out)
	}
}

func TestRenderPeriodHTTPLinkSchemes(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		wantLink string
	}{
		{name: "http", url: "http://example.com", wantLink: "<http://example.com>"},
		{name: "https", url: "https://example.com", wantLink: "<https://example.com>"},
		{name: "uppercase http", url: "HTTP://example.com", wantLink: "<http://example.com>"},
		{name: "uppercase https", url: "HTTPS://example.com", wantLink: "<https://example.com>"},
		{name: "mixed-case https", url: "Https://example.com", wantLink: "<https://example.com>"},
		{name: "javascript scheme", url: "javascript:alert(1)"},
		{name: "data scheme", url: "data:text/html,test"},
		{name: "file scheme", url: "file:///etc/passwd"},
		{name: "http without hostname", url: "http:///etc/passwd"},
		{name: "https without hostname", url: "https:/example.com"},
	}
	start := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := RenderPeriod("daily", start, start.AddDate(0, 0, 1), []Article{{
				Item: feeds.Item{Title: "Article", Link: tt.url}, EffectiveDate: start,
			}})
			if tt.wantLink != "" && !strings.Contains(out, tt.wantLink) {
				t.Errorf("accepted URL was not rendered as clickable Markdown %q:\n%s", tt.wantLink, out)
			}
			if tt.wantLink == "" && strings.Contains(out, "<"+tt.url) {
				t.Errorf("rejected URL generated an unsafe Markdown link:\n%s", out)
			}
		})
	}
}

func TestRenderExistingAPIRemainsUnchanged(t *testing.T) {
	out := Render(time.Date(2026, 6, 16, 9, 0, 0, 0, time.UTC), nil)
	if !strings.Contains(out, "# Reading digest — Tuesday, 16 June 2026") ||
		!strings.Contains(out, "No new items cleared the relevance threshold today.") {
		t.Errorf("existing Render output changed:\n%s", out)
	}
}
