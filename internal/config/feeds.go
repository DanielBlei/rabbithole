package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"gopkg.in/yaml.v3"
)

// defaultFeedsFileName is the feeds file Load looks for next to the config
// file when `ingest.feeds` isn't set.
const defaultFeedsFileName = "feeds.yaml"

// feedIDLen is how many hex characters of the URL digest make up a feed ID.
// 12 hex chars (48 bits) makes an accidental collision across a personal feed
// list statistically impossible while staying readable in a log line.
const feedIDLen = 12

// Feed is one RSS/Atom source exactly as written in the feeds file. Every knob
// past name/url is a pointer so an omitted field is distinguishable from a
// zero value and can fall through to the file's defaults — see resolveFeed for
// the cascade.
type Feed struct {
	Name     string    `yaml:"name"`
	URL      string    `yaml:"url"`
	Enabled  *bool     `yaml:"enabled"`   // park a feed without deleting it
	Since    *Duration `yaml:"since"`     // per-feed lookback window
	MaxItems *int      `yaml:"max_items"` // cap on newest items contributed per run
	Tags     []string  `yaml:"tags"`      // free-form labels, unioned with the defaults'
}

// FeedDefaults are the file-wide fallbacks applied to any feed that doesn't
// set a knob itself. Tags are the exception: they union onto every feed's own
// tags rather than being overridden by them.
type FeedDefaults struct {
	Enabled  *bool     `yaml:"enabled"`
	Since    *Duration `yaml:"since"`
	MaxItems *int      `yaml:"max_items"`
	Tags     []string  `yaml:"tags"`
}

// FeedsDoc is a parsed feeds file: file-wide defaults plus the feed list.
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
// (feed → feeds-file defaults → global config → built-in), carrying the
// provenance of each resolved value alongside it. This is what the ingest
// cycle and the feeds viewer both consume; nothing downstream re-implements
// the fallback rules.
type ResolvedFeed struct {
	// ID is the feed's stable identity, derived from its URL — see FeedID.
	// Persisted state (fetch history) is keyed on this rather than on Name, so
	// renaming a feed keeps its history. Renaming is a label change; changing
	// the URL makes it a different feed.
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

// FeedID derives a feed's stable identity from its URL. The URL is what makes
// a feed that feed — the name is a label the user is free to change — so it is
// the identity persisted state hangs off.
//
// Two entries pointing at the same URL therefore share an ID and, with it, one
// row of fetch history. That's accepted rather than forbidden: it is a
// harmless, self-inflicted duplicate, and rejecting it would be a validation
// error for something that still works fine. resolveFeeds logs a warning so it
// isn't silent.
func FeedID(url string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(url)))
	return hex.EncodeToString(sum[:])[:feedIDLen]
}

// FeedSet is the resolved feed configuration: the feeds themselves, the
// defaults they were resolved against, and where they were read from. It is
// derived state — the product of loading, not something written in a file —
// so it lives behind one field on Config rather than being scattered across
// several alongside the declared YAML fields.
//
// A FeedSet is immutable once Load returns. Nothing takes a lock around it
// because nothing mutates it after startup; a future feature that edits feeds
// at runtime (see the feed-management backlog item) must not simply reassign
// it in place while an ingest run holds a reference.
type FeedSet struct {
	All      []ResolvedFeed
	Defaults FeedDefaults
	Path     string // the file the feeds were read from
}

// Len reports how many feeds are configured, enabled or not.
func (s FeedSet) Len() int { return len(s.All) }

// Enabled returns only the feeds an ingest run should fetch.
func (s FeedSet) Enabled() []ResolvedFeed {
	out := make([]ResolvedFeed, 0, len(s.All))
	for _, f := range s.All {
		if f.Enabled {
			out = append(out, f)
		}
	}
	return out
}

// loadFeeds resolves the config's feed set from the feeds file. configPath is
// the config file's own path — the implicit feeds file is looked for beside it.
func (c *Config) loadFeeds(configPath string) error {
	path, explicit := c.feedsFilePath(configPath)
	doc, found, err := readFeedsFile(path)
	if err != nil {
		return err
	}
	if !found {
		if explicit {
			return fmt.Errorf("ingest.feeds %q does not exist", path)
		}
		return fmt.Errorf("no feeds configured: create %s (copy configs/feeds.example.yaml)", path)
	}

	if err := c.SetFeeds(*doc); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	c.Feeds.Path = path
	return nil
}

