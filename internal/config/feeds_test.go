// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// baseConfig is a valid config with no feeds of its own. ingest.since is the
// outermost fallback in the cascade, so 10d is what "inherited from global"
// resolves to in these tests.
const baseConfig = "profile: ./p.md\nstore:\n  db_path: ./test.db\ningest:\n  since: 10d\n"

// writeConfigWithFeeds writes a config file and, when feedsBody is non-empty, a
// feeds.yaml beside it — the implicit layout Load looks for. Returns the config
// path.
func writeConfigWithFeeds(t *testing.T, configBody, feedsBody string) string {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(configBody), 0o644); err != nil {
		t.Fatal(err)
	}
	if feedsBody != "" {
		if err := os.WriteFile(filepath.Join(dir, "feeds.yaml"), []byte(feedsBody), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return cfgPath
}

// ptrDuration builds an optional window for a defaults or feed override.
func ptrDuration(d time.Duration) *Duration {
	v := Duration(d)
	return &v
}

// resolveFeedsBody parses a feeds document and walks it through the cascade
// against baseConfig's ingest.since, which is what these tests assert against.
func resolveFeedsBody(t *testing.T, feedsBody string) []ResolvedFeed {
	t.Helper()
	cfgPath := writeConfigWithFeeds(t, baseConfig, feedsBody)
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	path, _ := cfg.FeedsFilePath(cfgPath)
	doc, found, err := ReadFeedsFile(path)
	if err != nil || !found {
		t.Fatalf("ReadFeedsFile(%s): found=%v err=%v", path, found, err)
	}
	resolved, err := ResolveFeeds(*doc, cfg.Ingest.Since.Std())
	if err != nil {
		t.Fatalf("ResolveFeeds: %v", err)
	}
	return resolved
}

// feedByName finds a resolved feed for assertions.
func feedByName(t *testing.T, feeds []ResolvedFeed, name string) ResolvedFeed {
	t.Helper()
	for _, f := range feeds {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("no resolved feed named %q in %+v", name, feeds)
	return ResolvedFeed{}
}

// The cascade is the core of the feature: a per-feed value wins, then the
// file's defaults, then the global config, then the built-in.
func TestResolveFeedCascade(t *testing.T) {
	const feedsBody = `
defaults:
  since: 5d
  max_items: 20
  tags: [all]
feeds:
  - name: Overrides
    url: http://a.test/feed
    since: 12h
    max_items: 3
    enabled: false
    tags: [ai]
  - name: Inherits
    url: http://b.test/feed
`
	resolved := resolveFeedsBody(t, feedsBody)

	cases := []struct {
		name         string
		feed         string
		wantSince    time.Duration
		wantSinceOn  Origin
		wantCap      int
		wantCapOn    Origin
		wantEnabled  bool
		wantEnableOn Origin
		wantTags     string
	}{
		{
			name:         "feed sets every knob itself",
			feed:         "Overrides",
			wantSince:    12 * time.Hour,
			wantSinceOn:  OriginFeed,
			wantCap:      3,
			wantCapOn:    OriginFeed,
			wantEnabled:  false,
			wantEnableOn: OriginFeed,
			// Defaults' tags union onto the feed's own rather than being replaced.
			wantTags: "all,ai",
		},
		{
			name:         "feed inherits from the defaults block",
			feed:         "Inherits",
			wantSince:    5 * 24 * time.Hour,
			wantSinceOn:  OriginDefaults,
			wantCap:      20,
			wantCapOn:    OriginDefaults,
			wantEnabled:  true,
			wantEnableOn: OriginBuiltin, // no defaults.enabled set
			wantTags:     "all",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := feedByName(t, resolved, c.feed)
			if f.Since != c.wantSince {
				t.Errorf("Since = %v, want %v", f.Since, c.wantSince)
			}
			if f.SinceFrom != c.wantSinceOn {
				t.Errorf("SinceFrom = %q, want %q", f.SinceFrom, c.wantSinceOn)
			}
			if f.MaxItems != c.wantCap {
				t.Errorf("MaxItems = %d, want %d", f.MaxItems, c.wantCap)
			}
			if f.MaxItemsFrom != c.wantCapOn {
				t.Errorf("MaxItemsFrom = %q, want %q", f.MaxItemsFrom, c.wantCapOn)
			}
			if f.Enabled != c.wantEnabled {
				t.Errorf("Enabled = %v, want %v", f.Enabled, c.wantEnabled)
			}
			if f.EnabledFrom != c.wantEnableOn {
				t.Errorf("EnabledFrom = %q, want %q", f.EnabledFrom, c.wantEnableOn)
			}
			if got := strings.Join(f.Tags, ","); got != c.wantTags {
				t.Errorf("Tags = %q, want %q", got, c.wantTags)
			}
		})
	}
}

// With no defaults block, each knob falls all the way to its outermost source.
func TestResolveFeedOutermostFallbacks(t *testing.T) {
	f := feedByName(t, resolveFeedsBody(t, "feeds:\n  - name: A\n    url: http://a.test/feed\n"), "A")

	cases := []struct {
		field      string
		got, want  any
		gotOn      Origin
		wantOrigin Origin
	}{
		{"Since", f.Since, 10 * 24 * time.Hour, f.SinceFrom, OriginGlobal},
		{"MaxItems", f.MaxItems, 0, f.MaxItemsFrom, OriginBuiltin},
		{"Enabled", f.Enabled, true, f.EnabledFrom, OriginBuiltin},
	}
	for _, c := range cases {
		t.Run(c.field, func(t *testing.T) {
			if c.got != c.want {
				t.Errorf("%s = %v, want %v", c.field, c.got, c.want)
			}
			if c.gotOn != c.wantOrigin {
				t.Errorf("%sFrom = %q, want %q", c.field, c.gotOn, c.wantOrigin)
			}
		})
	}
}

func TestEnabledFeeds(t *testing.T) {
	const feedsBody = `
defaults:
  enabled: false
feeds:
  - name: On
    url: http://a.test/feed
    enabled: true
  - name: OffByDefault
    url: http://b.test/feed
  - name: OffExplicitly
    url: http://c.test/feed
    enabled: false
`
	resolved := resolveFeedsBody(t, feedsBody)

	enabled := EnabledFeeds(resolved)
	if len(enabled) != 1 || enabled[0].Name != "On" {
		t.Fatalf("EnabledFeeds() = %+v, want just On", enabled)
	}
	// All three stay in the set — parked, not removed, so the page shows them.
	if len(resolved) != 3 {
		t.Errorf("resolved = %d feeds, want all 3 retained", len(resolved))
	}
	if f := feedByName(t, resolved, "OffByDefault"); f.EnabledFrom != OriginDefaults {
		t.Errorf("OffByDefault.EnabledFrom = %q, want defaults", f.EnabledFrom)
	}
}

// Identity is derived from the URL, so a rename keeps it and a URL change
// deliberately breaks it (that's a different feed).
func TestFeedID(t *testing.T) {
	const url = "https://example.test/feed.xml"
	cases := []struct {
		name string
		a, b string
		same bool
	}{
		{"same url is the same feed", url, url, true},
		{"surrounding whitespace ignored", url, "  " + url + "\t", true},
		{"different url is a different feed", url, "https://example.test/other.xml", false},
		{"trailing slash is a different url", url, url + "/", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := FeedID(c.a) == FeedID(c.b); got != c.same {
				t.Errorf("FeedID(%q)==FeedID(%q) = %v, want %v", c.a, c.b, got, c.same)
			}
		})
	}

	if n := len(FeedID(url)); n != feedIDLen {
		t.Errorf("FeedID length = %d, want %d", n, feedIDLen)
	}
	// A feed straight from the seed file has its ID minted from the URL alone,
	// so the same URL under a different name is the same feed.
	before := resolveFeedsBody(t, "feeds:\n  - name: Before\n    url: "+url+"\n")
	after := resolveFeedsBody(t, "feeds:\n  - name: After\n    url: "+url+"\n")
	if before[0].ID != after[0].ID {
		t.Error("renaming a feed changed its ID; history would be orphaned")
	}

	// Once stored, the ID is carried rather than re-derived, so changing the
	// URL keeps the feed (and its history) instead of forking a new one.
	stored := Feed{ID: "frozen00id00", Name: "Stored", URL: "https://moved.test/feed"}
	if got := resolveFeed(stored, FeedDefaults{}, time.Hour).ID; got != stored.ID {
		t.Errorf("stored ID = %q, want the frozen %q", got, stored.ID)
	}
}

