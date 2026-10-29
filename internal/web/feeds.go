// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/httpclient"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// feedStripLen is how many past fetch attempts the per-feed history strip
// shows. Enough to see a pattern ("failing since three runs ago") without
// turning the row into a chart.
const feedStripLen = 12

// Feed row display states.
const (
	feedStateOK    = "ok"
	feedStateError = "error" // failing: the fail streak has reached failingStreak
	feedStateFlaky = "flaky" // the latest fetch failed, but the streak is short
	feedStateNever = "never" // configured but never fetched
	feedStateOff   = "off"   // parked
)

// failingStreak is the number of consecutive failed fetches that makes a feed
// failing rather than flaky.
const failingStreak = 3

// feedRowData is one feed's line on the Sources page. The *On strings name
// where an inherited value came from (rendered as the asterisk's tooltip); a
// value the feed sets itself has an empty *On.
type feedRowData struct {
	ID   string
	Name string
	URL  string
	Host string // URL's host — the compact label under the feed name
	Type string // resolved kind; the row badges anything but rss

	Enabled bool
	Deleted bool
	Since   string
	SinceOn string
	Cap     string // max_items, or "—" when uncapped
	CapOn   string
	Tags    []string
	// TagsAttr is Tags comma-joined — the row's data-tags, which the page's
	// client-side search matches against alongside the name and URL.
	TagsAttr string

	// Selected marks the feed whose editor is open, and Editor is that editor.
	Selected bool
	Editor   *feedDetail
	// OOB renders the row's unit as an out-of-band update to the one on the page.
	OOB bool

	// Health, aggregated from the feed's fetch history.
	State      string
	Detail     string // error message, or the item-count phrase
	LastFetch  string // relative time of the latest attempt, "" when never
	FailStreak int    // consecutive failures; >1 is worth showing

	// Explain is the plain-language reason behind an error or flaky row's
	// raw Detail.
	Explain string
	// FailingSince is when the current failure run began, zero when unknown.
	// FailingSinceAtLeast is set when the run is longer than the attempts on
	// record, so the true start is earlier than FailingSince.
	FailingSince        time.Time
	FailingSinceAtLeast bool
	SincePhrase         string // "failing since Jan 2 (3d ago)", or "failing"

	// Ticks is the last feedStripLen attempts, oldest first, each with its
	// own tooltip.
	Ticks []feedTick
	// TickOK and TickErr count the attempts in Ticks, for the strip's label
	// and the page's fetch success rate.
	TickOK, TickErr int
}

// feedTick is one attempt in the history strip. Class is "ok", "err", "flaky"
// (the newest attempt of a flaky row) or "none" (padding); Title is the hover
// text, the outcome and when.
type feedTick struct {
	Class string
	Title string
}

// toFeedRow shapes one resolved feed plus its health into a display row.
func toFeedRow(f config.ResolvedFeed, h store.FeedHealth, now time.Time) feedRowData {
	row := feedRowData{
		ID:       f.ID,
		Name:     f.Name,
		URL:      f.URL,
		Host:     hostOf(f.URL),
		Type:     string(f.Type),
		Enabled:  f.Enabled,
		Since:    shortDur(f.Since),
		Cap:      capLabel(f.MaxItems),
		Tags:     f.Tags,
		TagsAttr: strings.Join(f.Tags, ","),
	}
	if f.SinceFrom.Inherited() {
		row.SinceOn = string(f.SinceFrom)
	}
	if f.MaxItemsFrom.Inherited() {
		row.CapOn = string(f.MaxItemsFrom)
	}

	// A disabled feed reports as parked regardless of what its last fetch did:
	// stale history from before it was switched off would read as a live
	// problem. Its strip is still worth showing, since that is the point of
	// keeping history, so it is built either way.
	row.Ticks, row.TickOK, row.TickErr = toFeedTicks(h.Recent, now)
	if !f.Enabled {
		row.State, row.Detail = feedStateOff, "disabled"
		return row
	}

	switch {
	case h.LastFetch.IsZero():
		row.State, row.Detail = feedStateNever, "not fetched yet"
	case h.OK():
		row.State = feedStateOK
		row.Detail = itemsPhrase(h.Items)
		row.LastFetch = agoPhrase(h.LastFetch, now)
	default:
		row.State = feedStateFlaky
		if h.FailStreak >= failingStreak {
			row.State = feedStateError
		}
		row.Detail = h.Error
		row.Explain = explainFeedError(h.Error)
		row.LastFetch = agoPhrase(h.LastFetch, now)
		row.FailStreak = h.FailStreak
		row.FailingSince, row.FailingSinceAtLeast = failingSince(h)
		row.SincePhrase = sincePhrase(row.FailingSince, row.FailingSinceAtLeast, now)
	}
	// Only a live, enabled, flaky row marks its newest failure; parked and
	// removed rows returned above with their state final.
	if row.State == feedStateFlaky && row.Ticks[feedStripLen-1].Class == "err" {
		row.Ticks[feedStripLen-1].Class = "flaky"
	}
	return row
}

