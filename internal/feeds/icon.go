// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package feeds

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/draw"
	_ "image/gif"  // registers the GIF decoder
	_ "image/jpeg" // registers the JPEG decoder
	"image/png"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/publicsuffix"

	"github.com/DanielBlei/rabbithole/internal/httpclient"
)

const (
	// IconHTMLBytes caps how much of a site's home page is scanned for icon links.
	IconHTMLBytes = 512 << 10
	// IconReadBytes caps one downloaded icon.
	IconReadBytes = 256 << 10
	// IconStoredBytes caps an icon after it is re-encoded to PNG.
	IconStoredBytes = 64 << 10

	iconCallTimeout    = 12 * time.Second
	iconRequestTimeout = 5 * time.Second
	// IconMaxSide is the largest accepted width or height in pixels.
	IconMaxSide = 512
	// iconGoodSide is the smallest side a declared icon needs to rank first.
	iconGoodSide = 32
	// iconCandidates is how many declared icons are tried before /favicon.ico.
	iconCandidates = 3
	// icoTries is how many payloads of one ICO are tried.
	icoTries = 3
	// darkLuma is the Rec.709 luma (0 to 255) below which a visible pixel counts as dark.
	darkLuma = 90
	// darkShare is the percentage of visible pixels that must be dark for a glyph to count as dark.
	darkShare = 40
)

// ErrNoIcon means the site has no icon this package accepts.
var ErrNoIcon = errors.New("no usable icon")

// errBadIcon marks a candidate rejected for its content, so the next one is tried.
var errBadIcon = errors.New("unusable icon")

// Icon is a site icon re-encoded as PNG. Dark reports a dark glyph on a
// transparent background, which needs a light plate to be visible.
type Icon struct {
	PNG  []byte
	Dark bool
}

// SiteKey is the lowercase host of an http(s) address without a leading "www.",
// or "" for anything else. Two addresses with one key share an icon.
func SiteKey(raw string) string {
	u := httpURL(raw)
	if u == nil {
		return ""
	}
	return strings.TrimPrefix(strings.ToLower(u.Host), "www.")
}

// SiteOrigin is scheme://host of an http(s) address, or "" for anything else.
func SiteOrigin(raw string) string {
	u := httpURL(raw)
	if u == nil {
		return ""
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
}

func httpURL(raw string) *url.URL {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return nil
	}
	if s := strings.ToLower(u.Scheme); s != "http" && s != "https" {
		return nil
	}
	return u
}

// NoIconHost reports a host that never serves a site icon: crossref.org and its
// subdomains.
func NoIconHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(host, ".")
	return host == "crossref.org" || strings.HasSuffix(host, ".crossref.org")
}

// FetchIcon finds and downloads the icon of the site at site. It returns
// ErrNoIcon when the site and, for a subdomain, its registrable parent offer no
// usable icon. Transport, timeout and context errors are returned as they are,
// without trying the parent. Every request goes through client.
func FetchIcon(ctx context.Context, client *http.Client, site string) (Icon, error) {
	origin := SiteOrigin(site)
	if origin == "" || NoIconHost(SiteKey(site)) {
		return Icon{}, ErrNoIcon
	}
	if client == nil {
		client = NewClient("")
	}
	ctx, cancel := context.WithTimeout(ctx, iconCallTimeout)
	defer cancel()

	icon, err := siteIcon(ctx, client, origin)
	if !errors.Is(err, ErrNoIcon) {
		return icon, err
	}
	if parent := parentOrigin(origin); parent != "" {
		return siteIcon(ctx, client, parent)
	}
	return Icon{}, err
}

// parentOrigin is the origin of the registrable domain above origin's host, or
// "" when the host is that domain, an IP or has no public suffix.
func parentOrigin(origin string) string {
	u, err := url.Parse(origin)
	if err != nil {
		return ""
	}
	host := u.Hostname()
	if net.ParseIP(host) != nil {
		return ""
	}
	parent, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil || parent == host {
		return ""
	}
	if port := u.Port(); port != "" {
		parent = net.JoinHostPort(parent, port)
	}
	return u.Scheme + "://" + parent
}

// siteIcon tries the icons a site's home page declares, then /favicon.ico. It
// returns ErrNoIcon when each failed for its content, the first transport or
// transient error when one failed that way, and a context error as it is.
func siteIcon(ctx context.Context, client *http.Client, origin string) (Icon, error) {
	final, links, err := homeIcons(ctx, client, origin)
	if err != nil {
		return Icon{}, err
	}
	urls := make([]string, 0, iconCandidates+1)
	for _, l := range links[:min(len(links), iconCandidates)] {
		urls = append(urls, l.href)
	}
	root := url.URL{Scheme: final.Scheme, Host: final.Host, Path: "/favicon.ico"}
	if s := root.String(); !slices.Contains(urls, s) {
		urls = append(urls, s)
	}

	// A candidate that cannot be reached does not stop the others; the first
	// such failure is what the site reports when none of them yields an icon.
	var transport error
	for _, u := range urls {
		icon, err := downloadIcon(ctx, client, u)
		switch {
		case err == nil:
			return icon, nil
		case ctx.Err() != nil:
			return Icon{}, err
		case !errors.Is(err, errBadIcon) && transport == nil:
			transport = err
		}
	}
	if transport != nil {
		return Icon{}, transport
	}
	return Icon{}, ErrNoIcon
}

