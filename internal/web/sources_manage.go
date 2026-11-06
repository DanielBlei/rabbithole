// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Filter keys of the Manage view menu. The same words are the ?state= deep link.
const (
	srcFilterAll      = "all"
	srcFilterEnabled  = "enabled"
	srcFilterDisabled = "disabled"
	srcFilterFailing  = "failing"
	srcFilterFlaky    = "flaky"
	srcFilterRemoved  = "removed"
)

// srcFilter is one option of the Manage view menu.
type srcFilter struct {
	Key   string
	Label string
	Count int
}

// Filters is the view menu's options, counts included. Failing and flaky count
// enabled sources only, and every option but Removed leaves removed sources out.
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

// ViewMenu is the Manage view menu: the Filters options, each labelled with its
// count, and the bare name on the button. It travels out of band after a
// mutation, so the counts stay true.
func (c sourceCounts) ViewMenu() listboxData {
	var opts []listboxOpt
	for _, f := range c.Filters() {
		opts = append(opts, listboxOpt{Value: f.Key, Label: fmt.Sprintf("%s (%d)", f.Label, f.Count), Short: f.Label})
	}
	return listboxData{
		ID: "srcFilter", Label: "View", Key: "View", Name: "srcfilter",
		Value: srcFilterAll, Default: srcFilterAll, Inline: true, OOB: c.OOB, Options: opts,
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

// tagChip is one of a row's tags with its palette index.
type tagChip struct {
	Name  string
	Color int
}

// TagChips lists every tag of the row. The palette index is the tag input's
// own hash, so the chips are already right when the page paints; the script
// recolours them if the reader has picked another colour for a tag. The page
// script decides how many of them fit the column.
func (r feedRowData) TagChips() []tagChip {
	out := make([]tagChip, len(r.Tags))
	for i, t := range r.Tags {
		out[i] = tagChip{Name: t, Color: tagColor(t)}
	}
	return out
}

// StateText is the short status under the fetch history. Never-fetched rows
// have none: the strip itself says they are waiting.
func (r feedRowData) StateText() string {
	switch {
	case r.Deleted && r.DeletedAgo != "":
		return "Removed " + r.DeletedAgo
	case r.Deleted:
		return "Removed"
	case r.State == feedStateOff:
		return "Disabled"
	case r.State == feedStateError:
		return fmt.Sprintf("Failing ×%d", r.FailStreak)
	case r.State == feedStateFlaky:
		return fmt.Sprintf("Flaky ×%d", r.FailStreak)
	case r.Stats.Quiet:
		return "Gone quiet"
	case r.State == feedStateOK && r.LastFetch != "":
		return "Fetched " + r.LastFetch
	}
	return ""
}

// StateTitle is the tooltip of the state text: the full time a removed source
// was removed. Empty for every other state.
func (r feedRowData) StateTitle() string {
	if !r.Deleted || r.DeletedAt.IsZero() {
		return ""
	}
	return "Removed " + r.DeletedAt.Local().Format("2 Jan 2006, 15:04")
}

// StateTone colours StateText: dim, bad or warn.
func (r feedRowData) StateTone() string {
	switch {
	case r.Deleted, r.State == feedStateOff:
		return "dim"
	case r.State == feedStateError:
		return "bad"
	case r.State == feedStateFlaky, r.Stats.Quiet:
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

// Sort keys of the Manage sort menu. The same words are the ?sort= deep link.
const (
	srcSortName   = "name"
	srcSortItems  = "items"
	srcSortSignal = "signal"
	srcSortLast   = "last"
)

// SortMenu is the Manage sort menu. The list is ordered by the script from the
// rows' data- attributes.
func (d sourcesData) SortMenu() listboxData {
	return listboxData{
		ID: "srcSortMenu", Label: "Sort sources", Key: "Sort", Name: "srcsort",
		Value: srcSortName, Default: srcSortName, Inline: true,
		Options: []listboxOpt{
			{Value: srcSortName, Label: "Name"},
			{Value: srcSortItems, Label: "Stored items per week"},
			{Value: srcSortSignal, Label: "Signal"},
			{Value: srcSortLast, Label: "Last item"},
		},
	}
}

// manageTag is one chip of the Manage tag box.
type manageTag struct {
	Name  string
	Count int // live sources carrying the tag
	Color int // tag palette index, as tagColor gives it
}

// TagChoices is every tag in use by a live source, with how many live sources
// carry it. Tags match without regard to case and the first spelling seen is
// the one shown. The box travels out of band after a mutation, so the counts
// stay true; the script restores the picked chip afterwards.
func (d sourcesData) TagChoices() []manageTag {
	type tagCount struct {
		name string
		n    int
	}
	var tags []tagCount
	at := make(map[string]int)
	for _, r := range d.Rows {
		if r.Deleted {
			continue
		}
		seen := make(map[string]bool)
		for _, t := range r.Tags {
			key := strings.ToLower(t)
			if seen[key] {
				continue
			}
			seen[key] = true
			i, ok := at[key]
			if !ok {
				i = len(tags)
				at[key] = i
				tags = append(tags, tagCount{name: t})
			}
			tags[i].n++
		}
	}
	slices.SortFunc(tags, func(a, b tagCount) int {
		return cmp.Compare(strings.ToLower(a.name), strings.ToLower(b.name))
	})
	out := make([]manageTag, len(tags))
	for i, t := range tags {
		out[i] = manageTag{Name: t.name, Count: t.n, Color: tagColor(t.name)}
	}
	return out
}

// PerWeekAttr is the row's data-perweek: the stored items per week, unrounded.
func (r feedRowData) PerWeekAttr() string {
	return strconv.FormatFloat(r.Stats.PerWeek, 'f', -1, 64)
}

// SignalAttr is the row's data-signal: the high-signal percentage, empty when
// there is not enough scored data to say.
func (r feedRowData) SignalAttr() string {
	if !r.Stats.Sig.Enough {
		return ""
	}
	return strconv.Itoa(r.Stats.Sig.Pct)
}

// LastAttr is the row's data-last: when its newest item was stored, in unix
// seconds, empty when it has none.
func (r feedRowData) LastAttr() string {
	if r.Stats.Last.IsZero() {
		return ""
	}
	return strconv.FormatInt(r.Stats.Last.Unix(), 10)
}

// SigTitle is the tooltip of the Signal cell: the counts behind the figure.
func (r feedRowData) SigTitle() string {
	g := r.Stats.Sig
	if !g.Enough {
		return fmt.Sprintf("Only %d scored so far (needs %d), %d unscored", g.Scored, minScored, g.Unscored)
	}
	return fmt.Sprintf("%d of %d scored are %s, %d unscored", g.High, g.Scored, highPlus(), g.Unscored)
}
