// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"gopkg.in/yaml.v3"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/ingest"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// The three states of an inheritable boolean in the detail form. A feed that
// sets nothing inherits, which is a different thing from setting it to off —
// so the control is a three-way choice, not a checkbox.
const (
	inheritChoice = ""
	onChoice      = "on"
	offChoice     = "off"
)

// sourcesData is the Sources dialog model: the whole feed set as rows on the
// left, one feed's editable settings on the right.
type sourcesData struct {
	PromptUser string

	Counts   sourceCounts
	Defaults string // one-line summary of the cascade
	Error    string // fetch history unavailable — a note, not a failure

	Rows   []feedRowData
	Detail *feedDetail
	// AllTags is every tag in use across the set, comma-joined — what the tag
	// input suggests from.
	AllTags string

	// DefaultSince and DefaultCap are what a blank tuning field falls back to.
	// They are computed once per render rather than per feed because the answer
	// is the same for every feed: the blank replaces the feed's own value, so
	// what's left is the defaults → ingest.since → built-in chain. That also
	// gives the add form, which has no resolved feed yet, correct placeholders.
	DefaultSince string
	DefaultCap   string
}

// sourceCounts is the dialog's tally, rendered as the arguments of the faux
// shell command in its title bar — the same place the Maze puts its own counts.
// It re-renders out of band on every mutation, so it stays true without the
// header being part of the swap that would wipe the search box.
type sourceCounts struct {
	OOB     bool
	Total   int
	Enabled int
	Failing int
	Deleted int
}

// Cmd renders the tally as the command line: `sources --feeds 6 --enabled 5`.
// Feeds and enabled are always shown; the other two only when they have
// something to report, so a healthy set isn't padded with zeroes.
func (c sourceCounts) Cmd() string {
	cmd := fmt.Sprintf("sources --feeds %d --enabled %d", c.Total, c.Enabled)
	if c.Failing > 0 {
		cmd += fmt.Sprintf(" --failing %d", c.Failing)
	}
	if c.Deleted > 0 {
		cmd += fmt.Sprintf(" --deleted %d", c.Deleted)
	}
	return cmd
}

// feedDetail is the right-hand pane: one feed's settings as typed. A tuning
// field left blank takes the default, and the field's placeholder shows what
// that default is — so the form posts back an empty string and the page never
// has to explain the word "inherited".
type feedDetail struct {
	// Adding switches the pane to the new-feed form: same fields, no health,
	// and a create rather than a save.
	Adding bool

	ID      string
	Name    string
	URL     string
	Deleted bool

	// Own values, blank when the feed takes the default.
	EnabledChoice string
	Since         string
	MaxItems      string
	Tags          []string
	TagsValue     string // comma-joined, the hidden field the tag input drives

	// Defaults a blank field falls back to, rendered as the placeholder. Not
	// per-feed: see sourcesData.
	DefaultSince string
	DefaultCap   string

	// Eff carries the feed's health for the strip below the form.
	Eff feedRowData
	// InheritedTags come from the defaults and are unioned onto the feed's own.
	// Only a seed file can set them now, so they show as fixed chips that
	// explain a tag the feed doesn't list itself.
	InheritedTags []string

	Err    string // validation failure, rendered against the form
	Notice string // something that worked but is worth saying, e.g. items re-filed
	// Flash is the confirmation laid over the pane after a change. It clears
	// itself; Notice stays put, which is what a caveat like a half-done rename
	// needs. A plain "saved" only has to be seen once.
	Flash *flashData
}

// flashData is one self-clearing confirmation: the outcome in a word, over the
// command that produced it. Written in the shell register the drawer and the
// pane bars already speak, so a change reads as something that ran.
type flashData struct {
	Verb string // the outcome: added | saved | deleted
	Cmd  string // the command it stands in for, e.g. "feed rm"
	Name string // the feed it happened to
	Bad  bool   // a removal — red, and a cross rather than a tick
}

// defaultsData backs the defaults editor modal. No tags field: a tag applied to
// every feed can't tell any two apart, so offering one only invites a setting
// that makes the feed page's tag filter useless. Seed files may still set
// defaults.tags, and handleSourceSaveDefaults preserves it.
type defaultsData struct {
	EnabledChoice string
	Since         string
	MaxItems      string
	GlobalSince   string // ingest.since, the fallback when Since is blank
	Err           string
}

