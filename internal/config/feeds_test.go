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

// loadFeeds loads a config over the given feeds file and fails on error.
func loadWithFeeds(t *testing.T, feedsBody string) *Config {
	t.Helper()
	cfg, err := Load(writeConfigWithFeeds(t, baseConfig, feedsBody))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

// feedByName finds a resolved feed for assertions.
func feedByName(t *testing.T, cfg *Config, name string) ResolvedFeed {
	t.Helper()
	for _, f := range cfg.Feeds.All {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("no resolved feed named %q in %+v", name, cfg.Feeds.All)
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
	cfg := loadWithFeeds(t, feedsBody)

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
			f := feedByName(t, cfg, c.feed)
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
	cfg := loadWithFeeds(t, "feeds:\n  - name: A\n    url: http://a.test/feed\n")
	f := feedByName(t, cfg, "A")

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

func TestFeedSetEnabled(t *testing.T) {
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
	cfg := loadWithFeeds(t, feedsBody)

	enabled := cfg.Feeds.Enabled()
	if len(enabled) != 1 || enabled[0].Name != "On" {
		t.Fatalf("Enabled() = %+v, want just On", enabled)
	}
	// All three stay in the set — parked, not removed, so the viewer shows them.
	if cfg.Feeds.Len() != 3 {
		t.Errorf("Len() = %d, want all 3 retained", cfg.Feeds.Len())
	}
	if f := feedByName(t, cfg, "OffByDefault"); f.EnabledFrom != OriginDefaults {
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
	// The ID must survive a rename: it's a function of the URL alone.
	cfg := loadWithFeeds(t, "feeds:\n  - name: Before\n    url: "+url+"\n")
	renamed := loadWithFeeds(t, "feeds:\n  - name: After\n    url: "+url+"\n")
	if cfg.Feeds.All[0].ID != renamed.Feeds.All[0].ID {
		t.Error("renaming a feed changed its ID; history would be orphaned")
	}
}

// An explicit feeds_file is honoured and its path is reported for the viewer.
func TestLoadFeedsExplicitPath(t *testing.T) {
	dir := t.TempDir()
	feedsPath := filepath.Join(dir, "sources.yaml")
	if err := os.WriteFile(feedsPath,
		[]byte("feeds:\n  - name: A\n    url: http://a.test/feed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(baseConfig+"feeds_file: "+feedsPath+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Feeds.Path != feedsPath {
		t.Errorf("Feeds.Path = %q, want %q", cfg.Feeds.Path, feedsPath)
	}
	if cfg.Feeds.Len() != 1 {
		t.Errorf("Len() = %d, want 1", cfg.Feeds.Len())
	}
}

// The implicit feeds file is found beside the config, whatever the cwd.
func TestLoadFeedsImplicitPathIsBesideConfig(t *testing.T) {
	cfgPath := writeConfigWithFeeds(t, baseConfig, "feeds:\n  - name: A\n    url: http://a.test/feed\n")
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := filepath.Join(filepath.Dir(cfgPath), "feeds.yaml"); cfg.Feeds.Path != want {
		t.Errorf("Feeds.Path = %q, want %q", cfg.Feeds.Path, want)
	}
}

// Load-time failures, each naming the fix rather than surfacing a raw error.
func TestLoadFeedsErrors(t *testing.T) {
	cases := []struct {
		name    string
		config  string
		feeds   string // empty means "no feeds.yaml on disk"
		wantErr string
	}{
		{
			name:    "no feeds file at all",
			config:  baseConfig,
			wantErr: "no feeds configured",
		},
		{
			name:    "explicit feeds_file missing",
			config:  baseConfig + "feeds_file: /nope/feeds.yaml\n",
			wantErr: "does not exist",
		},
		{
			name:    "unparseable feeds file",
			config:  baseConfig,
			feeds:   "feeds:\n  - name: [this is not\n",
			wantErr: "parse feeds file",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Load(writeConfigWithFeeds(t, c.config, c.feeds))
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, c.wantErr)
			}
		})
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
			// Items are stored under the feed name, so duplicates would merge.
			name:    "duplicate name",
			feeds:   "feeds:\n  - name: A\n    url: http://a.test/1\n  - name: A\n    url: http://a.test/2\n",
			wantErr: "must be unique",
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
		{
			name:    "empty list",
			feeds:   "defaults:\n  since: 3d\nfeeds: []\n",
			wantErr: "at least one feed",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Load(writeConfigWithFeeds(t, baseConfig, c.feeds))
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, c.wantErr)
			}
		})
	}
}

// Two feeds may share a URL — they share one fetch-history entry, which is
// warned about rather than rejected.
func TestResolveFeedsAllowsDuplicateURLs(t *testing.T) {
	cfg := loadWithFeeds(t, "feeds:\n"+
		"  - name: One\n    url: http://a.test/feed\n"+
		"  - name: Two\n    url: http://a.test/feed\n")
	if cfg.Feeds.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", cfg.Feeds.Len())
	}
	if cfg.Feeds.All[0].ID != cfg.Feeds.All[1].ID {
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
	cfgPath := writeConfigWithFeeds(t, baseConfig+"feeds_file: "+examplePath+"\n", "")

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("configs/feeds.example.yaml does not load: %v", err)
	}
	if cfg.Feeds.Len() == 0 {
		t.Fatal("example resolved to no feeds")
	}

	// The example is also documentation: it should demonstrate the knobs, so
	// assert the shapes it's meant to teach are actually present.
	var (
		parked      int
		withCap     int
		withSince   int
		withTags    int
		defaultsSet = cfg.Feeds.Defaults.Since != nil && cfg.Feeds.Defaults.MaxItems != nil
	)
	for _, f := range cfg.Feeds.All {
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
	if withTags != cfg.Feeds.Len() {
		t.Error("example should tag every feed")
	}
}
