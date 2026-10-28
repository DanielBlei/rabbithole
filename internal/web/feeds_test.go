// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"strings"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// intPtr / boolPtr build the optional per-feed knobs.
func intPtr(n int) *int    { return &n }
func boolPtr(b bool) *bool { return &b }

// durPtr builds a *config.Duration for a per-feed override.
func durPtr(d time.Duration) *config.Duration {
	v := config.Duration(d)
	return &v
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
			name: "no defaults names the built-in fallback",
			want: "since 7d (built-in) · uncapped",
		},
		{
			name:     "defaults set",
			defaults: config.FeedDefaults{Since: durPtr(72 * time.Hour), MaxItems: intPtr(10)},
			want:     "since 3d · max_items 10",
		},
		{
			name:     "disabled by default is called out",
			defaults: config.FeedDefaults{Enabled: boolPtr(false)},
			want:     "since 7d (built-in) · uncapped · disabled by default",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := defaultsSummary(c.defaults); got != c.want {
				t.Errorf("defaultsSummary = %q, want %q", got, c.want)
			}
		})
	}
}

func TestExplainFeedError(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string // a fragment of the explanation that identifies it
	}{
		{name: "404", raw: "http error: 404 Not Found", want: "does not exist"},
		{name: "429", raw: "429 Too Many Requests", want: "limiting how often"},
		{name: "not a feed", raw: "failed to detect feed type", want: "returns a web page"},
		{name: "deadline", raw: "context deadline exceeded", want: "took longer than 30 seconds"},
		{
			name: "client timeout",
			raw:  "Get \"https://x.invalid/rss\": Client.Timeout exceeded",
			want: "took longer than 30 seconds",
		},
		{name: "dns", raw: "dial tcp: lookup x.invalid: no such host", want: "does not resolve"},
		{name: "arxiv 503", raw: "arxiv returned 503 Service Unavailable", want: "arXiv is temporarily unavailable"},
		{name: "unknown falls back", raw: "boom", want: "The fetch failed."},
		{name: "empty falls back", raw: "", want: "The fetch failed."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := explainFeedError(c.raw)
			if !strings.Contains(got, c.want) {
				t.Errorf("explainFeedError(%q) = %q, want it to contain %q", c.raw, got, c.want)
			}
		})
	}
}

func TestExplainFeedErrorFirstMatchWins(t *testing.T) {
	// Both the 404 and the timeout patterns match; the earlier entry wins.
	got := explainFeedError("404 Not Found after timeout")
	if !strings.Contains(got, "does not exist") {
		t.Errorf("explainFeedError = %q, want the 404 explanation", got)
	}
}

func TestExplainFeedErrorTimeoutUsesFetchTimeout(t *testing.T) {
	if feeds.FetchTimeout != 30*time.Second {
		t.Skipf("fetch timeout is %v; the sentence is pinned to 30 seconds", feeds.FetchTimeout)
	}
	want := "The site took longer than 30 seconds to answer."
	if got := explainFeedError("deadline exceeded"); !strings.HasPrefix(got, want) {
		t.Errorf("timeout explanation = %q, want prefix %q", got, want)
	}
}

// healthRow builds a feed row from a fetch history, newest first.
func healthRow(t *testing.T, enabled bool, streak int, now time.Time, recent ...store.FeedAttempt) feedRowData {
	t.Helper()
	h := store.FeedHealth{
		FeedID:     "f1",
		URL:        "https://example.test/rss",
		LastFetch:  now.Add(-time.Hour),
		Status:     store.FeedStatusError,
		Error:      "http error: 500",
		FailStreak: streak,
		Recent:     recent,
	}
	if streak == 0 {
		h.Status = store.FeedStatusOK
	}
	f := config.ResolvedFeed{ID: "f1", Name: "Example", URL: h.URL, Enabled: enabled}
	return toFeedRow(f, h, now)
}