// exportData backs the export modal: the live set rendered as a seed file.
type exportData struct {
	YAML      string
	Path      string
	FeedCount int
	Err       string
}

// handleSources opens the whole thing as a dialog. It answers with a bare
// fragment, not a page: every route below already spoke in fragments, and this
// was the last one that didn't.
func (s *Web) handleSources(w http.ResponseWriter, r *http.Request) {
	data, err := s.sourcesData(r.Context(), r.URL.Query().Get("feed"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderFragment(w, "sourcesModal", data)
}

// handleSourceSelect opens one feed in the detail pane.
func (s *Web) handleSourceSelect(w http.ResponseWriter, r *http.Request) {
	s.renderSourcesBody(w, r, r.PathValue("id"), nil)
}

// handleSourceNew opens the add-a-feed form in the detail pane.
func (s *Web) handleSourceNew(w http.ResponseWriter, r *http.Request) {
	s.renderSourcesBody(w, r, "", &feedDetail{Adding: true, EnabledChoice: inheritChoice})
}

// handleSourceAdd creates a feed. A failure re-renders the form with the
// message and everything the user typed still in it.
//
// A success leaves the add form open and empty rather than opening the feed it
// just made: feeds arrive in batches, and landing in the editor for one you have
// nothing more to say about means clicking "add feed" again for every single
// one. The new feed is in the list beside it either way, and the notice names
// what was added.
func (s *Web) handleSourceAdd(w http.ResponseWriter, r *http.Request) {
	feed, err := feedFromForm(r)
	if err != nil {
		s.renderSourcesBody(w, r, "", addFormWithError(r, err))
		return
	}
	if _, err := s.db.AddFeed(r.Context(), feed); err != nil {
		if isFeedInputError(err) {
			s.renderSourcesBody(w, r, "", addFormWithError(r, err))
			return
		}
		httpFeedError(w, err)
		return
	}
	s.afterFeedChange(r.Context())
	s.renderSourcesBody(w, r, "", &feedDetail{
		Adding:        true,
		EnabledChoice: inheritChoice,
		Flash:         &flashData{Verb: "added", Cmd: "feed add", Name: feed.Name},
	})
}

// handleSourceSave writes the detail form back.
//
// A rename also re-files the feed's existing items: items.source records the
// feed's name, so without that they would keep pointing at a label no feed
// claims any more. It is reported rather than enforced — the feed is saved
// either way.
func (s *Web) handleSourceSave(w http.ResponseWriter, r *http.Request) {
	ctx, id := r.Context(), r.PathValue("id")
	previous, err := s.db.FeedByID(ctx, id)
	if err != nil {
		httpFeedError(w, err)
		return
	}
	feed, err := feedFromForm(r)
	if err != nil {
		s.renderSourcesBody(w, r, id, editFormWithError(r, id, err))
		return
	}
	if err := s.db.UpdateFeed(ctx, id, feed); err != nil {
		if isFeedInputError(err) {
			s.renderSourcesBody(w, r, id, editFormWithError(r, id, err))
			return
		}
		httpFeedError(w, err)
		return
	}

	var notice string
	if moved, err := s.db.RenameSource(ctx, previous.Name, feed.Name); err != nil {
		log.Warn().Err(err).Str("feed", feed.Name).Msg("re-filing items after a feed rename failed")
		notice = "renamed, but existing items are still filed under " + previous.Name
	} else if moved > 0 {
		notice = fmt.Sprintf("renamed · re-filed %s", itemsPhrase(int(moved)))
	}
	s.afterFeedChange(ctx)
	s.renderSourcesBody(w, r, id, &feedDetail{
		Notice: notice,
		Flash:  &flashData{Verb: "saved", Cmd: "feed save", Name: feed.Name},
	})
}

// handleSourceEnabled parks or unparks a feed from its row in the list.
func (s *Web) handleSourceEnabled(w http.ResponseWriter, r *http.Request) {
	on := r.FormValue("enabled") == onChoice
	if err := s.db.SetFeedEnabled(r.Context(), r.PathValue("id"), &on); err != nil {
		httpFeedError(w, err)
		return
	}
	s.afterFeedChange(r.Context())
	s.renderSourcesBody(w, r, r.PathValue("id"), nil)
}

// handleSourceConfirmDelete asks before removing a feed. A dialog rather than a
// second button beside the first: the button appeared where the pointer already
// was, which is the one place a confirmation should never be.
func (s *Web) handleSourceConfirmDelete(w http.ResponseWriter, r *http.Request) {
	feed, err := s.db.FeedByID(r.Context(), r.PathValue("id"))
	if err != nil {
		httpFeedError(w, err)
		return
	}
	s.renderFragment(w, "sourceConfirmDelete", feed)
}

// handleSourceDelete parks a feed out of sight. The row survives, so its fetch
// history stays attached and re-seeding won't bring it back.
func (s *Web) handleSourceDelete(w http.ResponseWriter, r *http.Request) {
	ctx, id := r.Context(), r.PathValue("id")
	// Read it first: the confirmation names what went, and after the delete the
	// resolved set no longer carries it.
	feed, err := s.db.FeedByID(ctx, id)
	if err != nil {
		httpFeedError(w, err)
		return
	}
	if err := s.db.SoftDeleteFeed(ctx, id); err != nil {
		httpFeedError(w, err)
		return
	}
	s.afterFeedChange(ctx)

	// Closes the confirm dialog, then repaints the columns underneath it.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if _, err := w.Write(
		[]byte(`<span id="sourcesDeleteDone" hx-swap-oob="innerHTML:#modalTop"></span>`),
	); err != nil {
		log.Error().Err(err).Msg("write delete response")
		return
	}
	s.writeSourcesBody(w, r, "", &feedDetail{
		Flash: &flashData{Verb: "deleted", Cmd: "feed rm", Name: feed.Name, Bad: true},
	})
}

func (s *Web) handleSourceRestore(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.db.RestoreFeed(r.Context(), id); err != nil {
		httpFeedError(w, err)
		return
	}
	s.afterFeedChange(r.Context())
	s.renderSourcesBody(w, r, id, nil)
}

func (s *Web) handleSourceDefaults(w http.ResponseWriter, r *http.Request) {
	d, err := s.db.FeedDefaults(r.Context())
	if err != nil {
		httpFeedError(w, err)
		return
	}
	s.renderFragment(w, "sourceDefaults", s.defaultsForm(d, ""))
}

// handleSourceSaveDefaults writes the set-wide fallbacks. Every feed that
// inherits moves at once, so this repaints the whole list rather than one row.
func (s *Web) handleSourceSaveDefaults(w http.ResponseWriter, r *http.Request) {
	defaults, err := defaultsFromForm(r)
	if err == nil {
		// The form has no tags field — a tag on every feed can't filter
		// anything, so it isn't offered. A seed file can still set one, and a
		// save must carry it through rather than posting an absent field as an
		// instruction to clear it.
		if stored, storedErr := s.db.FeedDefaults(r.Context()); storedErr == nil {
			defaults.Tags = stored.Tags
		} else {
			log.Warn().Err(storedErr).Msg("reading feed defaults before a save failed")
		}
		err = s.db.SetFeedDefaults(r.Context(), defaults)
	}
	if err != nil {
		form := s.defaultsForm(defaults, err.Error())
		form.Since, form.MaxItems = r.FormValue("since"), r.FormValue("max_items")
		s.renderFragment(w, "sourceDefaults", form)
		return
	}
	s.afterFeedChange(r.Context())
	// Closes the modal (an empty layer) and repaints the columns underneath.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if _, err := w.Write(
		[]byte(`<span id="sourcesDefaultsDone" hx-swap-oob="innerHTML:#modalTop"></span>`),
	); err != nil {
		log.Error().Err(err).Msg("write defaults response")
		return
	}
	s.writeSourcesBody(w, r, "", nil)
}

