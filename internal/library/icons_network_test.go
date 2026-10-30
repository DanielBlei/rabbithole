// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

//go:build network

package library

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/feeds"
)

const (
	iconsWriteEnv     = "RABBITHOLE_LIBRARY_ICONS_WRITE"
	iconsOnlyEnv      = "RABBITHOLE_LIBRARY_ICONS_ONLY_MISSING"
	iconsDropEnv      = "RABBITHOLE_LIBRARY_ICONS_DROP"
	iconsDeadline     = 15 * time.Minute
	iconsMaxHosts     = 4
	iconsHostPause    = time.Second
	iconsTransientTry = 3
	iconsRetryWait    = 5 * time.Second
)

// iconSource is one library source to find an icon for.
type iconSource struct {
	id, name, url string
	typ           Type
}

// iconResult is the outcome for one site: an icon, none, or a transient failure.
type iconResult struct {
	icon Icon
	none bool
	err  error
}

// TestLibraryIconsWrite fetches the icon of every library source's site through the app's icon
// fetcher and writes library/icons.json. It does nothing unless RABBITHOLE_LIBRARY_ICONS_WRITE=1;
// make library-icons sets it. A site that fails for a transient reason keeps its entry from the
// existing file, and the test fails when it has none, so a second run can fill the gap. A site that
// now has no icon keeps its existing icon too, unless RABBITHOLE_LIBRARY_ICONS_DROP=1. With
// RABBITHOLE_LIBRARY_ICONS_ONLY_MISSING=1 only sources absent from the existing file are fetched
// and every other entry is kept as it is.
func TestLibraryIconsWrite(t *testing.T) {
	if os.Getenv(iconsWriteEnv) != "1" {
		t.Skipf("set %s=1 (make library-icons) to fetch and write the icons", iconsWriteEnv)
	}
	lib, diags := ParseFS(os.DirFS(libraryDir()))
	if err := diags.Err(); err != nil {
		t.Fatal(err)
	}
	var srcs []iconSource
	for _, f := range lib.Files {
		for _, g := range f.Groups {
			for _, s := range g.Sources {
				srcs = append(srcs, iconSource{id: config.FeedID(s.URL), name: s.Name, url: s.URL, typ: f.Type})
			}
		}
	}
	if len(srcs) == 0 {
		t.Fatal("library has no sources")
	}

	var prev IconSet
	if data, err := os.ReadFile(filepath.Join(libraryDir(), IconsFile)); err == nil {
		prev, _ = ParseIcons(data)
	}

	// fetch lists the sources to look up; the rest keep their entry from the existing file.
	fetch := srcs
	if os.Getenv(iconsOnlyEnv) == "1" {
		fetch = nil
		for _, s := range srcs {
			if !prev.Known(s.id) {
				fetch = append(fetch, s)
			}
		}
		t.Logf("%d of %d sources are missing from the existing file", len(fetch), len(srcs))
	}
	dropNone := os.Getenv(iconsDropEnv) == "1"

	ctx, cancel := context.WithTimeout(t.Context(), iconsDeadline)
	defer cancel()
	client := feeds.NewClient("network-test")
	sites := &iconSites{client: client, byKey: map[string]*siteFetch{}}

	// One goroutine per feed host, so a host is never hit in parallel with itself.
	byHost := map[string][]iconSource{}
	for _, s := range fetch {
		host := feeds.SiteKey(s.url)
		byHost[host] = append(byHost[host], s)
	}
	hosts := slices.Collect(maps.Keys(byHost))
	slices.SortFunc(hosts, func(a, b string) int {
		if c := cmp.Compare(len(byHost[b]), len(byHost[a])); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	})

	results := map[string]*iconResult{}
	var mu sync.Mutex
	sem := make(chan struct{}, iconsMaxHosts)
	var wg sync.WaitGroup
	for _, host := range hosts {
		sem <- struct{}{}
		wg.Add(1)
		go func(group []iconSource) {
			defer wg.Done()
			defer func() { <-sem }()
			for n, s := range group {
				if n > 0 {
					sleepIcons(ctx, iconsHostPause)
				}
				res := sites.resolve(ctx, s)
				mu.Lock()
				results[s.id] = res
				mu.Unlock()
			}
		}(byHost[host])
	}
	wg.Wait()

	icons := map[string]Icon{}
	var none []string
	var gaps []string
	for _, s := range srcs {
		res := results[s.id]
		switch {
		case res == nil && prev.Icons[s.id].PNG != nil:
			icons[s.id] = prev.Icons[s.id]
		case res == nil:
			none = append(none, s.id) // not fetched, so the existing file lists it as none
		case res.err == nil && res.none && prev.Icons[s.id].PNG != nil && !dropNone:
			icons[s.id] = prev.Icons[s.id]
			t.Logf("kept the existing icon of %s although it now resolves to none (set %s=1 to drop it)",
				s.name, iconsDropEnv)
		case res.err == nil && res.none:
			none = append(none, s.id)
		case res.err == nil:
			icons[s.id] = res.icon
		case prev.Icons[s.id].PNG != nil:
			icons[s.id] = prev.Icons[s.id]
			t.Logf("kept the existing icon of %s after: %v", s.name, res.err)
		case slices.Contains(prev.None, s.id):
			none = append(none, s.id)
			t.Logf("kept the existing none of %s after: %v", s.name, res.err)
		default:
			gaps = append(gaps, s.name)
			t.Errorf("ICON-FAIL %s %s: %v", s.name, s.url, res.err)
		}
	}

	data := MarshalIcons(icons, none)
	if len(data) > MaxIconsBytes {
		t.Fatalf("icons file is %d bytes, over the %d budget", len(data), MaxIconsBytes)
	}
	if err := os.WriteFile(filepath.Join(libraryDir(), IconsFile), data, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d sources: %d icons, %d none, %d failed (%s), %d bytes",
		len(srcs), len(icons), len(none), len(gaps), strings.Join(gaps, ", "), len(data))
}