// An explicit ingest.feeds is honoured; it names the seed file rather than the
// live feed set, which is in the store.
func TestFeedsFilePathExplicit(t *testing.T) {
	dir := t.TempDir()
	feedsPath := filepath.Join(dir, "sources.yaml")
	if err := os.WriteFile(feedsPath,
		[]byte("feeds:\n  - name: A\n    url: http://a.test/feed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(baseConfig+"  feeds: "+feedsPath+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	path, explicit := cfg.FeedsFilePath(cfgPath)
	if path != feedsPath || !explicit {
		t.Fatalf("FeedsFilePath = (%q, %v), want (%q, true)", path, explicit, feedsPath)
	}
	doc, found, err := ReadFeedsFile(path)
	if err != nil || !found || len(doc.Feeds) != 1 {
		t.Fatalf("ReadFeedsFile: found=%v err=%v doc=%+v", found, err, doc)
	}
}

// The implicit seed file is found beside the config, whatever the cwd.
func TestFeedsFilePathIsBesideConfig(t *testing.T) {
	cfgPath := writeConfigWithFeeds(t, baseConfig, "feeds:\n  - name: A\n    url: http://a.test/feed\n")
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	path, explicit := cfg.FeedsFilePath(cfgPath)
	if want := filepath.Join(filepath.Dir(cfgPath), "feeds.yaml"); path != want || explicit {
		t.Errorf("FeedsFilePath = (%q, %v), want (%q, false)", path, explicit, want)
	}
}

// A missing seed file is an ordinary state now that the store owns the feeds —
// it must not stop the config from loading.
func TestReadFeedsFileMissingIsNotAnError(t *testing.T) {
	cfgPath := writeConfigWithFeeds(t, baseConfig, "")
	if _, err := Load(cfgPath); err != nil {
		t.Fatalf("Load with no seed file: %v", err)
	}
	path, _ := (&Config{}).FeedsFilePath(cfgPath)
	doc, found, err := ReadFeedsFile(path)
	if err != nil {
		t.Fatalf("ReadFeedsFile: %v", err)
	}
	if found || doc != nil {
		t.Errorf("found=%v doc=%+v, want a clean miss", found, doc)
	}
}

// A seed file that won't parse says so, naming the file.
func TestReadFeedsFileUnparseable(t *testing.T) {
	cfgPath := writeConfigWithFeeds(t, baseConfig, "feeds:\n  - name: [this is not\n")
	path, _ := (&Config{}).FeedsFilePath(cfgPath)
	_, _, err := ReadFeedsFile(path)
	if err == nil || !strings.Contains(err.Error(), "parse feeds file") {
		t.Fatalf("err = %v, want one naming the parse failure", err)
	}
}

func TestResolveFeedsValidation(t *testing.T) {
	cases := []struct {
		name    string
		feeds   string
		wantErr string
	}{
		{
			name:    "missing url",
			feeds:   "feeds:\n  - name: A\n",
			wantErr: "has no url",
		},
		{
			name:    "missing name",
			feeds:   "feeds:\n  - url: http://a.test/feed\n",
			wantErr: "has no name",
		},
		{
			name:    "zero since",
			feeds:   "feeds:\n  - name: A\n    url: http://a.test/feed\n    since: 0h\n",
			wantErr: "since must be positive",
		},
		{
			name:    "negative since",
			feeds:   "feeds:\n  - name: A\n    url: http://a.test/feed\n    since: -3h\n",
			wantErr: "since must be positive",
		},
		{
			name:    "negative cap",
			feeds:   "feeds:\n  - name: A\n    url: http://a.test/feed\n    max_items: -1\n",
			wantErr: "max_items must be",
		},
		{
			name:    "bad defaults",
			feeds:   "defaults:\n  max_items: -5\nfeeds:\n  - name: A\n    url: http://a.test/feed\n",
			wantErr: "max_items must be",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfgPath := writeConfigWithFeeds(t, baseConfig, c.feeds)
			path, _ := (&Config{}).FeedsFilePath(cfgPath)
			doc, _, err := ReadFeedsFile(path)
			if err != nil {
				t.Fatalf("ReadFeedsFile: %v", err)
			}
			_, err = ResolveFeeds(*doc, 10*24*time.Hour)
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, c.wantErr)
			}
		})
	}
}