func TestToFeedRowFailingSince(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	fail := func(ago time.Duration) store.FeedAttempt {
		return store.FeedAttempt{Status: store.FeedStatusError, Error: "http error: 500", At: now.Add(-ago)}
	}
	ok := func(ago time.Duration) store.FeedAttempt {
		return store.FeedAttempt{Status: store.FeedStatusOK, Items: 1, At: now.Add(-ago)}
	}

	t.Run("streak ends at an ok attempt", func(t *testing.T) {
		row := healthRow(t, true, 3, now,
			fail(time.Hour), fail(24*time.Hour), fail(72*time.Hour), ok(96*time.Hour))
		if row.State != feedStateError {
			t.Fatalf("State = %q, want error", row.State)
		}
		want := now.Add(-72 * time.Hour)
		if !row.FailingSince.Equal(want) || row.FailingSinceAtLeast {
			t.Errorf("FailingSince = %v (atLeast %v), want %v exact", row.FailingSince, row.FailingSinceAtLeast, want)
		}
		wantPhrase := "failing since " + want.Local().Format("Jan 2") + " (3d ago)"
		if row.SincePhrase != wantPhrase {
			t.Errorf("SincePhrase = %q, want %q", row.SincePhrase, wantPhrase)
		}
		if row.Explain != explainFeedError("http error: 500") {
			t.Errorf("Explain = %q, want the fallback for an unmatched error", row.Explain)
		}
	})

	t.Run("history fully failing is at least", func(t *testing.T) {
		row := healthRow(t, true, 5, now,
			fail(time.Hour), fail(24*time.Hour), fail(48*time.Hour))
		want := now.Add(-48 * time.Hour)
		if !row.FailingSince.Equal(want) || !row.FailingSinceAtLeast {
			t.Errorf(
				"FailingSince = %v (atLeast %v), want %v at least",
				row.FailingSince,
				row.FailingSinceAtLeast,
				want,
			)
		}
		wantPhrase := "failing since at least " + want.Local().Format("Jan 2") + " (2d ago)"
		if row.SincePhrase != wantPhrase {
			t.Errorf("SincePhrase = %q, want %q", row.SincePhrase, wantPhrase)
		}
	})

	t.Run("no history is unknown", func(t *testing.T) {
		row := healthRow(t, true, 2, now)
		if !row.FailingSince.IsZero() || row.FailingSinceAtLeast {
			t.Errorf("FailingSince = %v (atLeast %v), want zero", row.FailingSince, row.FailingSinceAtLeast)
		}
		if row.SincePhrase != "failing" {
			t.Errorf("SincePhrase = %q, want %q", row.SincePhrase, "failing")
		}
	})

	t.Run("old run uses an absolute date", func(t *testing.T) {
		row := healthRow(t, true, 3, now,
			fail(time.Hour), fail(10*24*time.Hour), fail(20*24*time.Hour), ok(30*24*time.Hour))
		want := "failing since " + now.Add(-20*24*time.Hour).Local().Format("Jan 2")
		if row.SincePhrase != want {
			t.Errorf("SincePhrase = %q, want %q", row.SincePhrase, want)
		}
	})

	t.Run("ok row has no failure fields", func(t *testing.T) {
		row := healthRow(t, true, 0, now, ok(time.Hour))
		if row.State != feedStateOK || row.SincePhrase != "" || row.Explain != "" || !row.FailingSince.IsZero() {
			t.Errorf("ok row carries failure fields: state %q phrase %q explain %q",
				row.State, row.SincePhrase, row.Explain)
		}
	})
}