// handleSourceExport renders the live set as a seed file: the same YAML shape
// feeds.yaml uses, so it can be copied straight back out to one.
func (s *Web) handleSourceExport(w http.ResponseWriter, r *http.Request) {
	data := exportData{Path: s.seedPath()}
	doc, err := s.db.FeedsDoc(r.Context())
	if err != nil {
		data.Err = err.Error()
		s.renderFragment(w, "sourceExport", data)
		return
	}
	raw, err := marshalFeedsDoc(doc)
	if err != nil {
		data.Err = err.Error()
		s.renderFragment(w, "sourceExport", data)
		return
	}
	data.YAML, data.FeedCount = raw, len(doc.Feeds)
	s.renderFragment(w, "sourceExport", data)
}

// marshalFeedsDoc renders a feed set as a seed file. yaml.Marshal defaults to
// four-space indentation, which the shipped example does not use, so this goes
// through an encoder to match what a hand-written feeds.yaml looks like.
func marshalFeedsDoc(doc config.FeedsDoc) (string, error) {
	var out strings.Builder
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return "", err
	}
	if err := enc.Close(); err != nil {
		return "", err
	}
	return out.String(), nil
}

// sourcesData assembles the dialog: the resolved feed set joined with each feed's
// fetch history, plus whichever feed the detail pane is showing.
func (s *Web) sourcesData(ctx context.Context, selected string) (sourcesData, error) {
	doc, err := s.db.FeedsDoc(ctx)
	if err != nil {
		return sourcesData{}, err
	}
	resolved, err := config.ResolveFeeds(doc, s.cfg.Ingest.Since.Std())
	if err != nil {
		return sourcesData{}, err
	}
	deleted, err := s.db.DeletedFeeds(ctx)
	if err != nil {
		return sourcesData{}, err
	}

	defaultSince, defaultCap := blankFallbacks(doc.Defaults, s.cfg.Ingest.Since.Std())
	data := sourcesData{
		PromptUser:   s.user,
		Defaults:     defaultsSummary(doc.Defaults, s.cfg.Ingest.Since.Std()),
		DefaultSince: defaultSince,
		DefaultCap:   defaultCap,
		Counts:       sourceCounts{Total: len(resolved), Deleted: len(deleted)},
	}

	// History is a join, not a requirement: losing it still leaves a dialog you
	// can edit, so it degrades to a note rather than an error.
	health, err := s.db.FeedHealthByID(ctx, feedStripLen)
	if err != nil {
		log.Error().Err(err).Msg("read feed health")
		data.Error = "fetch history unavailable: " + err.Error()
	}

	now := time.Now()
	data.Rows = make([]feedRowData, 0, len(resolved)+len(deleted))
	for _, f := range resolved {
		row := toFeedRow(f, health[f.ID], now)
		if f.Enabled {
			data.Counts.Enabled++
		}
		if row.State == feedStateError {
			data.Counts.Failing++
		}
		data.Rows = append(data.Rows, row)
	}
	if len(deleted) > 0 {
		// Deleted feeds aren't in doc, so they go through the cascade here to
		// render with the same effective values as the rest.
		gone, err := config.ResolveFeeds(
			config.FeedsDoc{Defaults: doc.Defaults, Feeds: deleted},
			s.cfg.Ingest.Since.Std(),
		)
		if err != nil {
			return sourcesData{}, err
		}
		for _, f := range gone {
			row := toFeedRow(f, health[f.ID], now)
			row.Deleted, row.State, row.Detail = true, feedStateOff, "deleted"
			data.Rows = append(data.Rows, row)
		}
	}

	data.AllTags = strings.Join(tagUniverse(resolved), ",")
	data.Detail = s.detailFor(ctx, selected, doc.Defaults, data.Rows)
	stampDefaults(&data)
	return data, nil
}

