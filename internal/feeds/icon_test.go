// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package feeds_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/httpclient"
)

const typeHTMLPlain = "text/html"

// solid is a w by h image of one color.
func solid(w, h int, c color.NRGBA) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = c.R, c.G, c.B, c.A
	}
	return img
}

var red = color.NRGBA{R: 200, G: 30, B: 30, A: 255}

func pngOf(img image.Image) []byte {
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		panic(err)
	}
	return b.Bytes()
}

func pngSolid(side int) string { return string(pngOf(solid(side, side, red))) }

func gifOf(side int) string {
	var b bytes.Buffer
	if err := gif.Encode(&b, solid(side, side, red), nil); err != nil {
		panic(err)
	}
	return b.String()
}

func jpegOf(side int) string {
	var b bytes.Buffer
	if err := jpeg.Encode(&b, solid(side, side, red), nil); err != nil {
		panic(err)
	}
	return b.String()
}

// noisePNG is an incompressible image of the given side.
func noisePNG(side int) string {
	img := image.NewNRGBA(image.Rect(0, 0, side, side))
	rand.New(rand.NewSource(1)).Read(img.Pix) //nolint:gosec // test data
	return string(pngOf(img))
}

type icoEntry struct {
	w, h    byte
	payload []byte
}

// icoOf builds an ICO file holding the given payloads.
func icoOf(entries ...icoEntry) []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, [3]uint16{0, 1, uint16(len(entries))})
	off := uint32(6 + 16*len(entries))
	for _, e := range entries {
		b.Write([]byte{e.w, e.h, 0, 0})
		_ = binary.Write(&b, binary.LittleEndian, [2]uint16{1, 32})
		_ = binary.Write(&b, binary.LittleEndian, [2]uint32{uint32(len(e.payload)), off})
		off += uint32(len(e.payload))
	}
	for _, e := range entries {
		b.Write(e.payload)
	}
	return b.Bytes()
}

// bmpPayload is the start of a BITMAPINFOHEADER image, as old ICOs hold.
func bmpPayload() []byte {
	p := make([]byte, 40+64)
	binary.LittleEndian.PutUint32(p, 40)
	binary.LittleEndian.PutUint32(p[4:], 16)
	binary.LittleEndian.PutUint32(p[8:], 32)
	return p
}

func width(t *testing.T, ic feeds.Icon) int {
	t.Helper()
	cfg, err := png.DecodeConfig(bytes.NewReader(ic.PNG))
	if err != nil {
		t.Fatalf("result is not a PNG: %v", err)
	}
	return cfg.Width
}

func png1(side int) resp { return resp{ctype: "image/png", body: pngSolid(side)} }

func fetch(t *testing.T, s *site) (feeds.Icon, error) {
	t.Helper()
	return feeds.FetchIcon(context.Background(), s.Client(), s.URL)
}

func TestFetchIconLinkParsing(t *testing.T) {
	cases := []struct {
		name  string
		links string
		paths map[string]resp
		side  int
	}{
		{"icon", `<link rel="icon" href="/a.png">`, map[string]resp{"/a.png": png1(40)}, 40},
		{"shortcut icon", `<link rel="shortcut icon" href="/a.png">`, map[string]resp{"/a.png": png1(41)}, 41},
		{"upper case rel", `<LINK REL="Icon" HREF="/a.png">`, map[string]resp{"/a.png": png1(42)}, 42},
		{"apple touch", `<link rel="apple-touch-icon" href="/a.png">`, map[string]resp{"/a.png": png1(43)}, 43},
		{"apple touch precomposed", `<link rel="apple-touch-icon-precomposed" href="/a.png">`,
			map[string]resp{"/a.png": png1(44)}, 44},
		{"relative href", `<link rel="icon" href="img/a.png">`, map[string]resp{"/img/a.png": png1(45)}, 45},
		{"base after the link", `<link rel="icon" href="a.png"><base href="/static/">`,
			map[string]resp{"/static/a.png": png1(46)}, 46},
		{"absolute href", `<link rel="icon" href="//HOST/abs.png">`, map[string]resp{"/abs.png": png1(47)}, 47},
		{"sizes list", `<link rel="icon" sizes="16x16 32x32" href="/small.png"><link rel="icon" href="/plain.png">`,
			map[string]resp{"/small.png": png1(20), "/plain.png": png1(21)}, 20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			paths := map[string]resp{}
			for k, v := range tc.paths {
				paths[k] = v
			}
			s := newSite(t, paths)
			links := strings.ReplaceAll(tc.links, "HOST", strings.TrimPrefix(s.URL, "http://"))
			s.mu.Lock()
			s.paths["/"] = head(links)
			s.mu.Unlock()
			ic, err := fetch(t, s)
			if err != nil {
				t.Fatalf("FetchIcon: %v", err)
			}
			if got := width(t, ic); got != tc.side {
				t.Errorf("icon is %dpx, want %d", got, tc.side)
			}
		})
	}
}

