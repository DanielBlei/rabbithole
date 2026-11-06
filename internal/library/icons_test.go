// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package library

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"maps"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/DanielBlei/rabbithole/internal/config"
)

// testPNG encodes a w by h PNG: a flat colour, or noise when noisy.
func testPNG(t *testing.T, w, h int, noisy bool) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	rng := rand.New(rand.NewPCG(1, 2))
	for y := range h {
		for x := range w {
			c := color.NRGBA{R: 10, G: 120, B: 200, A: 255}
			if noisy {
				c = color.NRGBA{R: uint8(rng.UintN(256)), G: uint8(rng.UintN(256)), B: uint8(rng.UintN(256)), A: 255}
			}
			img.SetNRGBA(x, y, c)
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// iconsJSON is an icons file with the given raw base64 entries, in id order of the map.
func iconsJSON(entries map[string]string, none ...string) []byte {
	var parts []string
	for _, id := range slices.Sorted(maps.Keys(entries)) {
		parts = append(parts, fmt.Sprintf("%q: {\"png\": %q, \"dark\": false}", id, entries[id]))
	}
	noneJSON := ""
	for i, id := range none {
		if i > 0 {
			noneJSON += ", "
		}
		noneJSON += fmt.Sprintf("%q", id)
	}
	return fmt.Appendf(nil, `{"version": 1, "icons": {%s}, "none": [%s]}`, strings.Join(parts, ", "), noneJSON)
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// Valid icon ids: 12 lowercase hex characters, like config.FeedID.
const (
	idA    = "aaaaaaaaaaaa"
	idB    = "bbbbbbbbbbbb"
	idC    = "cccccccccccc"
	idZ    = "fffffffffffe"
	idGood = "0000000000a1"
)

func TestIconsFormatRoundTrips(t *testing.T) {
	small := testPNG(t, 16, 16, false)
	other := testPNG(t, 32, 32, false)
	in := map[string]Icon{idB: {PNG: other, Dark: true}, idA: {PNG: small}}
	data := MarshalIcons(in, []string{idZ, idC})
	if !bytes.Equal(data, MarshalIcons(in, []string{idC, idZ})) {
		t.Fatal("output is not deterministic")
	}
	if strings.Index(string(data), `"`+idA+`"`) > strings.Index(string(data), `"`+idB+`"`) {
		t.Error("icons are not sorted by id")
	}

	set, err := ParseIcons(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Dropped) != 0 || len(set.Icons) != 2 {
		t.Fatalf("set = %+v, want two icons and none dropped", set)
	}
	if got := set.Icons[idB]; !got.Dark || !bytes.Equal(got.PNG, other) || got.ETag != IconETag(other) {
		t.Errorf("%s = %+v, want dark, the same bytes and their tag", idB, got)
	}
	if !slices.Equal(set.None, []string{idC, idZ}) {
		t.Errorf("None = %v, want sorted ids", set.None)
	}
	for _, id := range []string{idA, idC} {
		if !set.Known(id) {
			t.Errorf("Known(%q) = false", id)
		}
	}
	if set.Known("nope") {
		t.Error("Known(nope) = true")
	}
	if len(IconETag(small)) != iconETagChars || IconETag(small) == IconETag(other) {
		t.Error("ETag is not a distinct short hex tag")
	}
}

func TestParseIconsRejectsBadFiles(t *testing.T) {
	for name, in := range map[string]string{
		"not json":     "icons",
		"no version":   `{"icons": {}}`,
		"zero version": `{"version": 0}`,
		"newer":        `{"version": 2, "icons": {}}`,
	} {
		if _, err := ParseIcons([]byte(in)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if _, err := ParseIcons([]byte(`{"version": 2}`)); err != errNewerIcons {
		t.Errorf("newer version error = %v, want errNewerIcons", err)
	}
}

func TestParseIconsDropsBadEntries(t *testing.T) {
	good := testPNG(t, 16, 16, false)
	const (
		idBase64   = "000000000001"
		idEmpty    = "000000000002"
		idHuge     = "000000000003"
		idNotPNG   = "000000000004"
		idOversize = "000000000005"
		idTruncat  = "000000000006"
	)
	entries := map[string]string{
		idGood:    b64(good),
		idBase64:  "!!! not base64",
		idNotPNG:  b64([]byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;")),
		idHuge:    b64(testPNG(t, 1000, 1000, false)),
		idEmpty:   "",
		idTruncat: b64(good[:20]),
	}
	noisy := testPNG(t, 200, 200, true)
	if len(noisy) <= 64<<10 {
		t.Fatalf("noise PNG is %d bytes, want over the stored cap", len(noisy))
	}
	entries[idOversize] = b64(noisy)

	set, err := ParseIcons(iconsJSON(entries, idC))
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Icons) != 1 || set.Icons[idGood].PNG == nil {
		t.Fatalf("kept %v, want only %s", slices.Sorted(maps.Keys(set.Icons)), idGood)
	}
	want := []string{idBase64, idEmpty, idHuge, idNotPNG, idOversize, idTruncat}
	if !slices.Equal(set.Dropped, want) {
		t.Errorf("Dropped = %v, want %v", set.Dropped, want)
	}
	if set.Known(idHuge) {
		t.Error("a dropped icon must not count as known")
	}
}

func TestParseIconsDropsIdsThatAreNotFeedIDs(t *testing.T) {
	good := b64(testPNG(t, 16, 16, false))
	bad := []string{"good", "AAAAAAAAAAAA", "aaaaaaaaaaa", "aaaaaaaaaaaaa", "gggggggggggg", "../etc/pass"}
	entries := map[string]string{idA: good}
	for _, id := range bad {
		entries[id] = good
	}
	set, err := ParseIcons(iconsJSON(entries, append([]string{idC}, bad...)...))
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Icons) != 1 || set.Icons[idA].PNG == nil {
		t.Errorf("kept %v, want only %s", slices.Sorted(maps.Keys(set.Icons)), idA)
	}
	if want := slices.Sorted(slices.Values(bad)); !slices.Equal(set.Dropped, want) {
		t.Errorf("Dropped = %v, want %v", set.Dropped, want)
	}
	if !slices.Equal(set.None, []string{idC}) {
		t.Errorf("None = %v, want only %s", set.None, idC)
	}
}

// iconFiles is repoFiles plus an icons file.
func iconFiles(t *testing.T, icons *fakeFile) map[string]*fakeFile {
	files := repoFiles(t)
	if icons != nil {
		files[IconsFile] = icons
	}
	return files
}

func oneIcon(t *testing.T, id string) []byte {
	return iconsJSON(map[string]string{id: b64(testPNG(t, 16, 16, false))}, idC)
}

func TestLoaderServesBakedIconsAndRevalidates(t *testing.T) {
	srv := newFakeServer(t, iconFiles(t, &fakeFile{body: oneIcon(t, idA), etag: `"icons-1"`}))
	clk := newClock()
	l := loaderFor(srv, clk)

	snap := l.Get(context.Background())
	requireFiles(t, snap, "rss.yaml", "academic.yaml")
	if len(snap.Icons) != 1 || snap.Icons[idA].ETag == "" {
		t.Fatalf("Icons = %v, want aaaa", snap.Icons)
	}
	if snap.Notice != "" {
		t.Errorf("Notice = %q", snap.Notice)
	}

	// Inside the TTL nothing is requested; past it the ETag is sent and a 304 keeps the icons.
	clk.advance(30 * time.Minute)
	l.Get(context.Background())
	settle(l)
	if n := srv.hitCount(IconsFile); n != 1 {
		t.Fatalf("icons hits = %d within the TTL, want 1", n)
	}
	clk.advance(31 * time.Minute)
	l.Get(context.Background())
	settle(l)
	if got := srv.ifNoneMatch(IconsFile); len(got) != 2 || got[1] != `"icons-1"` {
		t.Fatalf("If-None-Match = %q, want the second request to send icons-1", got)
	}
	if got := l.Snapshot().Icons; len(got) != 1 {
		t.Errorf("Icons = %v after a 304, want kept", got)
	}

	// New bytes replace the set.
	srv.set(IconsFile, func(f *fakeFile) {
		f.body = oneIcon(t, idB)
		f.etag = `"icons-2"`
	})
	clk.advance(2 * time.Hour)
	l.Snapshot()
	settle(l)
	if got := l.Snapshot().Icons; len(got) != 1 || got[idB].PNG == nil {
		t.Errorf("Icons = %v, want bbbb after new bytes", got)
	}
}

func TestLoaderMissingIconsFileIsNotAnError(t *testing.T) {
	t.Run("http 404", func(t *testing.T) {
		srv := newFakeServer(t, iconFiles(t, nil))
		clk := newClock()
		l := loaderFor(srv, clk)
		snap := l.Get(context.Background())
		requireFiles(t, snap, "rss.yaml", "academic.yaml")
		if snap.Icons != nil || snap.Notice != "" {
			t.Fatalf("snapshot = %+v, want no icons and no notice", snap)
		}
		// A missing file counts as understood: the TTL applies, there is no hammering.
		clk.advance(10 * time.Minute)
		l.Snapshot()
		settle(l)
		if n := srv.hitCount(IconsFile); n != 1 {
			t.Errorf("icons hits = %d, want 1 inside the TTL", n)
		}
	})

	t.Run("absent on disk", func(t *testing.T) {
		fsys := fstest.MapFS{
			"rss.yaml":      {Data: repoFile(t, "rss.yaml")},
			"academic.yaml": {Data: repoFile(t, "academic.yaml")},
		}
		clk := newClock()
		l := NewLoader(Options{Dir: fsys, Now: clk.now})
		snap := l.Get(t.Context())
		requireFiles(t, snap, "rss.yaml", "academic.yaml")
		if snap.Icons != nil || snap.Notice != "" {
			t.Fatalf("snapshot = %+v, want no icons and no notice", snap)
		}

		// The file appears, then goes away again.
		fsys[IconsFile] = &fstest.MapFile{Data: oneIcon(t, idA)}
		clk.advance(3 * time.Second)
		l.Snapshot()
		settle(l)
		if got := l.Snapshot().Icons; len(got) != 1 {
			t.Fatalf("Icons = %v, want the new file", got)
		}
		delete(fsys, IconsFile)
		clk.advance(3 * time.Second)
		l.Snapshot()
		settle(l)
		if got := l.Snapshot().Icons; got != nil {
			t.Errorf("Icons = %v after the file went away, want none", got)
		}
	})

	t.Run("only the icons file", func(t *testing.T) {
		fsys := fstest.MapFS{IconsFile: {Data: oneIcon(t, idA)}}
		snap := NewLoader(Options{Dir: fsys}).Get(t.Context())
		if snap.Loaded {
			t.Error("the icons file alone must not count as a loaded library")
		}
	})
}

func TestLoaderIconsFailuresKeepTheLastGood(t *testing.T) {
	srv := newFakeServer(t, iconFiles(t, &fakeFile{body: oneIcon(t, idA)}))
	clk := newClock()
	l := loaderFor(srv, clk)
	l.Get(context.Background())

	for name, mutate := range map[string]func(*fakeFile){
		"server error": func(f *fakeFile) { f.status = 500 },
		"garbage":      func(f *fakeFile) { f.status = 0; f.body = []byte("<html>") },
	} {
		srv.set(IconsFile, mutate)
		clk.advance(2 * time.Hour)
		l.Snapshot()
		settle(l)
		snap := l.Snapshot()
		if len(snap.Icons) != 1 || !snap.Loaded || snap.Notice != "" {
			t.Errorf("%s: snapshot = %+v, want the last good icons and no notice", name, snap)
		}
	}
}

// A file from a newer app is ignored without a notice: icons are cosmetic.
func TestLoaderNewerIconsVersionIsIgnoredQuietly(t *testing.T) {
	srv := newFakeServer(t, iconFiles(t, &fakeFile{body: []byte(`{"version": 2, "icons": {}}`)}))
	snap := loaderFor(srv, newClock()).Get(context.Background())
	requireFiles(t, snap, "rss.yaml", "academic.yaml")
	if snap.Notice != "" || snap.Icons != nil {
		t.Fatalf("snapshot = %+v, want no notice and no icons", snap)
	}

	// After a good set, a newer file keeps it.
	srv = newFakeServer(t, iconFiles(t, &fakeFile{body: oneIcon(t, idA)}))
	clk := newClock()
	l := loaderFor(srv, clk)
	l.Get(context.Background())
	srv.set(IconsFile, func(f *fakeFile) { f.body = []byte(`{"version": 2, "icons": {}}`) })
	clk.advance(2 * time.Hour)
	l.Snapshot()
	settle(l)
	snap = l.Snapshot()
	if snap.Notice != "" || len(snap.Icons) != 1 {
		t.Errorf("snapshot = %+v after a newer file, want the last good icons and no notice", snap)
	}
}

// A 404 after an earlier 200 means the library no longer ships icons.
func TestLoaderIconsFile404AfterOKClearsIcons(t *testing.T) {
	srv := newFakeServer(t, iconFiles(t, &fakeFile{body: oneIcon(t, idA), etag: `"icons-1"`}))
	clk := newClock()
	l := loaderFor(srv, clk)
	if got := l.Get(context.Background()).Icons; len(got) != 1 {
		t.Fatalf("Icons = %v, want one", got)
	}
	srv.set(IconsFile, func(f *fakeFile) { f.status = http.StatusNotFound })
	clk.advance(2 * time.Hour)
	l.Snapshot()
	settle(l)
	snap := l.Snapshot()
	if snap.Icons != nil || snap.Notice != "" || !snap.Loaded {
		t.Errorf("snapshot = %+v after a 404, want no icons, no notice, library kept", snap)
	}
}

func TestLoaderDropsBadIconsOnLoad(t *testing.T) {
	good := testPNG(t, 16, 16, false)
	body := iconsJSON(map[string]string{idGood: b64(good), idA: b64([]byte("nope"))})
	srv := newFakeServer(t, iconFiles(t, &fakeFile{body: body}))
	snap := loaderFor(srv, newClock()).Get(context.Background())
	if len(snap.Icons) != 1 || snap.Icons[idGood].PNG == nil {
		t.Fatalf("Icons = %v, want only good", snap.Icons)
	}
}

// committedIcons reads the checked-in icons file.
func committedIcons(t *testing.T) ([]byte, IconSet) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(libraryDir(), IconsFile))
	if err != nil {
		t.Fatalf("%v: run make library-icons", err)
	}
	set, err := ParseIcons(data)
	if err != nil {
		t.Fatal(err)
	}
	return data, set
}

// Every library source has an icon or a known none in the checked-in file.
func TestLibraryIconsCoverTheLibrary(t *testing.T) {
	lib, _ := ParseFS(os.DirFS(libraryDir()))
	_, set := committedIcons(t)
	for _, f := range MissingIcons(lib, set) {
		t.Error(f)
	}
	if len(set.Dropped) > 0 {
		t.Errorf("library/%s holds invalid icons: %v", IconsFile, set.Dropped)
	}
	live := map[string]bool{}
	for _, f := range lib.Files {
		for _, g := range f.Groups {
			for _, s := range g.Sources {
				live[config.FeedID(s.URL)] = true
			}
		}
	}
	for id := range set.Icons {
		if !live[id] {
			t.Errorf("library/%s has an icon for %s, which is no library source", IconsFile, id)
		}
	}
	for _, id := range set.None {
		if !live[id] {
			t.Errorf("library/%s lists %s as having no icon, which is no library source", IconsFile, id)
		}
	}
}

// The checked-in file is exactly what the generator writes, so a hand edit, a duplicate id or an
// id in both lists shows up here.
func TestLibraryIconsFileIsCanonical(t *testing.T) {
	data, set := committedIcons(t)
	if !bytes.Equal(data, MarshalIcons(set.Icons, set.None)) {
		t.Errorf("library/%s is not in the generated layout: run make library-icons", IconsFile)
	}
	seen := map[string]bool{}
	for _, id := range set.None {
		if seen[id] {
			t.Errorf("library/%s lists %s twice", IconsFile, id)
		}
		seen[id] = true
		if _, ok := set.Icons[id]; ok {
			t.Errorf("library/%s has %s in both icons and none", IconsFile, id)
		}
	}
}

func TestMissingIconsNamesTheSource(t *testing.T) {
	lib := &Library{Files: []File{{Groups: []Group{{Sources: []Source{
		{Name: "Has", URL: "https://has.test/feed"},
		{Name: "Quiet", URL: "https://quiet.test/feed"},
		{Name: "Fresh", URL: "https://fresh.test/feed"},
	}}}}}}
	set := IconSet{
		Icons: map[string]Icon{config.FeedID("https://has.test/feed"): {}},
		None:  []string{config.FeedID("https://quiet.test/feed")},
	}
	got := MissingIcons(lib, set)
	want := []string{"library/icons.json has no entry for Fresh: run make library-icons"}
	if !slices.Equal(got, want) {
		t.Fatalf("MissingIcons = %q, want %q", got, want)
	}
}

func TestLibraryIconsFileSizeBudget(t *testing.T) {
	data, _ := committedIcons(t)
	if len(data) > MaxIconsBytes {
		t.Fatalf("library/%s is %d bytes, over the %d budget", IconsFile, len(data), MaxIconsBytes)
	}
}
