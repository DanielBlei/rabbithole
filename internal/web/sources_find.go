// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// The find controls post one address, so a larger body is not from the page.
const findFormLimit = 8 << 10

const (
	// maxFindName and maxFindTitle cap text a remote feed supplies before it is
	// put in a field or a list.
	maxFindName  = 120
	maxFindTitle = 160

	findEmptyText = "Paste a site or feed address first."
)

// Candidate states, as the rows carry them in data-state.
const (
	candNew        = "new"
	candSubscribed = "subscribed"
	candRemoved    = "removed"
	candConflict   = "conflict"
	candBlocked    = "blocked"
)

// findResult is the Add form's answer to Find feeds: the candidates, or why
// there are none. Manual offers the details link beside a no-feed message.
type findResult struct {
	Err    string
	Manual bool
	Head   string
	Cands  []candView
}

// candView is one feed found. Everything but State, ID and Kind comes from the
// remote site, so the template prints it as text and in data attributes only.
type candView struct {
	Index  int
	Name   string
	URL    string
	Kind   string
	Posts  string
	State  string
	ID     string
	Note   string
	Latest previewView
}

// Disabled is whether the row can be picked. A subscribed or removed feed is
// dealt with from its note; a blocked one cannot be added at all.
func (c candView) Disabled() bool {
	return c.State == candSubscribed || c.State == candRemoved || c.State == candBlocked
}

// previewView is the three newest posts of a feed, or the reason there are none.
type previewView struct {
	Err   string
	Items []previewItem
}

type previewItem struct {
	Title string
	Ago   string
}

// dupView is the note under the Address field for a feed already stored.
type dupView struct {
	Live bool
	ID   string
	Name string
}

var kindLabels = map[feeds.Kind]string{feeds.KindRSS: "RSS", feeds.KindAtom: "Atom", feeds.KindJSON: "JSON"}

// readFindForm reads the address a find control posted. A body that is too
// large or unreadable is the caller's fault and ends the request.
func readFindForm(w http.ResponseWriter, r *http.Request) (raw string, ok bool) {
	r.Body = http.MaxBytesReader(w, r.Body, findFormLimit)
	if err := r.ParseForm(); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "form too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "could not read the form", http.StatusBadRequest)
		}
		return "", false
	}
	return strings.TrimSpace(r.PostFormValue("url")), true
}

// findTarget turns what was typed into an address to fetch, or says why it
// cannot be one.
func findTarget(raw string) (target, msg string) {
	if raw == "" {
		return "", findEmptyText
	}
	target = config.NormalizeFeedURL(raw)
	if config.ValidateFeedURL(target) != nil {
		if u, err := url.Parse(target); err == nil && u.Scheme != "http" && u.Scheme != "https" {
			return "", schemeText
		}
		return "", findBadAddrText
	}
	return target, ""
}

// handleSourceDiscover finds the feeds behind a site or feed address and
// answers with the candidate list. Every outcome the visitor can act on is a
// 200 fragment, so it lands where the results go.
func (s *Web) handleSourceDiscover(w http.ResponseWriter, r *http.Request) {
	raw, ok := readFindForm(w, r)
	if !ok {
		return
	}
	target, msg := findTarget(raw)
	if msg != "" {
		s.renderFragment(w, "sourceCandidates", findResult{Err: msg})
		return
	}
	cands, err := feeds.Discover(r.Context(), s.feedHTTP(), target)
	if err != nil {
		log.Debug().Err(err).Msg("find feeds failed")
		s.renderFragment(w, "sourceCandidates", findResult{
			Err: explainFindError(err), Manual: errors.Is(err, feeds.ErrNoFeed),
		})
		return
	}
	views, err := s.candidateViews(r, cands)
	if err != nil {
		log.Error().Err(err).Msg("classify feeds found")
		s.renderFragment(w, "sourceCandidates", findResult{Err: findCheckText})
		return
	}
	if len(views) == 0 {
		s.renderFragment(w, "sourceCandidates", findResult{Err: findNoFeedText, Manual: true})
		return
	}
	s.renderFragment(w, "sourceCandidates", findResult{Head: foundHead(views), Cands: views})
}

