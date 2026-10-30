// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/DanielBlei/rabbithole/internal/store"
)

// iconCacheControl lets a browser keep an icon for a week without asking. The
// page URL carries the content tag (?v=), so a changed icon is a new URL.
const iconCacheControl = "private, max-age=604800, immutable"

// iconCSP locks the response down: it is our own re-encoded PNG, so nothing in
// it may run or load, even if a client renders it as a document.
const iconCSP = "default-src 'none'; sandbox"

// iconRef is what a page needs to draw an icon: a feed's stored one or one the
// library ships. The zero value means no icon: the page draws the letter tile.
type iconRef struct {
	ID   string // the feed, or the library source, the icon belongs to
	ETag string // content tag, without quotes
	Dark bool   // mostly dark glyph: drawn on a light plate
	Lib  bool   // the icon ships with the library instead of being fetched for a feed
}

// Src is the icon's URL. The id goes through path escaping and the tag through
// query escaping, so neither can leave its slot whatever they hold.
func (i iconRef) Src() string {
	prefix := "/icons/"
	if i.Lib {
		prefix = "/icons/library/"
	}
	return prefix + url.PathEscape(i.ID) + "?v=" + url.QueryEscape(i.ETag)
}

// validIconTag reports whether a stored content tag is short and plain enough
// to put in a URL.
func validIconTag(tag string) bool {
	if tag == "" || len(tag) > 128 {
		return false
	}
	for _, r := range tag {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// iconRefs reads which sources have an icon, once per page build. A feed's own
// fetched icon wins; a source the library ships an icon for gets that one
// otherwise, keyed by the same id (the id of a subscribed library source is its
// feed's id). Rows without bytes are left out, and a failed read yields only the
// library's icons, so the page still renders with letter tiles.
func (s *Web) iconRefs(ctx context.Context) map[string]iconRef {
	meta, err := s.db.FeedIconMeta(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("read feed icons")
	}
	baked := s.library.Snapshot().Icons
	out := make(map[string]iconRef, len(meta)+len(baked))
	for id, m := range meta {
		tag := strings.Trim(m.ETag, `"`)
		if m.Status != store.IconStatusOK || !validIconTag(tag) {
			continue
		}
		out[id] = iconRef{ID: id, ETag: tag, Dark: m.Dark}
	}
	for id, ic := range baked {
		if _, ok := out[id]; !ok && validIconTag(ic.ETag) {
			out[id] = iconRef{ID: id, ETag: ic.ETag, Dark: ic.Dark, Lib: true}
		}
	}
	return out
}

// favData is the input to the favicon partial: the stored icon when there is
// one, else the letter tile.
type favData struct {
	Icon iconRef
	Mono monogramData
	Size int
}

// ImgSize is the icon's edge: the tile less a 2px inset on each side.
func (f favData) ImgSize() int { return f.Size - 4 }

// HasIcon reports whether the tile shows the stored icon.
func (f favData) HasIcon() bool { return f.Icon.ETag != "" }

// fav builds the favicon partial's input for a source.
func fav(icon iconRef, name, rawURL string, size int) favData {
	return favData{Icon: icon, Mono: monogram(name, rawURL, size), Size: size}
}

// kickIcons asks the ingest manager to look up icons for feeds that lack one.
func (s *Web) kickIcons() {
	if s.ing == nil {
		return
	}
	s.ing.KickIcons()
}

// handleIcon serves a feed's stored icon. Only ok rows have bytes; anything
// else is a 404 that is not cached, so an icon fetched later shows up.
func (s *Web) handleIcon(w http.ResponseWriter, r *http.Request) {
	icon, err := s.db.FeedIconBytes(r.Context(), r.PathValue("feedID"))
	switch {
	case errors.Is(err, store.ErrFeedIconNotFound):
		iconNotFound(w, r)
	case err != nil:
		log.Error().Err(err).Msg("read feed icon")
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "could not read icon", http.StatusInternalServerError)
	default:
		serveIcon(w, r, icon.Bytes, icon.ETag)
	}
}

// handleLibraryIcon serves an icon that ships with the library, from the loaded
// snapshot: no site is contacted. It waits for the first load only while
// nothing has loaded, so a page that names an icon right after a restart still
// gets it.
func (s *Web) handleLibraryIcon(w http.ResponseWriter, r *http.Request) {
	ic, ok := s.library.Get(r.Context()).Icons[r.PathValue("id")]
	if !ok {
		iconNotFound(w, r)
		return
	}
	serveIcon(w, r, ic.PNG, ic.ETag)
}

// iconNotFound answers 404 without letting a browser keep it, so an icon that
// appears later shows up.
func iconNotFound(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	http.NotFound(w, r)
}

// serveIcon answers with an icon's PNG bytes; a matching If-None-Match gets a 304.
func serveIcon(w http.ResponseWriter, r *http.Request, png []byte, etag string) {
	h := w.Header()
	h.Set("Content-Type", "image/png")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", iconCSP)
	h.Set("Cache-Control", iconCacheControl)
	if tag := strings.Trim(etag, `"`); validIconTag(tag) {
		h.Set("ETag", `"`+tag+`"`)
	}
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(png))
}