// An empty set resolves cleanly. Feeds live in the store, which can be empty on
// a fresh install or after the last one is deleted — the ingest cycle and the
// Sources page both handle it, so the resolver has no business refusing.
func TestResolveFeedsAllowsAnEmptySet(t *testing.T) {
	resolved, err := ResolveFeeds(FeedsDoc{Defaults: FeedDefaults{Since: ptrDuration(72 * time.Hour)}}, time.Hour)
	if err != nil {
		t.Fatalf("ResolveFeeds on an empty set: %v", err)
	}
	if len(resolved) != 0 {
		t.Errorf("resolved = %+v, want none", resolved)
	}
}

// Both functions read the same typed address, so they are checked over one set
// of inputs: what it becomes, and whether it is worth warning about.
func TestNormalizeFeedURL(t *testing.T) {
	cases := []struct {
		name         string
		in           string
		want         string
		wantInsecure bool
	}{
		{name: "bare host", in: "example.test/feed.xml", want: "https://example.test/feed.xml"},
		{name: "host only", in: "example.test", want: "https://example.test"},
		{name: "protocol relative", in: "//example.test/feed", want: "https://example.test/feed"},
		{name: "https untouched", in: "https://example.test/feed", want: "https://example.test/feed"},
		{
			name: "http kept as written", in: "http://example.test/feed",
			want: "http://example.test/feed", wantInsecure: true,
		},
		{
			name: "uppercase scheme", in: "HTTP://example.test/feed",
			want: "HTTP://example.test/feed", wantInsecure: true,
		},
		{name: "surrounding space", in: "  example.test/feed  ", want: "https://example.test/feed"},
		{name: "empty stays empty", in: "   ", want: ""},
		// Left alone so ValidateFeedURL can reject it by name rather than
		// reporting a URL the user never typed.
		{name: "wrong scheme", in: "ftp://example.test/feed", want: "ftp://example.test/feed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := NormalizeFeedURL(c.in); got != c.want {
				t.Errorf("NormalizeFeedURL(%q) = %q, want %q", c.in, got, c.want)
			}
			if got := InsecureFeedURL(c.in); got != c.wantInsecure {
				t.Errorf("InsecureFeedURL(%q) = %v, want %v", c.in, got, c.wantInsecure)
			}
		})
	}
}

