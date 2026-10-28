// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"fmt"
	"strings"
	"unicode/utf16"
)

// Filter keys of the Manage pills. The same words are the ?state= deep link.
const (
	srcFilterAll      = "all"
	srcFilterEnabled  = "enabled"
	srcFilterDisabled = "disabled"
	srcFilterFailing  = "failing"
	srcFilterFlaky    = "flaky"
	srcFilterRemoved  = "removed"
)

// srcFilter is one pill of the Manage filter group.
type srcFilter struct {
	Key   string
	Label string
	Count int
}

// Zero reports whether the pill has nothing behind it and should be dimmed.
// All is never dimmed.
func (f srcFilter) Zero() bool { return f.Count == 0 && f.Key != srcFilterAll }

// Filters is the pill group, counts included. Failing and flaky count enabled
// sources only, and every pill but Removed leaves removed sources out.
func (c sourceCounts) Filters() []srcFilter {
	return []srcFilter{
		{srcFilterAll, "All", c.Total},
		{srcFilterEnabled, "Enabled", c.Enabled},
		{srcFilterDisabled, "Disabled", c.Total - c.Enabled},
		{srcFilterFailing, "Failing", c.Failing},
		{srcFilterFlaky, "Flaky", c.Flaky},
		{srcFilterRemoved, "Removed", c.Deleted},
	}
}

// Summary is the line under the page heading, re-rendered after every mutation.
func (c sourceCounts) Summary() string {
	if c.Total == 0 {
		return "No sources yet"
	}
	out := plural(c.Total, "source", "sources") + fmt.Sprintf(" · %d enabled · ", c.Enabled)
	if c.Failing > 0 {
		out += fmt.Sprintf("%d failing", c.Failing)
	} else {
		out += "none failing"
	}
	if c.Flaky > 0 {
		out += fmt.Sprintf(" · %d flaky", c.Flaky)
	}
	return out
}

// attnFlaky is one flaky source's line in the attention block.
type attnFlaky struct {
	ID   string
	Name string
	// Need is how many more misses in a row make the source failing, worded.
	Need string
}

// sourcesAttention is the Manage status block: the failing sources with what to
// do about them, the flaky ones as notes, and the rerun bar.
type sourcesAttention struct {
	// OOB renders the block as an out-of-band update, after a mutation.
	OOB bool
	// Total is the live source count; with none the block is empty.
	Total   int
	Failing []feedRowData
	Flaky   []attnFlaky
	Bar     ingestBarData
}

// newAttention picks the failing and flaky rows out of the page's rows.
// Parked and removed rows carry State off, so only enabled sources land here.
func newAttention(rows []feedRowData, total int, bar ingestBarData) sourcesAttention {
	a := sourcesAttention{Total: total, Bar: bar}
	for _, r := range rows {
		switch r.State {
		case feedStateError:
			a.Failing = append(a.Failing, r)
		case feedStateFlaky:
			need := missesPhrase(failingStreak - r.FailStreak)
			a.Flaky = append(a.Flaky, attnFlaky{ID: r.ID, Name: r.Name, Need: need})
		}
	}
	return a
}

// missesPhrase words how many more failed fetches in a row tip a flaky source
// into failing; there is always at least one.
func missesPhrase(need int) string {
	switch {
	case need <= 1:
		return "One more miss"
	case need == 2:
		return "Two more misses"
	default:
		return fmt.Sprintf("%d more misses", need)
	}
}

// NoteBar is the data for the running note. The note polls itself when the page
// has no ingest bar to carry it, which is whenever nothing is failing.
func (a sourcesAttention) NoteBar() ingestBarData {
	n := a.Bar
	n.Poll = a.Total == 0 || len(a.Failing) == 0
	return n
}

// Names lists the failing sources, shown beside the title while the block is
// collapsed.
func (a sourcesAttention) Names() string {
	names := make([]string, len(a.Failing))
	for i, r := range a.Failing {
		names[i] = r.Name
	}
	return strings.Join(names, ", ")
}

// RunPhrase is the tail of the "Nothing failing." line: when ingestion last ran.
// Empty while a run is live.
func (a sourcesAttention) RunPhrase() string {
	switch {
	case a.Bar.Running:
		return ""
	case a.Bar.Status == "none":
		return "No ingestion run yet."
	default:
		return "Last run " + a.Bar.When + "."
	}
}

// ShortHost is the feed's host without a leading www.
func (r feedRowData) ShortHost() string {
	return strings.TrimPrefix(r.Host, "www.")
}

// KindNote tags a source that is not a plain feed, for the line under its name.
func (r feedRowData) KindNote() string {
	if r.Type == "" || r.Type == "rss" {
		return ""
	}
	return " · " + r.Type
}

// FirstTag is the tag the row shows; the rest collapse into a count.
func (r feedRowData) FirstTag() string {
	if len(r.Tags) == 0 {
		return ""
	}
	return r.Tags[0]
}

// FirstTagColor is the palette index of the first tag by the tag input's own
// hash, so the chip is already right when the page paints. The script then
// recolours it if the reader has picked another colour for that tag.
func (r feedRowData) FirstTagColor() int {
	return tagColor(r.FirstTag())
}

// MoreTags is how many tags sit behind the first.
func (r feedRowData) MoreTags() int {
	if len(r.Tags) < 2 {
		return 0
	}
	return len(r.Tags) - 1
}

// MoreTagsTitle names the tags behind the first, for the count's tooltip.
func (r feedRowData) MoreTagsTitle() string {
	if len(r.Tags) < 2 {
		return ""
	}
	return strings.Join(r.Tags[1:], ", ")
}

// StateText is the short status under the fetch history. Never-fetched rows
// have none: the strip itself says they are waiting.
func (r feedRowData) StateText() string {
	switch {
	case r.Deleted:
		return "Removed"
	case r.State == feedStateOff:
		return "Disabled"
	case r.State == feedStateError:
		return fmt.Sprintf("Failing ×%d", r.FailStreak)
	case r.State == feedStateFlaky:
		return fmt.Sprintf("Flaky ×%d", r.FailStreak)
	case r.State == feedStateOK && r.LastFetch != "":
		return "Fetched " + r.LastFetch
	}
	return ""
}

// StateTone colours StateText: dim, bad or warn.
func (r feedRowData) StateTone() string {
	switch {
	case r.Deleted, r.State == feedStateOff:
		return "dim"
	case r.State == feedStateError:
		return "bad"
	case r.State == feedStateFlaky:
		return "warn"
	}
	return "dim"
}

// Dim reports whether the history strip is shown muted: the source is not
// being fetched.
func (r feedRowData) Dim() bool {
	return r.Deleted || r.State == feedStateOff
}

// StripLabel describes the history strip for assistive tech and the tooltip.
func (r feedRowData) StripLabel() string {
	return fmt.Sprintf("Last %d fetches, newest on the right: %d ok, %d failed", feedStripLen, r.TickOK, r.TickErr)
}

// tagColor maps a tag name to one of the eight tag palette classes. It is the
// hash static/js/tagput.js uses, so server and script agree.
func tagColor(name string) int {
	h := uint32(5381)
	for _, u := range utf16.Encode([]rune(strings.ToLower(name))) {
		h = h<<5 + h + uint32(u)
	}
	return int(h % 8)
}