func TestFetchIconIgnoresMaskIconAndBodyLinks(t *testing.T) {
	s := newSite(t, map[string]resp{
		"/": {ctype: typeHTMLPlain, body: `<html><head><link rel="mask-icon" href="/mask.png">` +
			`</head><body><link rel="icon" href="/body.png"></body></html>`},
		"/mask.png":    png1(50),
		"/body.png":    png1(51),
		"/favicon.ico": png1(52),
	})
	ic, err := fetch(t, s)
	if err != nil {
		t.Fatal(err)
	}
	if got := width(t, ic); got != 52 {
		t.Errorf("icon is %dpx, want the /favicon.ico one", got)
	}
	if s.count("/mask.png") != 0 || s.count("/body.png") != 0 {
		t.Error("a mask icon or a link after <body> was fetched")
	}
}

func TestFetchIconSelection(t *testing.T) {
	t.Run("smallest png of at least 32px wins", func(t *testing.T) {
		s := newSite(t, map[string]resp{
			"/": head(`<link rel="icon" sizes="16x16" href="/s16.png">` +
				`<link rel="icon" sizes="192x192" href="/s192.png">` +
				`<link rel="icon" sizes="48x48" href="/s48.png">` +
				`<link rel="icon" sizes="32x32" href="/s32.png">` +
				`<link rel="apple-touch-icon" sizes="180x180" href="/apple.png">`),
			"/s16.png": png1(16), "/s192.png": png1(192), "/s48.png": png1(48), "/s32.png": png1(32),
			"/apple.png": png1(180),
		})
		ic, err := fetch(t, s)
		if err != nil {
			t.Fatal(err)
		}
		if got := width(t, ic); got != 32 {
			t.Errorf("icon is %dpx, want 32", got)
		}
		if n := s.total(); n != 2 {
			t.Errorf("%d requests, want the page and one icon", n)
		}
	})

	t.Run("order is unknown, small, ico, apple, capped at three", func(t *testing.T) {
		s := newSite(t, map[string]resp{
			"/": head(`<link rel="apple-touch-icon" href="/apple.png">` +
				`<link rel="icon" href="/x.ico" type="image/x-icon">` +
				`<link rel="icon" sizes="16x16" href="/small.png">` +
				`<link rel="icon" href="/unknown.png">`),
			"/apple.png": {
				body: "no",
			},
			"/x.ico":       {body: "no"},
			"/small.png":   {body: "no"},
			"/unknown.png": {body: "no"},
			"/favicon.ico": {body: "no"},
		})
		var order []string
		var mu sync.Mutex
		inner := s.Config.Handler
		s.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			order = append(order, r.URL.Path)
			mu.Unlock()
			inner.ServeHTTP(w, r)
		})
		if _, err := fetch(t, s); !errors.Is(err, feeds.ErrNoIcon) {
			t.Fatalf("err = %v", err)
		}
		want := "/ /unknown.png /small.png /x.ico /favicon.ico"
		if got := strings.Join(order, " "); got != want {
			t.Errorf("requests = %q, want %q", got, want)
		}
	})

	t.Run("at most three candidates then favicon.ico", func(t *testing.T) {
		var links strings.Builder
		paths := map[string]resp{"/favicon.ico": png1(30)}
		for i := range 6 {
			fmt.Fprintf(&links, `<link rel="icon" href="/bad%d.png">`, i)
			paths[fmt.Sprintf("/bad%d.png", i)] = resp{ctype: "image/png", body: "not an image"}
		}
		paths["/"] = head(links.String())
		s := newSite(t, paths)
		ic, err := fetch(t, s)
		if err != nil {
			t.Fatal(err)
		}
		if width(t, ic) != 30 {
			t.Error("did not fall through to /favicon.ico")
		}
		if n := s.total(); n != 5 {
			t.Errorf("%d requests, want page + 3 candidates + favicon.ico", n)
		}
	})

	for name, page := range map[string]resp{
		"no declared icon":   head(""),
		"page answers 404":   {status: 404, ctype: typeHTMLPlain, body: "nope"},
		"page answers 500":   {status: 500, ctype: typeHTMLPlain, body: `<head><link rel="icon" href="/declared.png">`},
		"page missing":       {status: 404},
		"icon link is a 404": head(`<link rel="icon" href="/gone.png">`),
	} {
		t.Run("falls back to favicon.ico: "+name, func(t *testing.T) {
			s := newSite(t, map[string]resp{"/": page, "/favicon.ico": png1(33), "/declared.png": png1(60)})
			ic, err := fetch(t, s)
			if err != nil {
				t.Fatal(err)
			}
			if got := width(t, ic); got != 33 {
				t.Errorf("icon is %dpx, want the favicon.ico one", got)
			}
		})
	}
}