func TestValidateFeedURL(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr string
	}{
		{name: "https", in: "https://example.test/feed.xml"},
		{name: "http", in: "http://example.test/rss"},
		{name: "surrounding space is trimmed", in: "  https://example.test/feed  "},
		{name: "empty", in: "", wantErr: "url is required"},
		{name: "no scheme", in: "example.test/feed", wantErr: "must start with http"},
		{name: "wrong scheme", in: "ftp://example.test/feed", wantErr: "must start with http"},
		{name: "no host", in: "https:///feed", wantErr: "has no host"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateFeedURL(c.in)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateFeedURL(%q) = %v, want nil", c.in, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("ValidateFeedURL(%q) = %v, want one containing %q", c.in, err, c.wantErr)
			}
		})
	}
}

// Two feeds may share a URL — they share one fetch-history entry, which is
// warned about rather than rejected.
func TestResolveFeedsAllowsDuplicateURLs(t *testing.T) {
	resolved := resolveFeedsBody(t, "feeds:\n"+
		"  - name: One\n    url: http://a.test/feed\n"+
		"  - name: Two\n    url: http://a.test/feed\n")
	if len(resolved) != 2 {
		t.Fatalf("resolved = %d feeds, want 2", len(resolved))
	}
	if resolved[0].ID != resolved[1].ID {
		t.Error("feeds sharing a URL should share an ID (and thus one history entry)")
	}
}