// removedRow shapes a removed feed. It goes through the cascade as a parked
// feed so it never carries a health state, a failure explanation or a flaky
// tick, and keeps its history for the strip.
func removedRow(f config.ResolvedFeed, h store.FeedHealth, now time.Time) feedRowData {
	f.Enabled = false
	row := toFeedRow(f, h, now)
	row.Deleted, row.Detail = true, "deleted"
	return row
}

// feedErrorExplanations maps raw fetch errors to plain-language reasons. The
// first match wins, so the more specific patterns come first.
var feedErrorExplanations = []struct {
	re   *regexp.Regexp
	text string
}{
	{
		regexp.MustCompile(regexp.QuoteMeta(httpclient.ErrPrivateRedirect.Error())),
		"The feed redirected to a private address, which is blocked.",
	},
	{
		regexp.MustCompile(regexp.QuoteMeta(httpclient.ErrBlockedAddress.Error()) + `.*\(private range\)`),
		"This address is on a private network. Set " + httpclient.AllowPrivateEnv + "=1 to allow private addresses.",
	},
	{
		regexp.MustCompile(regexp.QuoteMeta(httpclient.ErrBlockedAddress.Error())),
		"This address is blocked: link-local and cloud metadata addresses are never fetched.",
	},
	{
		regexp.MustCompile(regexp.QuoteMeta(httpclient.ErrTooLarge.Error())),
		fmt.Sprintf("The feed is larger than %d MiB.", feeds.MaxFeedBytes>>20),
	},
	{
		regexp.MustCompile(regexp.QuoteMeta(httpclient.ErrTooManyRedirects.Error())),
		"The feed redirects too many times.",
	},
	{
		regexp.MustCompile(regexp.QuoteMeta(httpclient.ErrInsecureRedirect.Error())),
		"The feed redirected from https to plain http, which is blocked.",
	},
	{
		regexp.MustCompile(regexp.QuoteMeta(httpclient.ErrUnsupportedScheme.Error())),
		"Only http and https addresses can be fetched.",
	},
	{
		regexp.MustCompile(`\b404\b`),
		"The site says this address does not exist. The feed probably moved: edit the URL, or remove the source.",
	},
	{
		regexp.MustCompile(`\b429\b`),
		"The site is limiting how often we fetch it. Wait a while and re-run ingestion; it usually clears on its own.",
	},
	{
		regexp.MustCompile(`failed to detect feed type`),
		"The address returns a web page, not a feed. Paste the feed link instead (often ends in /feed, /rss or .xml).",
	},
	{
		regexp.MustCompile(`(?i)deadline exceeded|timeout|timed out`),
		fmt.Sprintf(
			"The site took longer than %d seconds to answer. Often temporary: re-run ingestion, or disable it if it keeps happening.",
			int(feeds.FetchTimeout/time.Second),
		),
	},
	{
		regexp.MustCompile(`no such host`),
		"The host name does not resolve. Check the address for typos, or remove the source if the site is gone.",
	},
	{
		regexp.MustCompile(`arxiv returned 503`),
		"arXiv is temporarily unavailable. Usually clears on its own: re-run ingestion later.",
	},
}

// feedErrorFallback explains a failed fetch that matches no known pattern.
const feedErrorFallback = "The fetch failed. Re-run ingestion; if it keeps failing, edit the URL or remove the source."

// quotedText matches a double-quoted run. Go's fetch errors quote the URL they
// failed on, and a URL can contain "404" or "timeout".
var quotedText = regexp.MustCompile(`"[^"]*"`)

// explainFeedError returns the plain-language reason for a raw fetch error. The
// quoted URL is left out of the match, so only the error itself picks the reason.
func explainFeedError(raw string) string {
	raw = quotedText.ReplaceAllString(raw, `""`)
	for _, e := range feedErrorExplanations {
		if e.re.MatchString(raw) {
			return e.text
		}
	}
	return feedErrorFallback
}

// failingSince is when the current failure run began, from the store's own
// reading of the history. atLeast is set when every attempt on record failed
// and the streak is longer than them, so the start is earlier still.
func failingSince(h store.FeedHealth) (since time.Time, atLeast bool) {
	since, failing := h.FailingSince()
	if !failing {
		return time.Time{}, false
	}
	for _, a := range h.Recent {
		if a.OK() {
			return since, false
		}
	}
	return since, h.FailStreak > len(h.Recent)
}

