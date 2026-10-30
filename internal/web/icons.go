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

// iconRef is what a page needs to draw a feed's stored icon. The zero value
// means no icon: the page draws the letter tile.
type iconRef struct {
	ID   string // the feed the icon belongs to
	ETag string // content tag, without quotes
	Dark bool   // mostly dark glyph: drawn on a light plate
}

// Src is the icon's URL. The id goes through path escaping and the tag through
// query escaping, so neither can leave its slot whatever they hold.
func (i iconRef) Src() string {
	return "/icons/" + url.PathEscape(i.ID) + "?v=" + url.QueryEscape(i.ETag)
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

// iconRefs reads which feeds have a stored icon, once per page build. Rows
// without bytes are left out, and a failed read yields no icons, so the page
// still renders with letter tiles.
func (s *Web) iconRefs(ctx context.Context) map[string]iconRef {
	meta, err := s.db.FeedIconMeta(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("read feed icons")
		return nil
	}
	out := make(map[string]iconRef, len(meta))
	for id, m := range meta {
		tag := strings.Trim(m.ETag, `"`)
		if m.Status != store.IconStatusOK || !validIconTag(tag) {
			continue
		}
		out[id] = iconRef{ID: id, ETag: tag, Dark: m.Dark}
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
	h := w.Header()
	switch {
	case errors.Is(err, store.ErrFeedIconNotFound):
		h.Set("Cache-Control", "no-store")
		http.NotFound(w, r)
		return
	case err != nil:
		log.Error().Err(err).Msg("read feed icon")
		h.Set("Cache-Control", "no-store")
		http.Error(w, "could not read icon", http.StatusInternalServerError)
		return
	}
	h.Set("Content-Type", "image/png")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", iconCSP)
	h.Set("Cache-Control", iconCacheControl)
	if tag := strings.Trim(icon.ETag, `"`); validIconTag(tag) {
		h.Set("ETag", `"`+tag+`"`)
	}
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(icon.Bytes))
}