func TestToFeedRowTicks(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	at := func(ago time.Duration, status string) store.FeedAttempt {
		return store.FeedAttempt{Status: status, At: now.Add(-ago)}
	}

	t.Run("pads the start and marks a flaky newest tick", func(t *testing.T) {
		row := healthRow(t, true, 1, now,
			at(time.Hour, store.FeedStatusError), at(2*time.Hour, store.FeedStatusOK))
		if len(row.Ticks) != feedStripLen {
			t.Fatalf("len(Ticks) = %d, want %d", len(row.Ticks), feedStripLen)
		}
		for i := 0; i < feedStripLen-2; i++ {
			if row.Ticks[i].Class != "none" {
				t.Fatalf("Ticks[%d] = %q, want none", i, row.Ticks[i])
			}
		}
		if row.Ticks[feedStripLen-2].Class != "ok" || row.Ticks[feedStripLen-1].Class != "flaky" {
			t.Errorf("tail = %v, want [ok flaky]", row.Ticks[feedStripLen-2:])
		}
		if row.TickOK != 1 || row.TickErr != 1 {
			t.Errorf("TickOK/TickErr = %d/%d, want 1/1", row.TickOK, row.TickErr)
		}
	})

	t.Run("failing newest tick stays err", func(t *testing.T) {
		row := healthRow(t, true, 3, now,
			at(time.Hour, store.FeedStatusError), at(2*time.Hour, store.FeedStatusError),
			at(3*time.Hour, store.FeedStatusError))
		if row.Ticks[feedStripLen-1].Class != "err" {
			t.Errorf("newest tick = %q, want err", row.Ticks[feedStripLen-1].Class)
		}
		if row.TickErr != 3 || row.TickOK != 0 {
			t.Errorf("TickOK/TickErr = %d/%d, want 0/3", row.TickOK, row.TickErr)
		}
	})

	t.Run("caps at the strip length, newest kept", func(t *testing.T) {
		var recent []store.FeedAttempt
		for i := 0; i < feedStripLen+3; i++ {
			recent = append(recent, at(time.Duration(i+1)*time.Hour, store.FeedStatusOK))
		}
		row := healthRow(t, true, 0, now, recent...)
		if len(row.Ticks) != feedStripLen || row.TickOK != feedStripLen {
			t.Errorf("len = %d, TickOK = %d, want %d of each", len(row.Ticks), row.TickOK, feedStripLen)
		}
	})
}

func TestFetchSuccess(t *testing.T) {
	rows := []feedRowData{
		{State: feedStateOK, TickOK: 3, TickErr: 1},
		{State: feedStateFlaky, TickOK: 2},
		{State: feedStateError, TickErr: 4},
		{State: feedStateNever},
		{State: feedStateOff, TickOK: 5, TickErr: 5},
	}
	ok, total := fetchSuccess(rows)
	if ok != 5 || total != 10 {
		t.Errorf("fetchSuccess = %d/%d, want 5/10", ok, total)
	}
	if ok, total := fetchSuccess(nil); ok != 0 || total != 0 {
		t.Errorf("fetchSuccess(nil) = %d/%d, want 0/0", ok, total)
	}
}

func TestRatePercent(t *testing.T) {
	cases := []struct {
		ok, total int
		want      string
	}{
		{ok: 0, total: 0, want: "n/a"},
		{ok: 0, total: 5, want: "0%"},
		{ok: 2, total: 3, want: "67%"},
		{ok: 1, total: 3, want: "33%"},
		{ok: 23, total: 25, want: "92%"},
		{ok: 5, total: 5, want: "100%"},
	}
	for _, c := range cases {
		if got := ratePercent(c.ok, c.total); got != c.want {
			t.Errorf("ratePercent(%d, %d) = %q, want %q", c.ok, c.total, got, c.want)
		}
	}
}

