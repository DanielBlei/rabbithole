// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package library

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"maps"
	"slices"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/feeds"
)

const (
	// IconsFile is the file of baked site icons, next to the YAML files.
	IconsFile = "icons.json"
	// IconsVersion is the newest icons file format this package reads.
	IconsVersion = 1
	// MaxIconsBytes is the size budget of the checked-in icons file.
	MaxIconsBytes = 768 << 10

	iconETagChars = 16
)

var pngSignature = []byte("\x89PNG\r\n\x1a\n")

// Icon is a baked site icon: a PNG re-encoded by the app's icon fetcher.
type Icon struct {
	PNG  []byte
	Dark bool   // a dark glyph on a transparent background, drawn on a light plate
	ETag string // content tag, hex and without quotes
}

// IconSet is a parsed icons file.
type IconSet struct {
	// Icons holds the icons that passed validation, by source id (config.FeedID of the source URL).
	Icons map[string]Icon
	// None lists the source ids known to have no usable icon, sorted.
	None []string
	// Dropped lists the ids whose icon failed validation, sorted.
	Dropped []string
}

// Known reports whether id has an icon or is known to have none.
func (s IconSet) Known(id string) bool {
	_, ok := s.Icons[id]
	return ok || slices.Contains(s.None, id)
}

// errNewerIcons means the file's version is newer than this package reads.
var errNewerIcons = errors.New("icons file is newer than supported")

type iconsDoc struct {
	Version int                  `json:"version"`
	Icons   map[string]iconEntry `json:"icons"`
	None    []string             `json:"none"`
}

type iconEntry struct {
	PNG  string `json:"png"`
	Dark bool   `json:"dark"`
}

// ParseIcons reads an icons file. An entry whose id is not a feed id, or that is
// not a small valid PNG, is dropped and listed in Dropped; the rest are kept.
// Ids in the none list that are not feed ids are ignored. A file that is not the
// format, or is newer than this package reads, is an error.
func ParseIcons(data []byte) (IconSet, error) {
	var doc iconsDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return IconSet{}, fmt.Errorf("icons file: %w", err)
	}
	switch {
	case doc.Version > IconsVersion:
		return IconSet{}, errNewerIcons
	case doc.Version != IconsVersion:
		return IconSet{}, fmt.Errorf("icons file: version %d is not %d", doc.Version, IconsVersion)
	}
	set := IconSet{Icons: make(map[string]Icon, len(doc.Icons))}
	for _, id := range slices.Sorted(slices.Values(doc.None)) {
		if validIconID(id) {
			set.None = append(set.None, id)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(doc.Icons)) {
		if !validIconID(id) {
			set.Dropped = append(set.Dropped, id)
			continue
		}
		e := doc.Icons[id]
		raw, err := base64.StdEncoding.DecodeString(e.PNG)
		if err == nil {
			err = checkIconPNG(raw)
		}
		if err != nil {
			set.Dropped = append(set.Dropped, id)
			continue
		}
		set.Icons[id] = Icon{PNG: raw, Dark: e.Dark, ETag: IconETag(raw)}
	}
	return set, nil
}

// validIconID reports whether id has the form of config.FeedID: lowercase hex of its length.
func validIconID(id string) bool {
	if len(id) != len(config.FeedID("")) {
		return false
	}
	for _, r := range id {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// checkIconPNG applies the fetcher's limits to baked bytes: a PNG within the
// stored size and side caps.
func checkIconPNG(raw []byte) error {
	if len(raw) == 0 || len(raw) > feeds.IconStoredBytes {
		return fmt.Errorf("icon is %d bytes", len(raw))
	}
	if !bytes.HasPrefix(raw, pngSignature) {
		return errors.New("icon is not a PNG")
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	if cfg.Width < 1 || cfg.Height < 1 || cfg.Width > feeds.IconMaxSide || cfg.Height > feeds.IconMaxSide {
		return fmt.Errorf("icon is %dx%d", cfg.Width, cfg.Height)
	}
	return nil
}

// IconETag is the content tag of a baked icon: a hex prefix of its sha256.
func IconETag(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:iconETagChars]
}

// MarshalIcons writes the icons file: sorted, one entry per line, so the diff of
// a regenerated file shows only the icons that changed.
func MarshalIcons(icons map[string]Icon, none []string) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "{\"version\": %d,\n \"icons\": {", IconsVersion)
	for i, id := range slices.Sorted(maps.Keys(icons)) {
		png, _ := json.Marshal(base64.StdEncoding.EncodeToString(icons[id].PNG))
		sep := "\n  "
		if i > 0 {
			sep = ",\n  "
		}
		key, _ := json.Marshal(id)
		fmt.Fprintf(&b, "%s%s: {\"png\": %s, \"dark\": %t}", sep, key, png, icons[id].Dark)
	}
	b.WriteString("\n },\n \"none\": [")
	sorted := slices.Sorted(slices.Values(none))
	for i, id := range sorted {
		sep := "\n  "
		if i > 0 {
			sep = ",\n  "
		}
		key, _ := json.Marshal(id)
		b.WriteString(sep + string(key))
	}
	b.WriteString("\n ]}\n")
	return b.Bytes()
}

// MissingIcons lists a finding for each source of lib whose id is in neither
// the icons nor the none list of set.
func MissingIcons(lib *Library, set IconSet) []string {
	var out []string
	for _, f := range lib.Files {
		for _, g := range f.Groups {
			for _, s := range g.Sources {
				if !set.Known(config.FeedID(s.URL)) {
					out = append(out, fmt.Sprintf(
						"library/%s has no entry for %s: run make library-icons", IconsFile, s.Name))
				}
			}
		}
	}
	return out
}