func TestFetchIconFormats(t *testing.T) {
	pngBody := pngOf(solid(24, 24, red))
	cases := []struct {
		name string
		ctyp string
		body string
		ok   bool
	}{
		{"png", "image/png", pngSolid(24), true},
		{"gif", "image/gif", gifOf(24), true},
		{"jpeg", "image/jpeg", jpegOf(24), true},
		{"png under a wrong declared type", "text/plain", pngSolid(24), true},
		{"gif under a png type", "image/png", gifOf(24), true},
		{"ico holding png", "image/x-icon", string(icoOf(icoEntry{24, 24, pngBody})), true},
		{"ico holding bmp", "image/x-icon", string(icoOf(icoEntry{16, 16, bmpPayload()})), false},
		{"webp", "image/webp", "RIFF\x24\x00\x00\x00WEBPVP8 \x18\x00\x00\x00" + strings.Repeat("\x00", 24), false},
		{"html under a png type", "image/png", "<!doctype html><html><body>not found</body></html>", false},
		{"svg declared", "image/svg+xml", `<svg xmlns="http://www.w3.org/2000/svg" width="8" height="8"/>`, false},
		{"svg declared as png", "image/png", `<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"/>`, false},
		{"plain text", "text/plain", "hello", false},
		{"empty", "image/png", "", false},
		{"truncated png", "image/png", pngSolid(24)[:40], false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newSite(t, map[string]resp{"/favicon.ico": {ctype: tc.ctyp, body: tc.body}})
			ic, err := fetch(t, s)
			if tc.ok {
				if err != nil {
					t.Fatalf("FetchIcon: %v", err)
				}
				if width(t, ic) != 24 {
					t.Errorf("width = %d, want 24", width(t, ic))
				}
				return
			}
			if !errors.Is(err, feeds.ErrNoIcon) {
				t.Fatalf("err = %v, want ErrNoIcon", err)
			}
		})
	}
}

func TestFetchIconSkipsSVGAndDataLinks(t *testing.T) {
	s := newSite(t, map[string]resp{
		"/": head(`<link rel="icon" type="image/svg+xml" href="/typed">` +
			`<link rel="icon" href="/logo.SVG">` +
			`<link rel="icon" href="data:image/png;base64,iVBORw0KGgo=">` +
			`<link rel="icon" href="javascript:alert(1)">`),
		"/typed": {ctype: "image/svg+xml", body: "<svg/>"}, "/logo.SVG": {ctype: "image/svg+xml", body: "<svg/>"},
		"/favicon.ico": png1(26),
	})
	ic, err := fetch(t, s)
	if err != nil {
		t.Fatal(err)
	}
	if width(t, ic) != 26 {
		t.Error("expected the /favicon.ico fallback")
	}
	if s.count("/typed") != 0 || s.count("/logo.SVG") != 0 {
		t.Error("an svg link was fetched")
	}
}

func TestFetchIconSoftFavicon404IsRejected(t *testing.T) {
	s := newSite(t, map[string]resp{"/favicon.ico": {ctype: "image/x-icon", body: "<html>custom 404 page</html>"}})
	if _, err := fetch(t, s); !errors.Is(err, feeds.ErrNoIcon) {
		t.Fatalf("err = %v, want ErrNoIcon", err)
	}
}

