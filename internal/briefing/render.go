// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package briefing

import (
	"fmt"
	"net/url"
	"strings"
	"unicode"

	"github.com/DanielBlei/rabbithole/internal/store"
)

// Render builds the deterministic markdown body stored with a briefing. It is
// the only renderer: the web page and the markdown export read the same bytes.
func Render(p Period, items []store.BriefingItem, b Briefing) string {
	var out strings.Builder
	label := "Daily"
	if p.Kind == "weekly" {
		label = "Weekly"
	}
	fmt.Fprintf(&out, "# %s Briefing — %s\n\n", label, periodTitle(p))
	fmt.Fprintf(&out, "_Period: %s to %s (UTC; start inclusive, end exclusive)_\n\n",
		p.Start.Format("2 Jan 2006"), p.End.Format("2 Jan 2006"))

	if len(items) == 0 {
		out.WriteString("No scored articles were found for this period.\n")
		return out.String()
	}

	out.WriteString("## Executive summary\n\n")
	out.WriteString(strings.TrimSpace(b.ExecutiveSummary))
	out.WriteString("\n\n")

	out.WriteString("## Key developments\n\n")
	for _, d := range b.Developments {
		fmt.Fprintf(&out, "### %s\n\n", markdownText(d.Headline))
		out.WriteString(strings.TrimSpace(d.Synthesis))
		if why := strings.TrimSpace(d.WhyItMatters); why != "" {
			fmt.Fprintf(&out, "\n\n**Why it matters:** %s", why)
		}
		out.WriteString("\n\n**Supporting articles:**\n\n")
		for _, n := range d.ItemIDs {
			if n < 1 || n > len(items) {
				continue
			}
			fmt.Fprintf(&out, "- %s\n", articleLine(items[n-1]))
		}
		out.WriteString("\n")
	}

	if len(b.EmergingTopics) > 0 {
		out.WriteString("## Emerging topics\n\n")
		for _, topic := range b.EmergingTopics {
			fmt.Fprintf(&out, "- %s\n", markdownText(topic))
		}
		out.WriteString("\n")
	}

	out.WriteString("## Recommended reading\n\n")
	if len(b.RecommendedReading) == 0 {
		out.WriteString("_No recommendation was made for this period._\n")
	} else {
		for i, n := range b.RecommendedReading {
			if n < 1 || n > len(items) {
				continue
			}
			fmt.Fprintf(&out, "%d. %s\n", i+1, articleLine(items[n-1]))
		}
	}
	return out.String()
}

func periodTitle(p Period) string {
	if p.Kind == "weekly" {
		return "week of " + p.Start.Format("Monday, 2 January 2006")
	}
	return p.Start.Format("Monday, 2 January 2006")
}

func articleLine(item store.BriefingItem) string {
	title := markdownText(item.Title)
	link := markdownURL(item.Link)
	source := markdownText(item.Source)
	switch {
	case link != "" && source != "":
		return fmt.Sprintf("[%s](%s) — %s", title, link, source)
	case link != "":
		return fmt.Sprintf("[%s](%s)", title, link)
	default:
		return title
	}
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
	if err != nil || u.Host == "" ||
		(!strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https")) {
		return ""
	}
	return strings.NewReplacer("<", "%3C", ">", "%3E", " ", "%20").Replace(u.String())
}