// sincePhrase words a failure run's start for the row's status line.
func sincePhrase(since time.Time, atLeast bool, now time.Time) string {
	if since.IsZero() {
		return "failing"
	}
	prefix := "failing since "
	if atLeast {
		prefix += "at least "
	}
	date := since.Local().Format("Jan 2")
	ago := agoPhrase(since, now)
	// agoPhrase switches to an absolute date after a week, which would
	// repeat the date already shown.
	if strings.HasPrefix(ago, "on ") {
		return prefix + date
	}
	return prefix + date + " (" + ago + ")"
}

// toFeedTicks renders the newest feedStripLen attempts as oldest-first ticks,
// padding the start with "none" when there is less history. It also counts
// the ok and failed ticks.
func toFeedTicks(attempts []store.FeedAttempt, now time.Time) (ticks []feedTick, ok, errs int) {
	ticks = make([]feedTick, feedStripLen)
	for i := range ticks {
		ticks[i].Class = "none"
	}
	for i := 0; i < len(attempts) && i < feedStripLen; i++ {
		// attempts is newest-first, so the first one fills the last slot.
		a := attempts[i]
		tick := feedTick{Class: "ok", Title: itemsPhrase(a.Items) + " · " + agoPhrase(a.At, now)}
		if a.OK() {
			ok++
		} else {
			errs++
			tick.Class = "err"
			title := a.Error
			if title == "" {
				title = "failed"
			}
			tick.Title = clipText(title) + " · " + agoPhrase(a.At, now)
		}
		ticks[feedStripLen-1-i] = tick
	}
	return ticks, ok, errs
}

// fetchSuccess sums the recent attempts of the enabled, fetched rows.
// Parked and never-fetched rows are left out.
func fetchSuccess(rows []feedRowData) (ok, total int) {
	for _, r := range rows {
		switch r.State {
		case feedStateOK, feedStateFlaky, feedStateError:
			ok += r.TickOK
			total += r.TickOK + r.TickErr
		}
	}
	return ok, total
}

// ratePercent formats ok out of total as a rounded percentage, or "n/a" when
// there is nothing to measure.
func ratePercent(ok, total int) string {
	if total == 0 {
		return "n/a"
	}
	return strconv.Itoa(int(math.Round(100*float64(ok)/float64(total)))) + "%"
}

// itemsPhrase renders a fetch's item count with correct pluralization.
func itemsPhrase(n int) string {
	if n == 1 {
		return "1 item"
	}
	return strconv.Itoa(n) + " items"
}

// defaultsSummary renders the set-wide defaults as one line for the page's
// legend, naming the built-in value for anything they leave unset, so the
// cascade is legible without opening the defaults editor.
func defaultsSummary(d config.FeedDefaults) string {
	var parts []string
	if d.Since != nil {
		parts = append(parts, "since "+shortDur(d.Since.Std()))
	} else {
		parts = append(parts, "since "+shortDur(config.DefaultSince)+" (built-in)")
	}
	if d.MaxItems != nil {
		parts = append(parts, "max_items "+capLabel(*d.MaxItems))
	} else {
		parts = append(parts, "uncapped")
	}
	if d.Enabled != nil && !*d.Enabled {
		parts = append(parts, "disabled by default")
	}
	return strings.Join(parts, " · ")
}

// capLabel renders a max_items value; zero means uncapped.
func capLabel(n int) string {
	if n <= 0 {
		return "—"
	}
	return strconv.Itoa(n)
}

// shortDur renders a lookback window the way it's written in config — "7d",
// "36h", "90m" — rather than Go's "168h0m0s".
func shortDur(d time.Duration) string {
	switch {
	case d <= 0:
		return "—"
	case d%(24*time.Hour) == 0:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d"
	case d%time.Hour == 0:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	case d%time.Minute == 0:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	default:
		return d.String()
	}
}

// hostOf pulls the host out of a feed URL for the compact cell label, falling
// back to the raw string when it isn't a parseable absolute URL.
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	return u.Host
}

// maxErrorText caps a raw fetch error where it is shown; the full text rides in
// a title attribute.
const maxErrorText = 300

// clipText cuts s to maxErrorText characters, with an ellipsis when it was
// longer.
func clipText(s string) string {
	r := []rune(s)
	if len(r) <= maxErrorText {
		return s
	}
	return string(r[:maxErrorText]) + "…"
}