// iconLink is a declared icon with its place in the ranking.
type iconLink struct {
	href  string
	class int // lower ranks first
	side  int // declared size, 0 when unknown
	order int // position in the document
}

// homeIcons fetches the home page and returns its final address and the icons
// its head declares, best first. A page that cannot be read as a page (non-2xx)
// declares none.
func homeIcons(ctx context.Context, client *http.Client, origin string) (*url.URL, []iconLink, error) {
	final, _ := url.Parse(origin + "/")
	var links []iconLink
	err := iconGet(ctx, client, origin+"/", "text/html,application/xhtml+xml;q=0.9,*/*;q=0.5",
		func(resp *http.Response) error {
			final = resp.Request.URL
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				return nil
			}
			var found []map[string]string
			base := headLinks(io.LimitReader(resp.Body, IconHTMLBytes), final, func(attrs map[string]string) {
				found = append(found, attrs)
			})
			seen := map[string]bool{}
			for i, attrs := range found {
				l, ok := rankIcon(attrs, base, i)
				if ok && !seen[l.href] {
					seen[l.href] = true
					links = append(links, l)
				}
			}
			return nil
		})
	if err != nil {
		return nil, nil, err
	}
	sort.SliceStable(links, func(i, j int) bool {
		a, b := links[i], links[j]
		if a.class != b.class {
			return a.class < b.class
		}
		if a.class == 0 && a.side != b.side {
			return a.side < b.side
		}
		return a.order < b.order
	})
	return final, links, nil
}

// rankIcon turns a <link> into a ranked candidate: sized PNGs first, smallest
// that is large enough, then PNGs of unknown size, small PNGs, ICOs and finally
// apple-touch icons. Mask icons, SVGs and non-http hrefs (data: included) are
// dropped.
func rankIcon(attrs map[string]string, base *url.URL, order int) (iconLink, bool) {
	rel := attrs["rel"]
	apple := hasToken(rel, "apple-touch-icon") || hasToken(rel, "apple-touch-icon-precomposed")
	if !apple && !hasToken(rel, "icon") {
		return iconLink{}, false
	}
	href := strings.TrimSpace(attrs["href"])
	if href == "" {
		return iconLink{}, false
	}
	u, err := base.Parse(href)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return iconLink{}, false
	}
	u.Fragment, u.RawFragment = "", ""
	ext := strings.ToLower(path.Ext(u.Path))
	typ := strings.ToLower(strings.TrimSpace(attrs["type"]))
	if ext == ".svg" || strings.Contains(typ, "svg") {
		return iconLink{}, false
	}
	side := declaredSide(attrs["sizes"])
	l := iconLink{href: u.String(), side: side, order: order}
	switch {
	case apple:
		l.class = 4
	case ext == ".ico" || strings.Contains(typ, "icon"):
		l.class = 3
	case side >= iconGoodSide:
		l.class = 0
	case side == 0:
		l.class = 1
	default:
		l.class = 2
	}
	return l, true
}

// declaredSide is the largest side in a sizes attribute such as "16x16 32x32",
// 0 for "any", a missing or an unreadable value.
func declaredSide(sizes string) int {
	best := 0
	for _, f := range strings.Fields(strings.ToLower(sizes)) {
		w, h, ok := strings.Cut(f, "x")
		if !ok {
			continue
		}
		wi, err1 := strconv.Atoi(w)
		hi, err2 := strconv.Atoi(h)
		if err1 != nil || err2 != nil {
			continue
		}
		best = max(best, wi, hi)
	}
	return best
}

// iconGet runs one GET under the per-request timeout and hands the response to
// use. A read cut short by that timeout surfaces as its error.
func iconGet(ctx context.Context, client *http.Client, rawURL, accept string, use func(*http.Response) error) error {
	ctx, cancel := context.WithTimeout(ctx, iconRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", accept)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := use(resp); err != nil {
		return err
	}
	return ctx.Err()
}

// downloadIcon fetches one icon address. A missing icon (4xx other than 429) or a
// bad body yields an error wrapping errBadIcon; anything else, including a 429 or
// 5xx answer, is a transient failure.
func downloadIcon(ctx context.Context, client *http.Client, rawURL string) (Icon, error) {
	var icon Icon
	err := iconGet(ctx, client, rawURL, "image/png,image/*;q=0.8,*/*;q=0.5", func(resp *http.Response) error {
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			return fmt.Errorf("icon status %d", resp.StatusCode)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return badIcon("status %d", resp.StatusCode)
		}
		data, err := io.ReadAll(httpclient.LimitBody(resp.Body, IconReadBytes))
		if errors.Is(err, httpclient.ErrTooLarge) {
			return badIcon("over %d bytes", IconReadBytes)
		}
		if err != nil {
			return err
		}
		icon, err = decodeIcon(data)
		return err
	})
	return icon, err
}