// stampDefaults copies the dialog's blank-field fallbacks onto the detail pane,
// wherever that pane came from — the store, a fresh add form, or a rejected
// submission being re-rendered. The template reads them as placeholders, and it
// only has the pane as its dot, not the dialog.
func stampDefaults(data *sourcesData) {
	if data.Detail != nil {
		data.Detail.DefaultSince = data.DefaultSince
		data.Detail.DefaultCap = data.DefaultCap
	}
}

// blankFallbacks resolves what a feed gets when it sets neither knob — the
// values the detail form shows as greyed placeholders. It walks the same chain
// resolveFeed does, minus the feed itself, which is exactly what a blank field
// means: defaults, then the global ingest.since, then the built-in.
//
// The cap reads as "uncapped" rather than capLabel's em dash: in a placeholder
// a dash says "no value", where what's meant is "no limit".
func blankFallbacks(d config.FeedDefaults, globalSince time.Duration) (since, cap string) {
	since = shortDur(globalSince)
	if d.Since != nil {
		since = d.Since.Short()
	}
	cap = "uncapped"
	if d.MaxItems != nil && *d.MaxItems > 0 {
		cap = strconv.Itoa(*d.MaxItems)
	}
	return since, cap
}

// tagUniverse is every tag any feed carries, sorted — what the tag input
// suggests from, so tagging a second feed "ai" is one keystroke and a click
// rather than a retype.
func tagUniverse(feeds []config.ResolvedFeed) []string {
	seen := make(map[string]bool)
	var out []string
	for _, f := range feeds {
		for _, tag := range f.Tags {
			if !seen[tag] {
				seen[tag] = true
				out = append(out, tag)
			}
		}
	}
	sort.Strings(out)
	return out
}