func TestMergeTags(t *testing.T) {
	cases := []struct {
		name          string
		defaults, own []string
		want          string
		wantNil       bool
	}{
		{name: "union in order", defaults: []string{"a", "b"}, own: []string{"c"}, want: "a,b,c"},
		{name: "duplicates dropped", defaults: []string{"a", "b"}, own: []string{"b", "c"}, want: "a,b,c"},
		{name: "blanks dropped", defaults: []string{"a"}, own: []string{"", "b"}, want: "a,b"},
		{name: "only defaults", defaults: []string{"a"}, want: "a"},
		{name: "only own", own: []string{"z"}, want: "z"},
		{name: "both empty is nil", wantNil: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := mergeTags(c.defaults, c.own)
			if c.wantNil {
				if got != nil {
					t.Errorf("mergeTags = %v, want nil (not an empty slice)", got)
				}
				return
			}
			if joined := strings.Join(got, ","); joined != c.want {
				t.Errorf("mergeTags = %q, want %q", joined, c.want)
			}
		})
	}
}

// The shipped example is what every new user copies, so a typo in it is a
// first-run failure. Load it through the real path rather than trusting review.
func TestFeedsExampleFileLoads(t *testing.T) {
	examplePath, err := filepath.Abs(filepath.Join("..", "..", "configs", "feeds.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	doc, found, err := ReadFeedsFile(examplePath)
	if err != nil || !found {
		t.Fatalf("configs/feeds.example.yaml does not parse: found=%v err=%v", found, err)
	}
	resolved, err := ResolveFeeds(*doc, 10*24*time.Hour)
	if err != nil {
		t.Fatalf("configs/feeds.example.yaml does not resolve: %v", err)
	}
	if len(resolved) == 0 {
		t.Fatal("example resolved to no feeds")
	}

	// The example is also documentation: it should demonstrate the knobs, so
	// assert the shapes it's meant to teach are actually present.
	var (
		parked      int
		withCap     int
		withSince   int
		withTags    int
		defaultsSet = doc.Defaults.Since != nil && doc.Defaults.MaxItems != nil
	)
	for _, f := range resolved {
		if !f.Enabled {
			parked++
		}
		if f.MaxItemsFrom == OriginFeed {
			withCap++
		}
		if f.SinceFrom == OriginFeed {
			withSince++
		}
		if len(f.Tags) > 0 {
			withTags++
		}
	}
	if !defaultsSet {
		t.Error("example should demonstrate a defaults block with since and max_items")
	}
	if parked == 0 {
		t.Error("example should demonstrate a parked feed (enabled: false)")
	}
	if withCap == 0 || withSince == 0 {
		t.Error("example should demonstrate per-feed since and max_items overrides")
	}
	if withTags != len(resolved) {
		t.Error("example should tag every feed")
	}
}
