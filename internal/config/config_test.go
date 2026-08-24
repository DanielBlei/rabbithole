// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseDuration(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"14d", 14 * 24 * time.Hour, false},
		{"7d", 7 * 24 * time.Hour, false},
		{"168h", 168 * time.Hour, false},
		{"1h30m", 90 * time.Minute, false},
		{" 7d ", 7 * 24 * time.Hour, false},
		{"", 0, true},
		{"abc", 0, true},
		{"d", 0, true},   // missing number before 'd'
		{"14x", 0, true}, // unknown unit
	}
	for _, c := range cases {
		got, err := ParseDuration(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseDuration(%q): expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseDuration(%q): unexpected error %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseDuration(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// writeConfig writes a config file plus the minimal feeds.yaml beside it that
// Load now requires, and returns the config path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	return writeConfigWithFeeds(t, body, minimalFeeds)
}

// baseFeeds is the required-fields-only config the duration/think tests build on.
const baseFeeds = "profile: ./p.md\nstore:\n  db_path: ./test.db\n"

// minimalFeeds is the smallest valid feeds file.
const minimalFeeds = "feeds:\n  - name: x\n    url: http://x\n"

func TestLoadSinceDaysAndHours(t *testing.T) {
	for _, tc := range []struct {
		since string
		want  time.Duration
	}{
		{"ingest:\n  since: 14d\n", 14 * 24 * time.Hour},
		{"ingest:\n  since: 168h\n", 168 * time.Hour},
		{"", 7 * 24 * time.Hour}, // omitted -> default 7d
	} {
		cfg, err := Load(writeConfig(t, baseFeeds+tc.since))
		if err != nil {
			t.Fatalf("Load(since=%q): %v", tc.since, err)
		}
		if cfg.Ingest.Since.Std() != tc.want {
			t.Errorf("since=%q -> %s, want %s", tc.since, cfg.Ingest.Since, tc.want)
		}
	}
}

func TestLoadSinceInvalid(t *testing.T) {
	if _, err := Load(writeConfig(t, baseFeeds+"ingest:\n  since: 14x\n")); err == nil {
		t.Error("expected error for invalid since value")
	}
}

func TestLoadThinkDefaultsAndOverride(t *testing.T) {
	for _, tc := range []struct {
		think string
		want  bool
	}{
		{"", true},                              // omitted -> default true
		{"inference:\n  think: true\n", true},   // explicit on
		{"inference:\n  think: false\n", false}, // explicit off must not read as the default
	} {
		cfg, err := Load(writeConfig(t, baseFeeds+tc.think))
		if err != nil {
			t.Fatalf("Load(think=%q): %v", tc.think, err)
		}
		if cfg.Inference.Think == nil {
			t.Fatalf("think=%q -> nil, want non-nil after defaults", tc.think)
		}
		if *cfg.Inference.Think != tc.want {
			t.Errorf("think=%q -> %v, want %v", tc.think, *cfg.Inference.Think, tc.want)
		}
	}
}

func TestStripHTMLComments(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"no comment", "# Profile\n- AI", "# Profile\n- AI"},
		{"leading block", "<!--\nnotes\n-->\n\n# Profile\n- AI", "# Profile\n- AI"},
		{"inline", "- AI <!-- keep an eye on this --> and infra", "- AI  and infra"},
		{"multiple", "<!-- a -->x<!-- b -->", "x"},
		{"unterminated is left alone", "# Profile\n<!-- oops", "# Profile\n<!-- oops"},
	} {
		if got := stripHTMLComments(tc.in); got != tc.want {
			t.Errorf("%s: stripHTMLComments(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// The shipped template is mostly an HTML comment, so a profile that looks filled
// in can strip to nothing. Scoring every article against nothing is meaningless
// rather than degraded, and it used to happen without a word.
func TestLoadProfileRejectsAnEmptyProfile(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		wantErr bool
	}{
		{"real profile", "# Profile\n- Local LLMs", false},
		{"empty file", "", true},
		{"whitespace only", "\n\n   \n", true},
		{"comment only", "<!--\nDescribe what you want to read.\n-->\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "profile.md")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := (&Config{Profile: path}).LoadProfile()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("LoadProfile() = %q, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadProfile() error = %v", err)
			}
		})
	}
}