// detailFor builds the pane for the selected feed, or nil when nothing is
// selected and the pane shows its placeholder. The lookup goes to the store
// rather than the resolved list so a deleted feed can still be opened — that is
// the only route back from an accidental delete.
func (s *Web) detailFor(ctx context.Context, id string, defaults config.FeedDefaults, rows []feedRowData) *feedDetail {
	if id == "" {
		return nil
	}
	own, err := s.db.FeedByID(ctx, id)
	if err != nil {
		if !errors.Is(err, store.ErrFeedNotFound) {
			log.Error().Err(err).Str("feed", id).Msg("read feed for the detail pane")
		}
		return nil
	}

	detail := &feedDetail{
		ID:            own.ID,
		Name:          own.Name,
		URL:           own.URL,
		EnabledChoice: enabledChoice(own.Enabled),
		Tags:          own.Tags,
		TagsValue:     strings.Join(own.Tags, ","),
		InheritedTags: inheritedOnly(defaults.Tags, own.Tags),
	}
	if own.Since != nil {
		detail.Since = own.Since.Short()
	}
	if own.MaxItems != nil {
		detail.MaxItems = strconv.Itoa(*own.MaxItems)
	}
	for _, row := range rows {
		if row.ID == id {
			detail.Eff, detail.Deleted = row, row.Deleted
			break
		}
	}
	return detail
}

// renderSourcesBody re-renders the columns and, out of band, the header counts.
// Every mutation lands here: the list is small enough that repainting it beats
// tracking which rows a change touched, and keeping the search box outside the
// swap means a filtered view survives an edit.
func (s *Web) renderSourcesBody(w http.ResponseWriter, r *http.Request, selected string, detail *feedDetail) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	s.writeSourcesBody(w, r, selected, detail)
}

func (s *Web) writeSourcesBody(w http.ResponseWriter, r *http.Request, selected string, detail *feedDetail) {
	data, err := s.sourcesData(r.Context(), selected)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// An explicit detail (a form with an error, or a notice) wins over the one
	// loaded from the store, so what the user typed isn't thrown away.
	if detail != nil {
		data.Detail = mergeDetail(data.Detail, detail)
		stampDefaults(&data)
	}
	data.Counts.OOB = true
	if err := feedTmpl.ExecuteTemplate(w, "sourcesBody", data); err != nil {
		log.Error().Err(err).Msg("render sources body")
	}
}

// renderFragment writes one of the sources partials. They live in
// templates/partials, so any page set carries them — feedTmpl will do, the same
// call the config and ingest modals make.
func (s *Web) renderFragment(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := feedTmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Error().Err(err).Str("fragment", name).Msg("render sources fragment")
	}
}

// afterFeedChange pushes a feed edit out to the items already recorded: their
// tags come from the feed, and an item already scored is never re-inserted, so
// without this a retag would only ever reach items ingested afterwards.
func (s *Web) afterFeedChange(ctx context.Context) {
	configured, err := ingest.ResolveFeeds(ctx, s.db, s.cfg)
	if err != nil {
		log.Warn().Err(err).Msg("resolving feeds after an edit failed")
		return
	}
	if err := s.db.SyncSourceTags(ctx, ingest.ConfiguredTags(configured)); err != nil {
		log.Warn().Err(err).Msg("syncing feed tags failed")
	}
}

func (s *Web) seedPath() string {
	path, _ := s.cfg.FeedsFilePath(s.cfgPath)
	return path
}

func (s *Web) defaultsForm(d config.FeedDefaults, errMsg string) defaultsData {
	form := defaultsData{
		EnabledChoice: enabledChoice(d.Enabled),
		GlobalSince:   shortDur(s.cfg.Ingest.Since.Std()),
		Err:           errMsg,
	}
	if d.Since != nil {
		form.Since = d.Since.Short()
	}
	if d.MaxItems != nil {
		form.MaxItems = strconv.Itoa(*d.MaxItems)
	}
	return form
}