func badIcon(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errBadIcon, fmt.Sprintf(format, args...))
}

// decodeIcon sniffs the format from the bytes (a declared type is never
// trusted), checks the size before decoding and returns the image re-encoded
// as PNG. It accepts PNG, GIF, JPEG and ICO files holding PNG images.
func decodeIcon(data []byte) (Icon, error) {
	switch http.DetectContentType(data[:min(len(data), 512)]) {
	case "image/png", "image/gif", "image/jpeg":
		return decodeRaster(data)
	case "image/x-icon":
		payloads, err := icoPNGs(data)
		if err != nil {
			return Icon{}, err
		}
		var last error
		for _, p := range payloads {
			icon, err := decodeRaster(p)
			if err == nil {
				return icon, nil
			}
			last = err
		}
		return Icon{}, last
	}
	return Icon{}, badIcon("unsupported format")
}

// decodeRaster decodes a PNG, GIF or JPEG, refusing oversized or empty images
// before allocating pixels, and re-encodes it as PNG.
func decodeRaster(data []byte) (Icon, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return Icon{}, badIcon("decode: %v", err)
	}
	if err := checkSide(cfg.Width, cfg.Height); err != nil {
		return Icon{}, err
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return Icon{}, badIcon("decode: %v", err)
	}
	b := src.Bounds()
	if err := checkSide(b.Dx(), b.Dy()); err != nil {
		return Icon{}, err
	}
	img := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(img, img.Bounds(), src, b.Min, draw.Src)

	var out bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&out, img); err != nil {
		return Icon{}, badIcon("encode: %v", err)
	}
	if out.Len() > IconStoredBytes {
		return Icon{}, badIcon("over %d bytes once encoded", IconStoredBytes)
	}
	return Icon{PNG: out.Bytes(), Dark: isDark(img)}, nil
}

func checkSide(w, h int) error {
	if w <= 0 || h <= 0 {
		return badIcon("empty image")
	}
	if w > IconMaxSide || h > IconMaxSide {
		return badIcon("image %dx%d over %d", w, h, IconMaxSide)
	}
	return nil
}

// isDark reports a glyph that is mostly dark and so vanishes on a dark page: at
// least darkShare percent of its visible pixels under darkLuma, and at least a
// tenth of all pixels transparent, so an opaque black tile is not a glyph. A
// bright accent on a dark glyph does not change the verdict.
func isDark(img *image.NRGBA) bool {
	var visible, transparent, dark int
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		row := img.Pix[(y-b.Min.Y)*img.Stride:]
		for x := range b.Dx() {
			p := row[x*4 : x*4+4]
			if p[3] < 128 {
				transparent++
				continue
			}
			visible++
			if 0.2126*float64(p[0])+0.7152*float64(p[1])+0.0722*float64(p[2]) < darkLuma {
				dark++
			}
		}
	}
	if visible == 0 {
		return false
	}
	total := visible + transparent
	return dark*100 >= visible*darkShare && transparent*10 >= total
}

var pngSignature = []byte("\x89PNG\r\n\x1a\n")

// icoPNGs returns the PNG payloads of an ICO file, best first: the smallest
// side of at least iconGoodSide, else the largest. Entries holding anything but
// PNG (bitmaps) are skipped. Every offset and size is checked against the file.
func icoPNGs(data []byte) ([][]byte, error) {
	const headerLen, entryLen = 6, 16
	if len(data) < headerLen {
		return nil, badIcon("truncated ico")
	}
	if binary.LittleEndian.Uint16(data[0:]) != 0 || binary.LittleEndian.Uint16(data[2:]) != 1 {
		return nil, badIcon("not an ico")
	}
	count := min(int(binary.LittleEndian.Uint16(data[4:])), (len(data)-headerLen)/entryLen)

	type entry struct {
		side    int
		payload []byte
	}
	var entries []entry
	for i := range count {
		e := data[headerLen+i*entryLen : headerLen+(i+1)*entryLen]
		w, h := int(e[0]), int(e[1])
		if w == 0 {
			w = 256
		}
		if h == 0 {
			h = 256
		}
		size := uint64(binary.LittleEndian.Uint32(e[8:]))
		off := uint64(binary.LittleEndian.Uint32(e[12:]))
		if size < uint64(len(pngSignature)) || off+size > uint64(len(data)) {
			continue
		}
		payload := data[off : off+size]
		if !bytes.HasPrefix(payload, pngSignature) {
			continue
		}
		entries = append(entries, entry{side: max(w, h), payload: payload})
	}
	if len(entries) == 0 {
		return nil, badIcon("no png in ico")
	}
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i].side, entries[j].side
		switch {
		case a >= iconGoodSide && b >= iconGoodSide:
			return a < b
		case a >= iconGoodSide:
			return true
		case b >= iconGoodSide:
			return false
		}
		return a > b
	})
	out := make([][]byte, 0, min(len(entries), icoTries))
	for _, e := range entries[:min(len(entries), icoTries)] {
		out = append(out, e.payload)
	}
	return out, nil
}