// SetFeeds resolves doc against this config and installs the result as the
// config's feed set — the same path Load takes, exposed for callers that build
// a Config programmatically rather than from a file. It is the only way to
// populate Feeds, so the defaults cascade can't be sidestepped.
//
// Load-time only: see FeedSet on why this must not be used to swap feeds under
// a running ingest.
func (c *Config) SetFeeds(doc FeedsDoc) error {
	resolved, err := c.resolveFeeds(doc)
	if err != nil {
		return err
	}
	c.Feeds = FeedSet{All: resolved, Defaults: doc.Defaults}
	return nil
}

// feedsFilePath returns the feeds file to read and whether the user named it
// explicitly. An explicit ingest.feeds is taken as written (resolved against the
// process working directory, like `profile`); when unset, the default sits
// beside the config file so moving the config directory keeps them together.
func (c *Config) feedsFilePath(configPath string) (path string, explicit bool) {
	if c.Ingest.Feeds != "" {
		return c.Ingest.Feeds, true
	}
	return filepath.Join(filepath.Dir(configPath), defaultFeedsFileName), false
}

// readFeedsFile parses the feeds file at path. A missing file is not an error —
// it reports found=false so the caller can produce a better message than a
// bare ENOENT.
func readFeedsFile(path string) (doc *FeedsDoc, found bool, err error) {
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

// resolveFeeds applies the defaults cascade to every entry and validates the
// result. Names must be present and unique: the ingest cycle groups items by
// feed name and items are stored under it, so two feeds sharing a name would
// silently merge into one source.
func (c *Config) resolveFeeds(doc FeedsDoc) ([]ResolvedFeed, error) {
	if len(doc.Feeds) == 0 {
		return nil, fmt.Errorf("at least one feed is required")
	}
	if err := doc.Defaults.validate(); err != nil {
		return nil, err
	}

	out := make([]ResolvedFeed, 0, len(doc.Feeds))
	byName := make(map[string]int, len(doc.Feeds))
	byURL := make(map[string]string, len(doc.Feeds))
	for i, f := range doc.Feeds {
		if f.Name == "" {
			return nil, fmt.Errorf("feed %d (%s) has no name", i, f.URL)
		}
		if f.URL == "" {
			return nil, fmt.Errorf("feed %d (%q) has no url", i, f.Name)
		}
		if prev, dup := byName[f.Name]; dup {
			return nil, fmt.Errorf(
				"feed %d (%q) reuses the name of feed %d; feed names must be unique", i, f.Name, prev)
		}
		byName[f.Name] = i
		if err := f.validate(); err != nil {
			return nil, fmt.Errorf("feed %d (%q): %w", i, f.Name, err)
		}
		// Not fatal (see FeedID) — but the two entries will share one row of
		// fetch history, which is worth knowing about.
		if prev, dup := byURL[f.URL]; dup {
			log.Warn().Str("feed", f.Name).Str("duplicate_of", prev).Str("url", f.URL).
				Msg("two feeds share a url; they will share one fetch-history entry")
		}
		byURL[f.URL] = f.Name
		out = append(out, c.resolveFeed(f, doc.Defaults))
	}
	return out, nil
}

// resolveFeed walks one feed through the cascade, recording where each value
// came from.
func (c *Config) resolveFeed(f Feed, d FeedDefaults) ResolvedFeed {
	r := ResolvedFeed{ID: FeedID(f.URL), Name: f.Name, URL: f.URL, Tags: mergeTags(d.Tags, f.Tags)}

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
		r.Since, r.SinceFrom = c.Ingest.Since.Std(), OriginGlobal
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

// validate checks one feed's tuning knobs. A zero since would drop everything
// and a negative cap is meaningless; both are more likely typos than intent.
func (f Feed) validate() error {
	if f.Since != nil && f.Since.Std() <= 0 {
		return fmt.Errorf("since must be positive, got %s", f.Since)
	}
	if f.MaxItems != nil && *f.MaxItems < 0 {
		return fmt.Errorf("max_items must be zero (uncapped) or positive, got %d", *f.MaxItems)
	}
	return nil
}

// validate checks the defaults block with the same rules as a feed entry.
func (d FeedDefaults) validate() error {
	return Feed{Since: d.Since, MaxItems: d.MaxItems}.validate()
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