// iconSites fetches each site once, however many sources point at it.
type iconSites struct {
	client *http.Client
	mu     sync.Mutex
	byKey  map[string]*siteFetch
}

// siteFetch is the one fetch of a site key.
type siteFetch struct {
	once sync.Once
	res  *iconResult
}

// resolve finds the site of a source and its icon. An rss source's site is the link its feed
// declares, else the origin of its URL when the feed loads without one; a feed that cannot be
// loaded is a transient failure. An academic source's site is its URL's origin.
func (s *iconSites) resolve(ctx context.Context, src iconSource) *iconResult {
	site := feeds.SiteOrigin(src.url)
	if src.typ == TypeRSS {
		res := feeds.FetchAll(ctx, s.client, []feeds.Source{{Name: src.name, URL: src.url}})[0]
		if res.Err != nil {
			return &iconResult{err: fmt.Errorf("fetch feed: %w", res.Err)}
		}
		if res.Site != "" {
			site = res.Site
		}
	}
	key := feeds.SiteKey(site)
	s.mu.Lock()
	f := s.byKey[key]
	if f == nil {
		f = &siteFetch{}
		s.byKey[key] = f
	}
	s.mu.Unlock()
	f.once.Do(func() { f.res = s.fetch(ctx, site) })
	return f.res
}

func (s *iconSites) fetch(ctx context.Context, site string) *iconResult {
	if feeds.NoIconHost(feeds.SiteKey(site)) {
		return &iconResult{none: true}
	}
	var err error
	for try := range iconsTransientTry {
		if try > 0 {
			sleepIcons(ctx, iconsRetryWait)
		}
		var ic feeds.Icon
		ic, err = feeds.FetchIcon(ctx, s.client, site)
		switch {
		case err == nil:
			return &iconResult{icon: Icon{PNG: ic.PNG, Dark: ic.Dark, ETag: IconETag(ic.PNG)}}
		case errors.Is(err, feeds.ErrNoIcon):
			return &iconResult{none: true}
		}
	}
	return &iconResult{err: err}
}

func sleepIcons(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
