// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"sort"
	"sync"
	"time"
	"unicode/utf8"

	zlog "github.com/rs/zerolog/log"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/httpclient"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// How long an icon lookup stays current: a found or definitively missing icon
// is retried monthly, a failed lookup daily.
const (
	iconFreshFor      = 30 * 24 * time.Hour
	iconErrorRetry    = 24 * time.Hour
	iconErrorMaxChars = 200
	iconIDHexChars    = 16
)

// Pass bounds. They are variables so tests can shrink them.
var (
	iconMaxSites    = 20
	iconConcurrency = 4
	iconPassTimeout = 60 * time.Second
)

// IconSite is a feed and the site whose icon represents it. Fallback marks a
// site taken from the feed URL's own origin because no site link is known: it
// may fill a feed that has no current lookup but never refreshes one.
type IconSite struct {
	FeedID, FeedURL, SiteURL string
	Fallback                 bool
}

// IconStats counts the outcome of one RefreshIcons pass, in sites (one per site
// key). Deferred sites were stale but left for a later pass, by the site cap,
// the pass deadline or cancellation.
type IconStats struct {
	Stale    int
	OK       int
	None     int
	Errors   int
	Deferred int
}

// iconGroup is the stale feeds that share one site key and so one fetch.
type iconGroup struct {
	key    string
	site   string
	feeds  []IconSite
	rank   int
	oldest time.Time
}

// Staleness ranks, most urgent first.
const (
	rankNever    = iota // no icon row yet
	rankMismatch        // the row was computed for another URL or site
	rankAged            // the row outlived its retry window
)

// RefreshIcons looks up the icons of the feeds whose stored lookup is missing,
// out of date or stale, and records one row per feed. With sites nil it covers
// every live feed, using each feed URL's origin as the site. Only a site with a
// real link (a run's fetched feeds) refreshes an aged lookup or redoes one whose
// site link moved to another host; a fallback site fills what has no lookup for
// the current feed URL and leaves the rest. Feeds on one site key share a single
// fetch. At most iconMaxSites
// sites are fetched, iconConcurrency at a time, within iconPassTimeout; the rest
// wait for the next pass. A cancelled ctx writes nothing.
func RefreshIcons(
	ctx context.Context,
	db *store.Store,
	client *http.Client,
	sites []IconSite,
	now time.Time,
) IconStats {
	var stats IconStats
	if ctx.Err() != nil {
		return stats
	}
	if client == nil {
		client = feeds.NewClient("")
	}
	if sites == nil {
		resolved, err := ResolveFeeds(ctx, db)
		if err != nil {
			zlog.Warn().Err(err).Msg("reading feeds for icons failed")
			return stats
		}
		sites = originSites(resolved)
	}
	meta, err := db.FeedIconMeta(ctx)
	if err != nil {
		zlog.Warn().Err(err).Msg("reading stored icons failed")
		return stats
	}

	var rows []store.FeedIcon
	groups := staleGroups(sites, meta, now)
	var fetch []*iconGroup
	for _, g := range groups {
		stats.Stale++
		if feeds.NoIconHost(g.key) {
			stats.None++
			rows = append(rows, iconRows(g, store.IconStatusNone, "", nil, now)...)
			continue
		}
		fetch = append(fetch, g)
	}
	if len(fetch) > iconMaxSites {
		fetch = fetch[:iconMaxSites]
	}

	fetched := fetchGroups(ctx, client, fetch, now)
	for _, r := range fetched {
		switch r.status {
		case store.IconStatusOK:
			stats.OK++
		case store.IconStatusNone:
			stats.None++
		default:
			stats.Errors++
		}
		rows = append(rows, r.rows...)
	}
	stats.Deferred = stats.Stale - stats.OK - stats.None - stats.Errors

	if ctx.Err() != nil {
		stats.Deferred = stats.Stale
		stats.OK, stats.None, stats.Errors = 0, 0, 0
		return stats
	}
	if err := db.UpsertFeedIcons(ctx, rows); err != nil {
		zlog.Warn().Err(err).Msg("recording site icons failed")
		return stats
	}
	if stats.Stale > 0 {
		zlog.Info().Int("stale", stats.Stale).Int("ok", stats.OK).Int("none", stats.None).
			Int("errors", stats.Errors).Int("deferred", stats.Deferred).Msg("site icons refreshed")
	}
	return stats
}

// originSites lists every feed with its URL's origin as the site. Feeds whose
// URL is not http(s) have no site and are left out.
func originSites(resolved []config.ResolvedFeed) []IconSite {
	out := make([]IconSite, 0, len(resolved))
	for _, f := range resolved {
		if site := feeds.SiteOrigin(f.URL); site != "" {
			out = append(out, IconSite{FeedID: f.ID, FeedURL: f.URL, SiteURL: site, Fallback: true})
		}
	}
	return out
}