// Each tick carries its own tooltip: the outcome and when, oldest first.
func TestToFeedTicksTitles(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	ticks, ok, errs := toFeedTicks([]store.FeedAttempt{
		{Status: store.FeedStatusError, Error: "dial tcp: no such host", At: now.Add(-time.Hour)},
		{Status: store.FeedStatusOK, Items: 2, At: now.Add(-2 * time.Hour)},
		{Status: store.FeedStatusError, At: now.Add(-3 * time.Hour)},
	}, now)
	if ok != 1 || errs != 2 {
		t.Errorf("ok/errs = %d/%d, want 1/2", ok, errs)
	}
	tail := ticks[feedStripLen-3:]
	want := []feedTick{
		{Class: "err", Title: "failed · 3h ago"},
		{Class: "ok", Title: "2 items · 2h ago"},
		{Class: "err", Title: "dial tcp: no such host · 1h ago"},
	}
	for i := range want {
		if tail[i] != want[i] {
			t.Errorf("tick %d = %+v, want %+v", i, tail[i], want[i])
		}
	}
	if ticks[0].Class != "none" || ticks[0].Title != "" {
		t.Errorf("padding tick = %+v, want an untitled none", ticks[0])
	}
	long := strings.Repeat("x", maxErrorText+50)
	ticks, _, _ = toFeedTicks([]store.FeedAttempt{{Status: store.FeedStatusError, Error: long, At: now}}, now)
	if got := ticks[feedStripLen-1].Title; !strings.HasPrefix(got, strings.Repeat("x", maxErrorText)+"…") {
		t.Errorf("a long error is not clipped in its tooltip: %.40s", got)
	}
}

// A removed source never carries a flaky tick, a health state or a failure
// explanation, whatever its history says; it keeps the strip.
func TestRemovedRowIsNeverFlaky(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	f := config.ResolvedFeed{ID: "f1", Name: "Gone", URL: "https://gone.test/feed", Enabled: true}
	h := store.FeedHealth{
		Status: store.FeedStatusError, Error: "503", LastFetch: now, FailStreak: 1,
		Recent: []store.FeedAttempt{
			{Status: store.FeedStatusError, Error: "503", At: now},
			{Status: store.FeedStatusOK, Items: 1, At: now.Add(-time.Hour)},
		},
	}
	row := removedRow(f, h, now)
	if !row.Deleted || row.State != feedStateOff || row.Explain != "" {
		t.Errorf("removed row = deleted %v state %q explain %q", row.Deleted, row.State, row.Explain)
	}
	if got := row.Ticks[feedStripLen-1].Class; got != "err" {
		t.Errorf("newest tick of a removed row = %q, want a plain err", got)
	}
	if row.TickOK != 1 || row.TickErr != 1 {
		t.Errorf("a removed row lost its history: %d ok %d err", row.TickOK, row.TickErr)
	}
	// A parked live source is the same.
	f.Enabled = false
	if row := toFeedRow(f, h, now); row.Ticks[feedStripLen-1].Class != "err" {
		t.Errorf("a parked row marks a flaky tick: %q", row.Ticks[feedStripLen-1].Class)
	}
}

// The quoted URL in a Go error is not part of the reason.
func TestExplainFeedErrorIgnoresTheQuotedURL(t *testing.T) {
	cases := []struct{ name, raw, want string }{
		{
			"404 in the URL",
			`Get "https://x/2024/404/feed": dial tcp: lookup x: no such host`,
			"The host name does not resolve",
		},
		{
			"timeout in the URL",
			`Get "https://x.test/timeout/feed.xml": dial tcp: lookup x.test: no such host`,
			"The host name does not resolve",
		},
		{
			"429 in the URL",
			`Get "https://x.test/429": failed to detect feed type`,
			"returns a web page, not a feed",
		},
		{"the error itself still matches", `Get "https://x.test/feed": 404 Not Found`, "does not exist"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := explainFeedError(c.raw); !strings.Contains(got, c.want) {
				t.Errorf("explainFeedError(%q) = %q, want it to contain %q", c.raw, got, c.want)
			}
		})
	}
}

func TestClipText(t *testing.T) {
	if got := clipText("short"); got != "short" {
		t.Errorf("clipText(short) = %q", got)
	}
	long := strings.Repeat("é", maxErrorText+1)
	got := clipText(long)
	if r := []rune(got); len(r) != maxErrorText+1 || r[maxErrorText] != '…' {
		t.Errorf("clipText cut to %d runes ending %q", len(r), string(r[len(r)-1]))
	}
}
