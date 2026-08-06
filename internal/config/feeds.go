// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// defaultFeedsFileName is the feeds file Load looks for next to the config
// file when `ingest.feeds` isn't set.
const defaultFeedsFileName = "feeds.yaml"

// feedIDLen is how many hex characters of the URL digest make up a feed ID.
// 12 hex chars (48 bits) makes an accidental collision across a personal feed
// list statistically impossible while staying readable in a log line.
const feedIDLen = 12

// Feed is one RSS/Atom source as declared, before the defaults cascade runs.
// Feeds live in the store; this is the shape they take in the seed file and in
// the exported YAML, and the shape the store hands back. Every knob past
// name/url is a pointer so an unset field is distinguishable from a zero value
// and can fall through to the defaults — see resolveFeed for the cascade. The
// store spells the same distinction as a NULL column.
type Feed struct {
	// ID is empty in the seed file and in the export — it is not something you
	// declare. The store fills it in on the way out so callers can address a
	// feed without going through its name or URL, both of which are editable.
	ID   string `yaml:"-"`
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
	// omitempty on every optional knob: an unset one has to come back out of the
	// export as an absent key, not as `enabled: null`. Absent is what "inherit"
	// looks like in a hand-written file, and it is what re-importing reads back.
	Enabled  *bool     `yaml:"enabled,omitempty"`   // park a feed without deleting it
	Since    *Duration `yaml:"since,omitempty"`     // per-feed lookback window
	MaxItems *int      `yaml:"max_items,omitempty"` // cap on newest items contributed per run
	Tags     []string  `yaml:"tags,omitempty"`      // free-form labels, unioned with the defaults'
}

// FeedDefaults are the set-wide fallbacks applied to any feed that doesn't set
// a knob itself. Tags are the exception: they union onto every feed's own tags
// rather than being overridden by them.
type FeedDefaults struct {
	Enabled  *bool     `yaml:"enabled,omitempty"`
	Since    *Duration `yaml:"since,omitempty"`
	MaxItems *int      `yaml:"max_items,omitempty"`
	Tags     []string  `yaml:"tags,omitempty"`
}

// FeedsDoc is a whole feed set in its declared form: the defaults plus the
// feeds. It is what the seed file parses into and what the export renders from,
// so a set can make the round trip out of the store and back into a fresh one.
type FeedsDoc struct {
	Defaults FeedDefaults `yaml:"defaults"`
	Feeds    []Feed       `yaml:"feeds"`
}

// Origin records where a resolved value came from, so the read-only viewer can
// show which knobs a feed sets itself and which it inherits.
type Origin string

const (
	OriginFeed     Origin = "feed"     // set on the feed itself
	OriginDefaults Origin = "defaults" // from the feeds file's defaults block
	OriginGlobal   Origin = "global"   // from the main config (ingest.since)
	OriginBuiltin  Origin = "builtin"  // no one set it; the compiled-in fallback
)

// Inherited reports whether the value came from somewhere other than the feed
// entry itself — what the viewer marks with an asterisk.
func (o Origin) Inherited() bool { return o != OriginFeed }

// ResolvedFeed is a Feed with every knob resolved through the cascade
// (feed → defaults → global config → built-in), carrying the provenance of each
// resolved value alongside it. This is what the ingest cycle and the Sources
// page both consume; nothing downstream re-implements the fallback rules.
type ResolvedFeed struct {
	// ID is the feed's stable identity. It is minted from the URL when the feed
	// is first stored (see FeedID) and frozen from then on, so persisted state —
	// fetch history — survives both a rename and a change of URL.
	ID       string
	Name     string
	URL      string
	Enabled  bool
	Since    time.Duration
	MaxItems int // 0 means uncapped
	Tags     []string

	EnabledFrom  Origin
	SinceFrom    Origin
	MaxItemsFrom Origin
}

// uncapped is the MaxItems value meaning "take every item in the window".
const uncapped = 0

// FeedID mints a feed's identity from its URL. It is called once, when a feed
// is first added to the store, and the result is kept as that row's primary key
// forever after — so a later edit to either the name or the URL keeps the
// feed's fetch history rather than starting a new one.
//
// Deriving it from the URL rather than using a counter means a feed re-added
// after being deleted, or seeded again into a fresh database, lands on the same
// ID and picks its history back up.
func FeedID(url string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(url)))
	return hex.EncodeToString(sum[:])[:feedIDLen]
}

// NormalizeFeedURL fills in a missing scheme with https, so "example.com/feed"
// is a URL rather than a validation error.
func NormalizeFeedURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || strings.Contains(trimmed, "://") {
		return trimmed
	}
	return "https://" + strings.TrimPrefix(trimmed, "//")
}

// InsecureFeedURL reports whether a feed would be fetched over plain http —
// what the Sources page warns about under the field.
func InsecureFeedURL(raw string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(raw)), "http://")
}

// ValidateFeedURL checks that a feed URL is one the fetcher could actually
// call. The seed file was hand-written and could be trusted to hold something
// URL-shaped; a form field cannot, and "asdf" should fail on the way in rather
// than as a fetch error on the next run.
func ValidateFeedURL(raw string) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return fmt.Errorf("url is required")
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return fmt.Errorf("invalid url %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("url must start with http:// or https://, got %q", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("url %q has no host", raw)
	}
	return nil
}