// staleGroups picks the feeds that need a lookup and groups them by site key,
// most urgent first: never tried, then moved, then oldest.
func staleGroups(sites []IconSite, meta map[string]store.FeedIcon, now time.Time) []*iconGroup {
	byKey := map[string]*iconGroup{}
	var order []*iconGroup
	seen := map[string]bool{}
	for _, s := range sites {
		key := feeds.SiteKey(s.SiteURL)
		if key == "" || seen[s.FeedID] {
			continue
		}
		seen[s.FeedID] = true
		rank, stale := iconStaleness(s, key, meta[s.FeedID], hasRow(meta, s.FeedID), now)
		if !stale {
			continue
		}
		g := byKey[key]
		if g == nil {
			g = &iconGroup{key: key, site: s.SiteURL, rank: rank, oldest: meta[s.FeedID].FetchedAt}
			byKey[key] = g
			order = append(order, g)
		}
		g.feeds = append(g.feeds, s)
		if rank < g.rank {
			g.rank = rank
		}
		if at := meta[s.FeedID].FetchedAt; at.Before(g.oldest) {
			g.oldest = at
		}
	}
	sort.SliceStable(order, func(i, j int) bool {
		a, b := order[i], order[j]
		if a.rank != b.rank {
			return a.rank < b.rank
		}
		if !a.oldest.Equal(b.oldest) {
			return a.oldest.Before(b.oldest)
		}
		return a.key < b.key
	})
	return order
}

func hasRow(meta map[string]store.FeedIcon, id string) bool {
	_, ok := meta[id]
	return ok
}

// iconStaleness reports whether the feed's icon needs a lookup and how urgently.
// A fallback site carries no news about the site, so it only fills a feed with no
// lookup for its current URL: it never counts as a move and never ages a row out.
func iconStaleness(
	s IconSite,
	key string,
	row store.FeedIcon,
	exists bool,
	now time.Time,
) (rank int, stale bool) {
	switch {
	case !exists:
		return rankNever, true
	case row.FeedURL != s.FeedURL:
		return rankMismatch, true
	case s.Fallback:
		return rankAged, false
	case row.SiteHost != key:
		return rankMismatch, true
	}
	window := iconFreshFor
	if row.Status == store.IconStatusError {
		window = iconErrorRetry
	}
	return rankAged, now.Sub(row.FetchedAt) > window
}

type iconResult struct {
	status string
	rows   []store.FeedIcon
}

// fetchGroups looks up each group's site, iconConcurrency at a time, within the
// pass deadline. A group the pass abandoned (deadline or cancellation) yields no
// result, so its rows stay as they were.
func fetchGroups(ctx context.Context, client *http.Client, groups []*iconGroup, now time.Time) []iconResult {
	if len(groups) == 0 {
		return nil
	}
	passCtx, cancel := context.WithTimeout(ctx, iconPassTimeout)
	defer cancel()

	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		out []iconResult
		sem = make(chan struct{}, iconConcurrency)
	)
launch:
	for _, g := range groups {
		select {
		case sem <- struct{}{}:
		case <-passCtx.Done():
			break launch
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			icon, err := feeds.FetchIcon(passCtx, client, g.site)
			if passCtx.Err() != nil {
				return
			}
			res := lookupResult(g, icon, err, now)
			mu.Lock()
			out = append(out, res)
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// lookupResult turns a fetch outcome into the rows to record for the group. A
// missing icon and a refused address are both final answers (none); anything
// else is a failure to retry soon.
func lookupResult(g *iconGroup, icon feeds.Icon, err error, now time.Time) iconResult {
	switch {
	case err == nil:
		return iconResult{store.IconStatusOK, iconRows(g, store.IconStatusOK, "", &icon, now)}
	case errors.Is(err, feeds.ErrNoIcon):
		return iconResult{store.IconStatusNone, iconRows(g, store.IconStatusNone, "", nil, now)}
	case errors.Is(err, httpclient.ErrBlockedAddress):
		return iconResult{store.IconStatusNone, iconRows(g, store.IconStatusNone, trimError(err), nil, now)}
	default:
		return iconResult{store.IconStatusError, iconRows(g, store.IconStatusError, trimError(err), nil, now)}
	}
}

// iconRows is one row per feed in the group, all sharing the group's outcome.
func iconRows(g *iconGroup, status, msg string, icon *feeds.Icon, now time.Time) []store.FeedIcon {
	rows := make([]store.FeedIcon, 0, len(g.feeds))
	for _, f := range g.feeds {
		row := store.FeedIcon{
			FeedID: f.FeedID, FeedURL: f.FeedURL, SiteHost: g.key,
			Status: status, Error: msg, FetchedAt: now,
		}
		if icon != nil {
			row.Mime = "image/png"
			row.Bytes = icon.PNG
			row.Dark = icon.Dark
			row.ETag = iconETag(icon.PNG)
		}
		rows = append(rows, row)
	}
	return rows
}

// iconETag is a quoted hex prefix of the icon's sha256.
func iconETag(png []byte) string {
	sum := sha256.Sum256(png)
	return `"` + hex.EncodeToString(sum[:])[:iconIDHexChars] + `"`
}

func trimError(err error) string {
	return clipBytes(err.Error(), iconErrorMaxChars)
}

// clipBytes cuts s to at most n bytes without splitting a rune.
func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// iconSites pairs each fetched feed with the site that stands for it: the
// feed's own site link when the fetch found one, else the origin of its URL
// (academic sources, failed fetches), marked as a fallback. results is
// positional with active.
func iconSites(active []config.ResolvedFeed, results []feeds.Result) []IconSite {
	out := make([]IconSite, 0, len(active))
	for i, f := range active {
		site, fallback := results[i].Site, false
		if feeds.SiteOrigin(site) == "" {
			site, fallback = feeds.SiteOrigin(f.URL), true
		}
		if site == "" {
			continue
		}
		out = append(out, IconSite{FeedID: f.ID, FeedURL: f.URL, SiteURL: site, Fallback: fallback})
	}
	return out
}
