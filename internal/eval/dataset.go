// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/DanielBlei/rabbithole/internal/feeds"
)

// supportedVersion is the metadata.version this loader understands. A bump
// means the shape changed in a way an older reader would misread.
const supportedVersion = 1

// Score bounds, matching what the scoring prompt asks for and what
// rank.ParseScores clamps to.
const (
	MinScore = 0
	MaxScore = 10
)

// Tag is an evaluation parameter: it groups samples so the report can be sliced
// by whatever distinction matters to you. The vocabulary belongs to the
// benchmark file rather than to this package, since which distinctions are worth
// measuring depends on the profile being tested. A fixture declares its own
// tags and this loader only checks that samples use ones that were declared.
//
// Not to be confused with items.tags in the store, which is feed-owned metadata
// on real rows. These exist only for the benchmark and never reach the model.
type Tag string

// Sample is one labelled article. Field names track the items table, since a
// sample stands in for a real row.
type Sample struct {
	ID      string `yaml:"id"`
	Source  string `yaml:"source"`
	Title   string `yaml:"title"`
	Summary string `yaml:"summary"`
	// Expected is the 0-10 the model should land on. A pointer so that an
	// omitted key is distinguishable from a deliberate 0, which is a real
	// score and the one a missing field would otherwise silently impersonate.
	// Load guarantees it is non-nil; use ExpectedScore.
	Expected *int `yaml:"expected_llm_score"`
	// Tags are the groups this sample belongs to in the report.
	Tags []Tag `yaml:"tags"`
	// Note is the author's reasoning, printed beside the sample when it fails.
	Note string `yaml:"note"`
}

// ExpectedScore is the score the sample should receive. Valid on any sample
// that came from Load.
func (s Sample) ExpectedScore() int { return *s.Expected }

// Metadata identifies the benchmark.
type Metadata struct {
	Name    string `yaml:"name"`
	Version int    `yaml:"version"`
}

// Dataset is a parsed and validated benchmark file.
type Dataset struct {
	Metadata Metadata `yaml:"metadata"`
	// Tags maps each declared tag to its one-line description, which the
	// report prints beside the group.
	Tags    map[Tag]string `yaml:"tags"`
	Samples []Sample       `yaml:"samples"`

	// Hash fingerprints the file this was read from, so a report can prove
	// which fixture produced it. Set by Load, never read from the YAML.
	Hash string `yaml:"-"`
}

// Load reads and validates the benchmark at path.
//
// Decoding is strict: an unknown key is an error rather than a silently
// ignored one. This is test data, and a misspelled key that fell back to a zero
// value would leave the benchmark quietly measuring something else.
func Load(path string) (*Dataset, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read benchmark: %w", err)
	}
	// Some editors prepend a BOM, which the YAML scanner will not accept.
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})

	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)

	var ds Dataset
	if err := dec.Decode(&ds); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("benchmark %s is empty", path)
		}
		return nil, fmt.Errorf("parse benchmark %s: %w", path, err)
	}
	if err := ds.validate(); err != nil {
		return nil, fmt.Errorf("benchmark %s: %w", path, err)
	}
	sum := sha256.Sum256(raw)
	ds.Hash = "sha256:" + hex.EncodeToString(sum[:])
	return &ds, nil
}

// validate rejects a dataset the benchmark could not report on honestly.
func (d *Dataset) validate() error {
	if d.Metadata.Version != supportedVersion {
		return fmt.Errorf("metadata.version is %d, want %d", d.Metadata.Version, supportedVersion)
	}
	if len(d.Samples) == 0 {
		return errors.New("no samples")
	}

	seen := make(map[string]int, len(d.Samples))
	for i, s := range d.Samples {
		where := fmt.Sprintf("sample %d", i)
		if s.ID != "" {
			where = fmt.Sprintf("sample %d (%s)", i, s.ID)
		}

		if s.ID == "" {
			return fmt.Errorf("%s: id is required", where)
		}
		if first, dup := seen[s.ID]; dup {
			return fmt.Errorf("%s: duplicate id, first seen at sample %d", where, first)
		}
		seen[s.ID] = i

		if s.Title == "" {
			return fmt.Errorf("%s: title is required", where)
		}
		if s.Expected == nil {
			return fmt.Errorf("%s: expected_llm_score is required", where)
		}
		if v := *s.Expected; v < MinScore || v > MaxScore {
			return fmt.Errorf("%s: expected_llm_score is %d, want %d-%d", where, v, MinScore, MaxScore)
		}

		tagSeen := make(map[Tag]bool, len(s.Tags))
		for _, t := range s.Tags {
			if _, declared := d.Tags[t]; !declared {
				return fmt.Errorf("%s: tag %q is not declared in the tags block", where, t)
			}
			if tagSeen[t] {
				return fmt.Errorf("%s: duplicate tag %q", where, t)
			}
			tagSeen[t] = true
		}
	}
	return nil
}

// Limit returns a copy holding only the first n samples, in file order, or the
// dataset itself when n is not a narrowing.
//
// File order rather than a random draw, so two runs either side of a profile
// edit score the same samples and the difference between them means something.
// Hash is carried over unchanged: it fingerprints the file, not the slice, and
// RunInfo records the narrowing separately.
func (d *Dataset) Limit(n int) *Dataset {
	if n < 1 || n >= len(d.Samples) {
		return d
	}
	limited := *d
	limited.Samples = d.Samples[:n:n]
	return &limited
}

// Items converts the samples into the type the scorer takes.
//
// Only Source, Title and Summary are carried, because those are the only
// fields rank.BuildUserPrompt puts in front of the model. Everything else on a
// Sample is for the report, and passing it here would make a benchmark sample
// behave unlike the live item it stands in for.
func (d *Dataset) Items() []feeds.Item {
	items := make([]feeds.Item, 0, len(d.Samples))
	for _, s := range d.Samples {
		items = append(items, feeds.Item{
			ID:      s.ID,
			Source:  s.Source,
			Title:   s.Title,
			Summary: s.Summary,
		})
	}
	return items
}