// FeedsFilePath returns the seed file to read and whether the user named it
// explicitly. An explicit ingest.feeds is taken as written (resolved against the
// process working directory, like `profile`); when unset, the default sits
// beside the config file so moving the config directory keeps them together.
func (c *Config) FeedsFilePath(configPath string) (path string, explicit bool) {
	if c.Ingest.Feeds != "" {
		return c.Ingest.Feeds, true
	}
	return filepath.Join(filepath.Dir(configPath), defaultFeedsFileName), false
}

// ReadFeedsFile parses the feed seed file at path. A missing file is not an
// error — it reports found=false, which is an ordinary state now that feeds
// live in the store and the file only seeds new ones.
func ReadFeedsFile(path string) (doc *FeedsDoc, found bool, err error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read feeds file: %w", err)
	}
	doc = &FeedsDoc{}
	if err := yaml.Unmarshal(raw, doc); err != nil {
		return nil, false, fmt.Errorf("parse feeds file %s: %w", path, err)
	}
	return doc, true, nil
}

// ResolveFeeds applies the defaults cascade to every entry in doc. globalSince
// is the cascade's outermost fallback for `since` — the main config's
// ingest.since — passed in rather than read from a Config so the resolver has
// no opinion about where the feeds came from.
//
// An empty doc resolves to an empty set. That is not an error: the store can
// legitimately hold no feeds, on a fresh install or after the last one is
// removed, and both the ingest cycle and the Sources page handle it.
func ResolveFeeds(doc FeedsDoc, globalSince time.Duration) ([]ResolvedFeed, error) {
	if err := doc.Defaults.Validate(); err != nil {
		return nil, err
	}
	out := make([]ResolvedFeed, 0, len(doc.Feeds))
	for i, f := range doc.Feeds {
		if f.Name == "" {
			return nil, fmt.Errorf("feed %d (%s) has no name", i, f.URL)
		}
		if f.URL == "" {
			return nil, fmt.Errorf("feed %d (%q) has no url", i, f.Name)
		}
		if err := f.Validate(); err != nil {
			return nil, fmt.Errorf("feed %d (%q): %w", i, f.Name, err)
		}
		out = append(out, resolveFeed(f, doc.Defaults, globalSince))
	}
	return out, nil
}

// EnabledFeeds returns only the feeds an ingest run should fetch.
func EnabledFeeds(all []ResolvedFeed) []ResolvedFeed {
	out := make([]ResolvedFeed, 0, len(all))
	for _, f := range all {
		if f.Enabled {
			out = append(out, f)
		}
	}
	return out
}

// resolveFeed walks one feed through the cascade, recording where each value
// came from.
func resolveFeed(f Feed, d FeedDefaults, globalSince time.Duration) ResolvedFeed {
	// A stored feed carries the ID it was minted with; only one that has never
	// been stored (straight from the seed file) needs one derived.
	id := f.ID
	if id == "" {
		id = FeedID(f.URL)
	}
	r := ResolvedFeed{ID: id, Name: f.Name, URL: f.URL, Tags: mergeTags(d.Tags, f.Tags)}

	switch {
	case f.Enabled != nil:
		r.Enabled, r.EnabledFrom = *f.Enabled, OriginFeed
	case d.Enabled != nil:
		r.Enabled, r.EnabledFrom = *d.Enabled, OriginDefaults
	default:
		r.Enabled, r.EnabledFrom = true, OriginBuiltin
	}

	switch {
	case f.Since != nil:
		r.Since, r.SinceFrom = f.Since.Std(), OriginFeed
	case d.Since != nil:
		r.Since, r.SinceFrom = d.Since.Std(), OriginDefaults
	default:
		// The main config's ingest.since is the global lookback; it always has
		// a value by this point (applyDefaults ran first).
		r.Since, r.SinceFrom = globalSince, OriginGlobal
	}

	switch {
	case f.MaxItems != nil:
		r.MaxItems, r.MaxItemsFrom = *f.MaxItems, OriginFeed
	case d.MaxItems != nil:
		r.MaxItems, r.MaxItemsFrom = *d.MaxItems, OriginDefaults
	default:
		r.MaxItems, r.MaxItemsFrom = uncapped, OriginBuiltin
	}
	return r
}

// Validate checks one feed's tuning knobs. A zero since would drop everything
// and a negative cap is meaningless; both are more likely typos than intent.
func (f Feed) Validate() error {
	if f.Since != nil && f.Since.Std() <= 0 {
		return fmt.Errorf("since must be positive, got %s", f.Since)
	}
	if f.MaxItems != nil && *f.MaxItems < 0 {
		return fmt.Errorf("max_items must be zero (uncapped) or positive, got %d", *f.MaxItems)
	}
	return nil
}

// Validate checks the defaults block with the same rules as a feed entry.
func (d FeedDefaults) Validate() error {
	return Feed{Since: d.Since, MaxItems: d.MaxItems}.Validate()
}

// mergeTags unions the defaults' tags with a feed's own, preserving order
// (defaults first) and dropping duplicates.
func mergeTags(defaults, own []string) []string {
	if len(defaults) == 0 && len(own) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(defaults)+len(own))
	out := make([]string, 0, len(defaults)+len(own))
	for _, t := range append(append([]string{}, defaults...), own...) {
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}