func TestFetchIconCaps(t *testing.T) {
	t.Run("body over the read cap", func(t *testing.T) {
		s := newSite(t, map[string]resp{"/favicon.ico": {ctype: "image/png", body: noisePNG(300)}})
		if len(s.paths["/favicon.ico"].body) <= feeds.IconReadBytes {
			t.Fatal("fixture is under the cap")
		}
		if _, err := fetch(t, s); !errors.Is(err, feeds.ErrNoIcon) {
			t.Fatalf("err = %v, want ErrNoIcon", err)
		}
	})

	t.Run("side over 512", func(t *testing.T) {
		s := newSite(t, map[string]resp{"/favicon.ico": png1(513)})
		if _, err := fetch(t, s); !errors.Is(err, feeds.ErrNoIcon) {
			t.Fatalf("err = %v, want ErrNoIcon", err)
		}
	})

	t.Run("side of exactly 512 is accepted", func(t *testing.T) {
		s := newSite(t, map[string]resp{"/favicon.ico": png1(512)})
		if _, err := fetch(t, s); err != nil {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("wide image over 512", func(t *testing.T) {
		wide := pngOf(solid(600, 4, red))
		s := newSite(t, map[string]resp{"/favicon.ico": {ctype: "image/png", body: string(wide)}})
		if _, err := fetch(t, s); !errors.Is(err, feeds.ErrNoIcon) {
			t.Fatalf("err = %v, want ErrNoIcon", err)
		}
	})

	t.Run("zero sized image", func(t *testing.T) {
		b := pngOf(solid(8, 8, red))
		binary.BigEndian.PutUint32(b[16:], 0) // IHDR width
		binary.BigEndian.PutUint32(b[29:], crc32.ChecksumIEEE(b[12:29]))
		s := newSite(t, map[string]resp{"/favicon.ico": {ctype: "image/png", body: string(b)}})
		if _, err := fetch(t, s); !errors.Is(err, feeds.ErrNoIcon) {
			t.Fatalf("err = %v, want ErrNoIcon", err)
		}
		if _, err := feeds.DecodeIcon(b); err == nil {
			t.Fatal("DecodeIcon accepted a zero width")
		}
	})

	t.Run("png over the stored cap moves to the next candidate", func(t *testing.T) {
		noise := noisePNG(150)
		if len(noise) > feeds.IconReadBytes {
			t.Fatal("fixture exceeds the read cap")
		}
		s := newSite(t, map[string]resp{
			"/":          head(`<link rel="icon" href="/noise.png"><link rel="icon" href="/ok.png">`),
			"/noise.png": {ctype: "image/png", body: noise},
			"/ok.png":    png1(34),
		})
		ic, err := fetch(t, s)
		if err != nil {
			t.Fatal(err)
		}
		if width(t, ic) != 34 {
			t.Error("did not move on to the second candidate")
		}
		if len(ic.PNG) > feeds.IconStoredBytes {
			t.Errorf("stored %d bytes", len(ic.PNG))
		}
	})

	t.Run("html scan stops at the cap", func(t *testing.T) {
		pad := strings.Repeat("<!-- x -->", feeds.IconHTMLBytes/10)
		s := newSite(t, map[string]resp{
			"/":            {ctype: typeHTMLPlain, body: "<head>" + pad + `<link rel="icon" href="/late.png">`},
			"/late.png":    png1(35),
			"/favicon.ico": png1(36),
		})
		ic, err := fetch(t, s)
		if err != nil {
			t.Fatal(err)
		}
		if width(t, ic) != 36 {
			t.Error("a link past the HTML cap was used")
		}
	})
}

func TestFetchIconReencodesAsPNG(t *testing.T) {
	s := newSite(t, map[string]resp{"/favicon.ico": {ctype: "image/jpeg", body: jpegOf(20)}})
	ic, err := fetch(t, s)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(ic.PNG, []byte("\x89PNG\r\n\x1a\n")) {
		t.Error("result is not a PNG")
	}
}

func TestIconDark(t *testing.T) {
	glyph := func(fg color.NRGBA) image.Image {
		img := image.NewNRGBA(image.Rect(0, 0, 16, 16))
		for y := 4; y < 12; y++ {
			for x := 4; x < 12; x++ {
				img.SetNRGBA(x, y, fg)
			}
		}
		return img
	}
	// split paints an 8x8 glyph on a transparent 16x16 canvas: its first n columns in a, the rest in b.
	split := func(n int, a, b color.NRGBA) image.Image {
		img := image.NewNRGBA(image.Rect(0, 0, 16, 16))
		for y := 4; y < 12; y++ {
			for x := 4; x < 12; x++ {
				c := b
				if x-4 < n {
					c = a
				}
				img.SetNRGBA(x, y, c)
			}
		}
		return img
	}
	// Colours of the arXiv and GitHub site icons: a dark red stroke beside a light grey one, and
	// near black.
	arxivRed := color.NRGBA{R: 179, G: 32, B: 37, A: 255}
	arxivGrey := color.NRGBA{R: 179, G: 172, B: 165, A: 255}
	githubBlack := color.NRGBA{R: 24, G: 22, B: 22, A: 255}
	cases := []struct {
		name string
		img  image.Image
		dark bool
	}{
		{"arxiv: half dark red, half light grey, mostly transparent", split(4, arxivRed, arxivGrey), true},
		{"github: near black glyph", glyph(githubBlack), true},
		{"bright logo with a dark minority", split(2, arxivRed, arxivGrey), false},
		{"black glyph on transparent", glyph(color.NRGBA{A: 255}), true},
		{"white glyph on transparent", glyph(color.NRGBA{R: 255, G: 255, B: 255, A: 255}), false},
		{"mid grey glyph", glyph(color.NRGBA{R: 128, G: 128, B: 128, A: 255}), false},
		{"opaque black square", solid(16, 16, color.NRGBA{A: 255}), false},
		{"empty image", image.NewNRGBA(image.Rect(0, 0, 16, 16)), false},
		{"coloured logo", solid(16, 16, red), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ic, err := feeds.DecodeIcon(pngOf(tc.img))
			if err != nil {
				t.Fatal(err)
			}
			if ic.Dark != tc.dark {
				t.Errorf("Dark = %v, want %v", ic.Dark, tc.dark)
			}
		})
	}
}

// icoFixtures are malformed ICO inputs; none may panic or be accepted.
func icoFixtures() map[string][]byte {
	good := icoOf(icoEntry{16, 16, pngOf(solid(16, 16, red))})
	patch := func(b []byte, off int, v uint32) []byte {
		out := bytes.Clone(b)
		binary.LittleEndian.PutUint32(out[off:], v)
		return out
	}
	fx := map[string][]byte{
		"empty":              {},
		"one byte":           {0},
		"header only":        {0, 0, 1, 0, 1, 0},
		"zero entries":       {0, 0, 1, 0, 0, 0},
		"cursor type":        {0, 0, 2, 0, 1, 0, 16, 16, 0, 0, 1, 0, 32, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		"huge count":         append([]byte{0, 0, 1, 0, 0xff, 0xff}, make([]byte, 40)...),
		"truncated entry":    good[:6+10],
		"truncated data":     good[:len(good)-5],
		"size past end":      patch(good, 6+8, 0xffffffff),
		"offset past end":    patch(good, 6+12, 0xffffffff),
		"offset overflow":    patch(good, 6+12, 0xfffffff0),
		"size zero":          patch(good, 6+8, 0),
		"size tiny":          patch(good, 6+8, 3),
		"offset in header":   patch(good, 6+12, 0),
		"png signature only": icoOf(icoEntry{16, 16, []byte("\x89PNG\r\n\x1a\n")}),
		"png garbage": icoOf(
			icoEntry{16, 16, append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0xff}, 64)...)},
		),
		"bmp only":        icoOf(icoEntry{16, 16, bmpPayload()}),
		"zero byte entry": icoOf(icoEntry{0, 0, nil}),
	}
	// Every prefix of a valid file.
	for n := range len(good) {
		fx[fmt.Sprintf("prefix %d", n)] = good[:n]
	}
	// Single byte corruptions of the header and the directory entry.
	for i := range 6 + 16 {
		b := bytes.Clone(good)
		b[i] ^= 0xff
		fx[fmt.Sprintf("flip %d", i)] = b
	}
	return fx
}