// feedFromForm reads the detail form. A blank field means "inherit", which is a
// nil pointer here and a NULL in the store — not a zero value.
func feedFromForm(r *http.Request) (config.Feed, error) {
	feed := config.Feed{
		Name: strings.TrimSpace(r.FormValue("name")),
		URL:  strings.TrimSpace(r.FormValue("url")),
		Tags: splitCommaList(r.FormValue("tags")),
	}
	switch r.FormValue("enabled") {
	case onChoice:
		on := true
		feed.Enabled = &on
	case offChoice:
		off := false
		feed.Enabled = &off
	}
	if raw := strings.TrimSpace(r.FormValue("since")); raw != "" {
		parsed, err := config.ParseDuration(raw)
		if err != nil {
			return feed, err
		}
		since := config.Duration(parsed)
		feed.Since = &since
	}
	if raw := strings.TrimSpace(r.FormValue("max_items")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return feed, fmt.Errorf("max_items must be a whole number, got %q", raw)
		}
		feed.MaxItems = &n
	}
	return feed, nil
}

func defaultsFromForm(r *http.Request) (config.FeedDefaults, error) {
	feed, err := feedFromForm(r)
	if err != nil {
		return config.FeedDefaults{}, err
	}
	return config.FeedDefaults{
		Enabled:  feed.Enabled,
		Since:    feed.Since,
		MaxItems: feed.MaxItems,
		Tags:     feed.Tags,
	}, nil
}

// addFormWithError rebuilds the add form from what was submitted, so a rejected
// feed comes back with the fields still filled in.
func addFormWithError(r *http.Request, err error) *feedDetail {
	return &feedDetail{
		Adding:        true,
		Name:          r.FormValue("name"),
		URL:           r.FormValue("url"),
		EnabledChoice: r.FormValue("enabled"),
		Since:         r.FormValue("since"),
		MaxItems:      r.FormValue("max_items"),
		TagsValue:     r.FormValue("tags"),
		Tags:          splitCommaList(r.FormValue("tags")),
		Err:           err.Error(),
	}
}

func editFormWithError(r *http.Request, id string, err error) *feedDetail {
	form := addFormWithError(r, err)
	form.Adding, form.ID = false, id
	return form
}

// mergeDetail lays the handler's overrides over the pane loaded from the store.
// A notice on its own leaves the stored values alone; a form carrying an error
// replaces them, because those are the values the user is still editing.
func mergeDetail(stored, override *feedDetail) *feedDetail {
	if override.Err == "" && !override.Adding {
		if stored == nil {
			// Nothing to lay it over — a delete's confirmation is the whole
			// pane, above the placeholder that has taken the form's place.
			return override
		}
		stored.Notice, stored.Flash = override.Notice, override.Flash
		return stored
	}
	if stored != nil {
		override.Eff, override.InheritedTags, override.Deleted = stored.Eff, stored.InheritedTags, stored.Deleted
	}
	return override
}

func enabledChoice(v *bool) string {
	switch {
	case v == nil:
		return inheritChoice
	case *v:
		return onChoice
	default:
		return offChoice
	}
}

// inheritedOnly returns the default tags a feed doesn't already carry — the
// ones shown as fixed chips it can't remove from here.
func inheritedOnly(defaults, own []string) []string {
	if len(defaults) == 0 {
		return nil
	}
	has := make(map[string]bool, len(own))
	for _, t := range own {
		has[t] = true
	}
	var out []string
	for _, t := range defaults {
		if !has[t] {
			out = append(out, t)
		}
	}
	return out
}

func splitCommaList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// isFeedInputError reports whether an error is the user's to fix, and so
// belongs inline against the form rather than as an HTTP error.
func isFeedInputError(err error) bool {
	return errors.Is(err, store.ErrFeedInvalid) ||
		errors.Is(err, store.ErrFeedNameTaken) ||
		errors.Is(err, store.ErrFeedURLTaken)
}

// httpFeedError maps a store failure onto a status: a missing feed is a 404,
// anything else is ours.
func httpFeedError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrFeedNotFound) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	log.Error().Err(err).Msg("feed store operation failed")
	http.Error(w, err.Error(), http.StatusInternalServerError)
}
