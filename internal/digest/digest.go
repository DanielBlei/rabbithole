// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

// Package digest turns ranked results into a dated markdown digest file.
package digest

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/rank"
)

// Article is a selected stored article for a period report. EffectiveDate is
// publication time or first-seen time when publication time is unavailable.
type Article struct {
	Item          feeds.Item
	Score         int
	Reason        string
	EffectiveDate time.Time
}

// PeriodBounds returns the last completed UTC calendar period relative to now.
// Daily periods are UTC days; weekly periods run Monday to Monday.
func PeriodBounds(period string, now time.Time) (time.Time, time.Time, error) {
	now = now.UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	switch period {
	case "daily":
		return day.AddDate(0, 0, -1), day, nil
	case "weekly":
		weekday := int(day.Weekday())
		mondayOffset := (weekday + 6) % 7
		thisMonday := day.AddDate(0, 0, -mondayOffset)
		return thisMonday.AddDate(0, 0, -7), thisMonday, nil
	default:
		return time.Time{}, time.Time{}, fmt.Errorf("unknown period %q (want daily or weekly)", period)
	}
}

// DefaultLimit is the default number of recommendations for a period.
func DefaultLimit(period string) (int, error) {
	switch period {
	case "daily":
		return 10, nil
	case "weekly":
		return 20, nil
	default:
		return 0, fmt.Errorf("unknown period %q (want daily or weekly)", period)
	}
}

// RenderPeriod builds a Markdown report for a bounded daily or weekly selection.
func RenderPeriod(period string, start, end time.Time, articles []Article) string {
	var b strings.Builder
	label := "Reading"
	if period == "daily" || period == "weekly" {
		label = strings.ToUpper(period[:1]) + period[1:]
	}
	fmt.Fprintf(&b, "# %s Reading Digest\n\n", label)
	fmt.Fprintf(&b, "Period: %s to %s (UTC; start inclusive, end exclusive)\n\n",
		start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339))
	if len(articles) == 0 {
		b.WriteString("No scored articles found for this period.\n")
		return b.String()
	}
	fmt.Fprintf(&b, "%d article(s), ranked by LLM relevance.\n\n", len(articles))
	for i, article := range articles {
		fmt.Fprintf(&b, "%d. **LLM relevance: %d/10 — %s**\n\n", i+1, article.Score, markdownText(article.Item.Title))
		fmt.Fprintf(&b, "   Source: %s\n\n", markdownText(article.Item.Source))
		if reason := markdownText(article.Reason); reason != "" {
			fmt.Fprintf(&b, "   Reason: %s\n\n", reason)
		}
		dateLabel := "First seen"
		if !article.Item.Published.IsZero() {
			dateLabel = "Published"
		}
		fmt.Fprintf(&b, "   %s: %s\n\n", dateLabel, article.EffectiveDate.UTC().Format("2 Jan 2006 15:04 UTC"))
		if link := markdownURL(article.Item.Link); link != "" {
			fmt.Fprintf(&b, "   <%s>\n\n", link)
		} else {
			fmt.Fprintf(&b, "   URL: %s\n\n", markdownText(article.Item.Link))
		}
	}
	return b.String()
}

func markdownText(value string) string {
	value = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
	var b strings.Builder
	for _, r := range value {
		if strings.ContainsRune("\\`*_{}[]()#+-.!|<>", r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

func markdownURL(value string) string {
	clean := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
	u, err := url.Parse(clean)
	if err != nil || u.Host == "" || (!strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https")) {
		return ""
	}
	return strings.NewReplacer("<", "%3C", ">", "%3E", " ", "%20").Replace(u.String())
}

// Render builds the markdown document for a day's digest.
func Render(day time.Time, results []rank.Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Reading digest — %s\n\n", day.Format("Monday, 2 January 2006"))
	if len(results) == 0 {
		b.WriteString("_No new items cleared the relevance threshold today._\n")
		return b.String()
	}
	fmt.Fprintf(&b, "%d item(s) worth your time, best first.\n\n", len(results))
	for _, r := range results {
		fmt.Fprintf(&b, "## [%d/10] %s\n\n", r.Score, r.Item.Title)
		if r.Reason != "" {
			fmt.Fprintf(&b, "> %s\n\n", r.Reason)
		}
		meta := r.Item.Source
		if !r.Item.Published.IsZero() {
			meta += " · " + r.Item.Published.Format("2 Jan 2006")
		}
		fmt.Fprintf(&b, "%s\n", meta)
		fmt.Fprintf(&b, "<%s>\n\n", r.Item.Link)
	}
	return b.String()
}

// Write renders the digest and writes it to dir/YYYY-MM-DD.md, returning the path.
func Write(dir string, day time.Time, results []rank.Result) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create output dir: %w", err)
	}
	path := filepath.Join(dir, day.Format("2006-01-02")+".md")
	if err := os.WriteFile(path, []byte(Render(day, results)), 0o644); err != nil {
		return "", fmt.Errorf("write digest: %w", err)
	}
	return path, nil
}