func TestDecodeIconMalformedICONeverPanics(t *testing.T) {
	good := icoOf(icoEntry{16, 16, pngOf(solid(16, 16, red))})
	if _, err := feeds.DecodeIcon(good); err != nil {
		t.Fatalf("the good fixture is rejected: %v", err)
	}
	for name, data := range icoFixtures() {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic: %v", r)
				}
			}()
			ic, err := feeds.DecodeIcon(data)
			// A bit flip in a field the parser does not need (planes, bit count,
			// colour fields) leaves a valid file; only the rest must be refused.
			if err == nil && !strings.HasPrefix(name, "flip") {
				t.Fatalf("accepted %d bytes (png %d)", len(data), len(ic.PNG))
			}
		})
	}
}

func FuzzDecodeIcon(f *testing.F) {
	for _, data := range icoFixtures() {
		f.Add(data)
	}
	f.Add([]byte(pngSolid(8)))
	f.Add([]byte(gifOf(8)))
	f.Add([]byte(jpegOf(8)))
	f.Fuzz(func(_ *testing.T, data []byte) {
		_, _ = feeds.DecodeIcon(data)
	})
}

func TestIcoPicksSizeByPreference(t *testing.T) {
	file := icoOf(
		icoEntry{16, 16, pngOf(solid(16, 16, red))},
		icoEntry{48, 48, pngOf(solid(48, 48, red))},
		icoEntry{32, 32, pngOf(solid(32, 32, red))},
		icoEntry{0, 0, pngOf(solid(256, 256, red))},
	)
	ic, err := feeds.DecodeIcon(file)
	if err != nil {
		t.Fatal(err)
	}
	if got := width(t, ic); got != 32 {
		t.Errorf("picked %dpx, want 32", got)
	}
	small := icoOf(icoEntry{8, 8, pngOf(solid(8, 8, red))}, icoEntry{16, 16, pngOf(solid(16, 16, red))})
	if ic, err = feeds.DecodeIcon(small); err != nil || width(t, ic) != 16 {
		t.Errorf("small ico: %v, %v", width(t, ic), err)
	}
}

// hostSite serves per-host, per-path responses and counts hits as "host/path".
type hostSite struct {
	*httptest.Server
	mu    sync.Mutex
	paths map[string]resp
	hits  map[string]int
}

func newHostSite(t *testing.T, paths map[string]resp) (*hostSite, *http.Client) {
	t.Helper()
	h := &hostSite{paths: paths, hits: map[string]int{}}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Host + r.URL.Path
		h.mu.Lock()
		h.hits[key]++
		rp, ok := h.paths[key]
		h.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		if rp.delay > 0 {
			select {
			case <-time.After(rp.delay):
			case <-r.Context().Done():
				return
			}
		}
		if rp.ctype != "" {
			w.Header().Set("Content-Type", rp.ctype)
		}
		_, _ = w.Write([]byte(rp.body))
	}))
	t.Cleanup(h.Close)
	addr := h.Listener.Addr().String()
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}}
	return h, client
}

func (h *hostSite) hitsFor(host string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for k, v := range h.hits {
		if strings.HasPrefix(k, host+"/") {
			n += v
		}
	}
	return n
}