// candidateViews classifies each feed found against the stored sources, one at
// a time so two candidates with the same title do not read as a clash with each
// other. A candidate the store would refuse outright is left out.
func (s *Web) candidateViews(r *http.Request, cands []feeds.Candidate) ([]candView, error) {
	now := s.clock()
	out := make([]candView, 0, len(cands))
	for _, c := range cands {
		name := candidateName(c)
		rows, err := s.db.PlanImport(r.Context(), config.FeedsDoc{Feeds: []config.Feed{{Name: name, URL: c.URL}}})
		if err != nil {
			return nil, err
		}
		if len(rows) != 1 {
			continue
		}
		row := rows[0]
		v := candView{
			Index: len(out),
			Name:  name,
			URL:   c.URL,
			Kind:  kindLabels[c.Kind],
			Posts: postsPhrase(c.ItemCount),
			ID:    row.ID,
			Latest: previewView{
				Items: previewItems(c.Latest, now),
			},
		}
		switch {
		case row.Status == store.PlanNew:
			v.State = candNew
		case row.Status == store.PlanSubscribed:
			v.State = candSubscribed
		case row.Status == store.PlanRemoved:
			v.State = candRemoved
		case row.IDTaken:
			v.State, v.Note = candBlocked, row.Reason
		case row.Status == store.PlanConflict:
			v.State = candConflict
		default:
			continue
		}
		out = append(out, v)
	}
	return out, nil
}

// candidateName is what the source is called when picked: the feed's title,
// else its host.
func candidateName(c feeds.Candidate) string {
	name := strings.TrimSpace(c.Title)
	if name == "" {
		if u, err := url.Parse(c.URL); err == nil {
			name = u.Hostname()
		}
	}
	return clipRunes(name, maxFindName)
}

// foundHead is the line above the list.
func foundHead(views []candView) string {
	if len(views) > 1 {
		return fmt.Sprintf("Found %d feeds. Choose one.", len(views))
	}
	if views[0].State == candNew || views[0].State == candConflict {
		return "Found a feed."
	}
	return "Found this feed."
}

func postsPhrase(n int) string {
	switch n {
	case 0:
		return "No posts yet"
	case 1:
		return "1 post"
	default:
		return fmt.Sprintf("%d posts", n)
	}
}

func previewItems(entries []feeds.Entry, now time.Time) []previewItem {
	out := make([]previewItem, 0, len(entries))
	for _, e := range entries {
		title := clipRunes(strings.TrimSpace(e.Title), maxFindTitle)
		if title == "" {
			title = "(untitled)"
		}
		item := previewItem{Title: title}
		if !e.Published.IsZero() {
			item.Ago = relTime(e.Published, now)
		}
		out = append(out, item)
	}
	return out
}

// clipRunes cuts s to n characters, marking the cut.
func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// handleSourcePreview reads the feed at the Address field and answers with its
// newest posts. Nothing is stored.
func (s *Web) handleSourcePreview(w http.ResponseWriter, r *http.Request) {
	raw, ok := readFindForm(w, r)
	if !ok {
		return
	}
	target, msg := findTarget(raw)
	if msg != "" {
		s.renderFragment(w, "sourcePreview", previewView{Err: msg})
		return
	}
	res, err := feeds.Preview(r.Context(), s.feedHTTP(), target)
	if err != nil {
		log.Debug().Err(err).Msg("feed preview failed")
		s.renderFragment(w, "sourcePreview", previewView{Err: explainFindError(err)})
		return
	}
	s.renderFragment(w, "sourcePreview", previewView{Items: previewItems(res.Items, s.clock())})
}

// handleSourceCheck says whether the address typed into the Add form is already
// a source, live or removed. It reads the store only. No match answers an empty
// 200, so the note clears.
func (s *Web) handleSourceCheck(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(r.URL.Query().Get("url"))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if raw == "" || len(raw) > findFormLimit {
		return
	}
	match, err := s.db.LookupFeedURL(r.Context(), raw)
	if err != nil {
		httpFeedError(w, err)
		return
	}
	if match.State == store.FeedMatchNone {
		return
	}
	if err := feedTmpl.ExecuteTemplate(w, "sourceDup", dupView{
		Live: match.State == store.FeedMatchLive, ID: match.ID, Name: match.Name,
	}); err != nil {
		log.Error().Err(err).Msg("render source dup note")
	}
}