func TestFetchIconParentFallback(t *testing.T) {
	t.Run("subdomain falls back to its domain", func(t *testing.T) {
		h, client := newHostSite(t, map[string]resp{"example.com/favicon.ico": png1(28)})
		ic, err := feeds.FetchIcon(context.Background(), client, "http://rss.example.com/feed")
		if err != nil {
			t.Fatal(err)
		}
		if width(t, ic) != 28 {
			t.Error("wrong icon")
		}
		if h.hitsFor("rss.example.com") == 0 {
			t.Error("the subdomain itself was not tried first")
		}
	})

	t.Run("multi label public suffix", func(t *testing.T) {
		h, client := newHostSite(t, map[string]resp{"bar.co.uk/favicon.ico": png1(29)})
		ic, err := feeds.FetchIcon(context.Background(), client, "http://foo.bar.co.uk/")
		if err != nil {
			t.Fatal(err)
		}
		if width(t, ic) != 29 {
			t.Error("wrong icon")
		}
		if h.hitsFor("co.uk") != 0 {
			t.Error("fell back to the public suffix")
		}
	})

	t.Run("the domain itself does not fall back", func(t *testing.T) {
		h, client := newHostSite(t, map[string]resp{})
		if _, err := feeds.FetchIcon(
			context.Background(),
			client,
			"http://example.com/",
		); !errors.Is(
			err,
			feeds.ErrNoIcon,
		) {
			t.Fatalf("err = %v", err)
		}
		if h.hitsFor("co.uk") != 0 || h.hitsFor("com") != 0 {
			t.Error("tried a public suffix")
		}
	})

	t.Run("not after a timeout", func(t *testing.T) {
		h, client := newHostSite(t, map[string]resp{
			"rss.example.com/":        {ctype: typeHTMLPlain, delay: 3 * time.Second},
			"example.com/favicon.ico": png1(28),
		})
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := feeds.FetchIcon(ctx, client, "http://rss.example.com/")
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want DeadlineExceeded", err)
		}
		if time.Since(start) > 2*time.Second {
			t.Errorf("took %v", time.Since(start))
		}
		if h.hitsFor("example.com") != 0 {
			t.Error("fell back to the parent after a timeout")
		}
	})

	t.Run("not after a transport error", func(t *testing.T) {
		h, client := newHostSite(t, map[string]resp{"example.com/favicon.ico": png1(28)})
		hc := &http.Client{Transport: refuseHost{host: "rss.example.com", next: client.Transport}}
		_, err := feeds.FetchIcon(context.Background(), hc, "http://rss.example.com/")
		if !errors.Is(err, httpclient.ErrBlockedAddress) {
			t.Fatalf("err = %v", err)
		}
		if h.hitsFor("example.com") != 0 {
			t.Error("fell back after a transport error")
		}
	})
}

func TestFetchIconFailures(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		s := newSite(t, map[string]resp{"/": {ctype: typeHTMLPlain, delay: 3 * time.Second}})
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := feeds.FetchIcon(ctx, s.Client(), s.URL)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want DeadlineExceeded", err)
		}
		if time.Since(start) > 2*time.Second {
			t.Errorf("took %v", time.Since(start))
		}
	})

	t.Run("slow icon is not an ErrNoIcon", func(t *testing.T) {
		s := newSite(
			t,
			map[string]resp{"/favicon.ico": {ctype: "image/png", body: pngSolid(8), delay: 3 * time.Second}},
		)
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		_, err := feeds.FetchIcon(ctx, s.Client(), s.URL)
		if err == nil || errors.Is(err, feeds.ErrNoIcon) {
			t.Fatalf("err = %v, want a context error", err)
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		s := newSite(t, map[string]resp{"/favicon.ico": png1(8)})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := feeds.FetchIcon(ctx, s.Client(), s.URL); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want Canceled", err)
		}
	})

	t.Run("redirect to a blocked address", func(t *testing.T) {
		s := newSite(t, map[string]resp{"/": {redirect: "http://internal.test/"}})
		client := &http.Client{Transport: refuseHost{host: "internal.test", next: s.Client().Transport}}
		_, err := feeds.FetchIcon(context.Background(), client, s.URL)
		if !errors.Is(err, httpclient.ErrBlockedAddress) {
			t.Fatalf("err = %v, want ErrBlockedAddress", err)
		}
	})

	t.Run("a blocked icon link does not stop the next candidate", func(t *testing.T) {
		s := newSite(t, map[string]resp{
			"/": head(
				`<link rel="icon" href="http://internal.test/i.png"><link rel="icon" href="/good.png">`,
			),
			"/good.png":    png1(36),
			"/favicon.ico": png1(20),
		})
		client := &http.Client{Transport: refuseHost{host: "internal.test", next: s.Client().Transport}}
		ic, err := feeds.FetchIcon(context.Background(), client, s.URL)
		if err != nil {
			t.Fatal(err)
		}
		if got := width(t, ic); got != 36 {
			t.Errorf("icon is %dpx, want the one after the blocked link", got)
		}
	})

	t.Run("a blocked icon link falls through to favicon.ico", func(t *testing.T) {
		s := newSite(t, map[string]resp{
			"/":            head(`<link rel="icon" href="http://internal.test/i.png">`),
			"/favicon.ico": png1(22),
		})
		client := &http.Client{Transport: refuseHost{host: "internal.test", next: s.Client().Transport}}
		ic, err := feeds.FetchIcon(context.Background(), client, s.URL)
		if err != nil {
			t.Fatal(err)
		}
		if got := width(t, ic); got != 22 {
			t.Errorf("icon is %dpx, want the favicon.ico one", got)
		}
	})

	t.Run("all candidates blocked or empty returns the transport error", func(t *testing.T) {
		s := newSite(t, map[string]resp{
			"/":         head(`<link rel="icon" href="http://internal.test/i.png"><link rel="icon" href="/junk.png">`),
			"/junk.png": {ctype: "image/png", body: "not an image"},
		})
		client := &http.Client{Transport: refuseHost{host: "internal.test", next: s.Client().Transport}}
		_, err := feeds.FetchIcon(context.Background(), client, s.URL)
		if !errors.Is(err, httpclient.ErrBlockedAddress) {
			t.Fatalf("err = %v, want ErrBlockedAddress", err)
		}
	})

	t.Run("all candidates blocked", func(t *testing.T) {
		s := newSite(t, map[string]resp{
			"/": head(
				`<link rel="icon" href="http://internal.test/a.png"><link rel="icon" href="http://internal.test/b.png">`,
			),
		})
		client := &http.Client{Transport: refuseHost{host: "internal.test", next: s.Client().Transport}}
		_, err := feeds.FetchIcon(context.Background(), client, s.URL)
		if !errors.Is(err, httpclient.ErrBlockedAddress) {
			t.Fatalf("err = %v, want ErrBlockedAddress", err)
		}
	})

	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprintf("icon answering %d is transient", status), func(t *testing.T) {
			s := newSite(t, map[string]resp{
				"/":            head(`<link rel="icon" href="/a.png">`),
				"/a.png":       {status: status},
				"/favicon.ico": {status: 404},
			})
			_, err := fetch(t, s)
			if err == nil || errors.Is(err, feeds.ErrNoIcon) {
				t.Fatalf("err = %v, want a transient failure", err)
			}

			s = newSite(t, map[string]resp{
				"/":            head(`<link rel="icon" href="/a.png">`),
				"/a.png":       {status: status},
				"/favicon.ico": png1(26),
			})
			ic, err := fetch(t, s)
			if err != nil || width(t, ic) != 26 {
				t.Fatalf("icon = %v, err = %v, want the favicon.ico one", ic, err)
			}
		})
	}

	for _, status := range []int{http.StatusNotFound, http.StatusGone, http.StatusForbidden} {
		t.Run(fmt.Sprintf("icon answering %d is a missing icon", status), func(t *testing.T) {
			s := newSite(t, map[string]resp{
				"/":            head(`<link rel="icon" href="/a.png">`),
				"/a.png":       {status: status},
				"/favicon.ico": {status: status},
			})
			if _, err := fetch(t, s); !errors.Is(err, feeds.ErrNoIcon) {
				t.Fatalf("err = %v, want ErrNoIcon", err)
			}
		})
	}

	t.Run("not a site address", func(t *testing.T) {
		for _, raw := range []string{"", "ftp://example.com", "javascript:alert(1)", "example.com", "http://"} {
			if _, err := feeds.FetchIcon(context.Background(), nil, raw); !errors.Is(err, feeds.ErrNoIcon) {
				t.Errorf("%q: err = %v, want ErrNoIcon", raw, err)
			}
		}
	})

	t.Run("crossref makes no request", func(t *testing.T) {
		h, client := newHostSite(t, map[string]resp{"api.crossref.org/favicon.ico": png1(8)})
		_, err := feeds.FetchIcon(context.Background(), client, "https://api.crossref.org/works")
		if !errors.Is(err, feeds.ErrNoIcon) {
			t.Fatalf("err = %v", err)
		}
		if h.hitsFor("api.crossref.org") != 0 || h.hitsFor("crossref.org") != 0 {
			t.Error("a crossref host was requested")
		}
	})
}

func TestFetchIconGuardedClientBlocksPrivateAddress(t *testing.T) {
	t.Setenv(httpclient.AllowPrivateEnv, "")
	s := newSite(
		t,
		map[string]resp{"/": head(`<link rel="icon" href="/a.png">`), "/a.png": png1(32), "/favicon.ico": png1(32)},
	)
	_, err := feeds.FetchIcon(context.Background(), feeds.NewClient(""), s.URL)
	if !errors.Is(err, httpclient.ErrBlockedAddress) {
		t.Fatalf("err = %v, want ErrBlockedAddress", err)
	}
	if n := s.total(); n != 0 {
		t.Errorf("the blocked address received %d requests", n)
	}
}

func TestFetchIconNilClientUsesGuardedDefault(t *testing.T) {
	t.Setenv(httpclient.AllowPrivateEnv, "")
	s := newSite(t, map[string]resp{"/favicon.ico": png1(32)})
	if _, err := feeds.FetchIcon(context.Background(), nil, s.URL); !errors.Is(err, httpclient.ErrBlockedAddress) {
		t.Fatalf("err = %v, want ErrBlockedAddress", err)
	}
}

func TestFetchIconSendsIconAccept(t *testing.T) {
	var accept string
	s := newSite(t, map[string]resp{"/favicon.ico": png1(32)})
	inner := s.Config.Handler
	s.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/favicon.ico" {
			accept = r.Header.Get("Accept")
		}
		inner.ServeHTTP(w, r)
	})
	if _, err := fetch(t, s); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(accept, "image/") {
		t.Errorf("Accept = %q", accept)
	}
}

func TestSiteKey(t *testing.T) {
	cases := map[string]string{
		"https://www.Example.com/blog": "example.com",
		"http://example.com":           "example.com",
		"https://blog.example.com/x":   "blog.example.com",
		"https://example.com:8443/":    "example.com:8443",
		"  https://example.com/a  ":    "example.com",
		"https://user:pw@example.com/": "example.com",
		"https://www.www.example.com":  "www.example.com",
		"ftp://example.com":            "",
		"javascript:alert(1)":          "",
		"data:text/html,hi":            "",
		"//example.com":                "",
		"example.com":                  "",
		"https://":                     "",
		"":                             "",
	}
	for in, want := range cases {
		if got := feeds.SiteKey(in); got != want {
			t.Errorf("SiteKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSiteOrigin(t *testing.T) {
	cases := map[string]string{
		"https://www.Example.com/blog?x=1#f": "https://www.example.com",
		"HTTP://example.com:8080/a":          "http://example.com:8080",
		"https://user:pw@example.com/":       "https://example.com",
		"ftp://example.com":                  "",
		"mailto:a@example.com":               "",
		"/relative":                          "",
		"":                                   "",
	}
	for in, want := range cases {
		if got := feeds.SiteOrigin(in); got != want {
			t.Errorf("SiteOrigin(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNoIconHost(t *testing.T) {
	cases := map[string]bool{
		"crossref.org":          true,
		"api.crossref.org":      true,
		"API.Crossref.org":      true,
		"api.crossref.org:443":  true,
		"crossref.org.":         true,
		"notcrossref.org":       false,
		"crossref.org.evil.com": false,
		"example.com":           false,
		"":                      false,
	}
	for in, want := range cases {
		if got := feeds.NoIconHost(in); got != want {
			t.Errorf("NoIconHost(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestFetchAllSetsSite(t *testing.T) {
	const atomSite = `<?xml version="1.0"?><feed xmlns="http://www.w3.org/2005/Atom"><title>A</title>
<link rel="self" href="https://feeds.example.com/atom.xml"/><link rel="alternate" href="https://blog.example.com/"/>
<entry><title>One</title><id>1</id><updated>2024-03-01T00:00:00Z</updated></entry></feed>`
	const atomSelfOnly = `<?xml version="1.0"?><feed xmlns="http://www.w3.org/2005/Atom"><title>A</title>
<link rel="self" href="https://feeds.example.com/atom.xml"/>
<entry><title>One</title><id>1</id><updated>2024-03-01T00:00:00Z</updated></entry></feed>`
	const rssSite = `<?xml version="1.0"?><rss version="2.0"><channel><title>R</title><link>https://rss.example.com/</link>
<item><title>One</title><guid>1</guid></item></channel></rss>`
	const rssBadLink = `<?xml version="1.0"?><rss version="2.0"><channel><title>R</title><link>javascript:alert(1)</link>
<item><title>One</title><guid>1</guid></item></channel></rss>`
	const rssRelative = `<?xml version="1.0"?><rss version="2.0"><channel><title>R</title><link>/blog/</link>
<item><title>One</title><guid>1</guid></item></channel></rss>`
	const rssNoLink = `<?xml version="1.0"?><rss version="2.0"><channel><title>R</title>
<item><title>One</title><guid>1</guid></item></channel></rss>`

	s := newSite(t, map[string]resp{
		"/atom":    {ctype: typeAtom, body: atomSite},
		"/self":    {ctype: typeAtom, body: atomSelfOnly},
		"/rss":     {ctype: typeRSS, body: rssSite},
		"/badlink": {ctype: typeRSS, body: rssBadLink},
		"/nolink":  {ctype: typeRSS, body: rssNoLink},
		"/rel":     {ctype: typeRSS, body: rssRelative},
		"/missing": {status: 500},
	})
	srcs := []feeds.Source{
		{Name: "atom", URL: s.URL + "/atom"}, {Name: "rss", URL: s.URL + "/rss"},
		{Name: "nolink", URL: s.URL + "/nolink"}, {Name: "bad", URL: s.URL + "/badlink"},
		{Name: "self", URL: s.URL + "/self"}, {Name: "fail", URL: s.URL + "/missing"},
		{Name: "relative", URL: s.URL + "/rel"},
	}
	got := feeds.FetchAll(context.Background(), s.Client(), srcs)
	want := []string{
		"https://blog.example.com/",
		"https://rss.example.com/",
		"",
		"",
		"",
		"",
		s.URL + "/blog/",
	}
	for i, r := range got {
		if r.Site != want[i] {
			t.Errorf("%s: Site = %q, want %q", srcs[i].Name, r.Site, want[i])
		}
	}
}
