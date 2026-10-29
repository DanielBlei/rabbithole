// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/rs/zerolog/log"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/httplog"
	"github.com/DanielBlei/rabbithole/internal/ingest"
	"github.com/DanielBlei/rabbithole/internal/library"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// The three states of an inheritable boolean in the editor. A feed that sets
// nothing inherits, which is a different thing from setting it to off, so the
// control is a three-way choice, not a checkbox.
const (
	inheritChoice = ""
	onChoice      = "on"
	offChoice     = "off"
)

// sourceTypes are the kinds the detail form offers: the ones ingest can fetch.
// Blog and news are declared in config but only skipped by ingest, so they stay
// out until they work.
var sourceTypes = []config.FeedType{config.FeedTypeRSS, config.FeedTypeAcademic}

// sourcesData is the Sources page model: the whole feed set as rows, one of
// which may have its editor open under it.
type sourcesData struct {
	Title      string
	Active     string
	Chrome     chromeData
	PromptUser string
	// Section is which Sources page is showing: "overview" or "manage".
	Section string
	// Overview is the Overview page's model, filled only when Section is overview.
	Overview overviewData
	// Library is the Library page's model, filled only when Section is library.
	Library *libraryPage
	// LibNav is the nav's Library group, on every section.
	LibNav libNav

	Counts sourceCounts
	// Attention is the Manage status block: failing sources, flaky notes and the
	// rerun bar.
	Attention sourcesAttention
	Defaults  string // one-line summary of the cascade
	Error     string // fetch history unavailable: a note, not a failure

	Rows []feedRowData
	// Adding is the add form, shown at the top of the list as an inline editor.
	Adding *feedDetail
	// LiveNote is the running note sent with a live refresh.
	LiveNote ingestBarData
	// Unit is set on a one-source response: the row to render, with its editor
	// when open.
	Unit *feedRowData
	// AllTags is every tag in use across the set, comma-joined — what the tag
	// input suggests from.
	AllTags string

	// DefaultSince and DefaultCap are what a blank tuning field falls back to,
	// and SinceFrom and CapFrom say where that fallback comes from. They are
	// computed once per render rather than per feed because the answer is the
	// same for every feed: the blank replaces the feed's own value, so what's
	// left is the defaults → ingest.since → built-in chain. That also gives the
	// add form, which has no resolved feed yet, correct placeholders.
	DefaultSince string
	DefaultCap   string
	SinceFrom    string
	CapFrom      string
	// DefaultOn is whether a source that sets no flag of its own is fetched.
	DefaultOn bool

	// Import is the outcome of a feeds-file import, shown above the list.
	Import *importStatus
	// Flash is the toast marker of a bulk or undo response.
	Flash flashData
}

// importStatus is what a feeds-file import reports: counts and warnings, or why
// the file was refused.
type importStatus struct {
	Msg      string
	Warnings []string
	Err      string
}

// sourceCounts is the Sources page's tally. It feeds the Manage summary line,
// the filter pill counts and the nav badge, and re-renders out of band on every
// mutation so they stay true while the toolbar stays out of the swap that
// would wipe the search box.
type sourceCounts struct {
	OOB     bool
	Total   int
	Enabled int
	Failing int
	Flaky   int
	Deleted int
}

// feedDetail is one source's editor: its settings as typed. A tuning field
// left blank takes the default, and the field's placeholder shows what that
// default is, so the form posts back an empty string and the page never has to
// explain the word "inherited".
type feedDetail struct {
	// Adding makes it the new-feed form: same fields, and a create rather than
	// a save.
	Adding bool

	ID   string
	Name string
	URL  string
	// Type is the feed's kind, resolved: an unset type shows as rss.
	Type string

	// EnabledChoice is the Fetching pill: on, off, or blank to follow the
	// defaults.
	EnabledChoice string
	Since         string
	MaxItems      string
	Tags          []string
	// TagsValue is Tags comma-joined, the hidden field the tag input drives.
	TagsValue string

	// OwnSince and OwnMax are the values stored on the source itself, blank
	// when it takes the default. The hints tell them apart from typed ones.
	OwnSince string
	OwnMax   string
	// DefaultSince and DefaultCap are what a blank field falls back to, with
	// the origin words (SinceFrom, CapFrom) the hints print beside them. Not
	// per-feed: see sourcesData.
	DefaultSince string
	DefaultCap   string
	SinceFrom    string
	CapFrom      string
	// DefaultOn is what the Default pill follows, for its hint.
	DefaultOn bool

	// InheritedTags come from the defaults and are unioned onto the feed's own.
	// Only a seed file can set them now, so they show as fixed chips that
	// explain a tag the feed doesn't list itself.
	InheritedTags []string

	Err    string // validation failure, rendered against the form
	Notice string // something that worked but is worth saying, e.g. items re-filed

	// Side is the panel beside the form: the source's last 30 days and fetch
	// history. Nil while adding.
	Side *sideData
}

// TypeChoices is what the type picker offers. A feed seeded with a kind the
// form doesn't offer keeps it as an extra choice, so saving it doesn't quietly
// turn it into RSS.
func (d *feedDetail) TypeChoices() []string {
	out := make([]string, 0, len(sourceTypes)+1)
	known := false
	for _, t := range sourceTypes {
		out = append(out, string(t))
		known = known || string(t) == d.Type
	}
	if !known && d.Type != "" {
		out = append(out, d.Type)
	}
	return out
}

// defaultsData backs the defaults editor modal. No tags field: a tag applied to
// every feed can't tell any two apart, so offering one only invites a setting
// that makes the feed page's tag filter useless. Seed files may still set
// defaults.tags, and handleSourceSaveDefaults preserves it.
type defaultsData struct {
	EnabledChoice string
	Since         string
	MaxItems      string
	BuiltinSince  string // the fallback when Since is blank and no default is set
	Err           string
}

// exportData backs the export modal: the live set rendered as a seed file.
type exportData struct {
	YAML      string
	FeedCount int
	// Selected is set for an export of chosen sources rather than the whole set.
	Selected bool
	// Msg is a plain explanation shown in place of the file; Err is a failure.
	Msg string
	Err string
}

// handleSources renders the Sources overview page.
func (s *Web) handleSources(w http.ResponseWriter, r *http.Request) {
	s.renderSourcesPage(w, r, "", "overview")
}

// handleSourcesManage renders the feed list and editor, preselecting the feed
// named by the feed query parameter.
func (s *Web) handleSourcesManage(w http.ResponseWriter, r *http.Request) {
	s.renderSourcesPage(w, r, r.URL.Query().Get("feed"), "manage")
}

// renderSourcesPage writes one Sources page.
func (s *Web) renderSourcesPage(w http.ResponseWriter, r *http.Request, selected, section string) {
	data, err := s.sourcesPageData(r.Context(), selected, section)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.writeSourcesPage(w, data)
}

// sourcesPageData is the model of a whole Sources page. The chrome is filled
// here rather than in sourcesData, which also serves the htmx mutations and has
// no use for it. The nav reads the library without waiting for it.
func (s *Web) sourcesPageData(ctx context.Context, selected, section string) (sourcesData, error) {
	data, err := s.sourcesData(ctx, selected, section != "library")
	if err != nil {
		return sourcesData{}, err
	}
	data.Title = "Sources"
	data.Active = "sources"
	data.Chrome = s.chrome(ctx)
	data.Section = section
	data.LibNav = libNavFor(s.library.Snapshot(), section, "")
	if section == "overview" {
		data.Overview = s.overview(ctx, data)
	}
	return data, nil
}

func (s *Web) writeSourcesPage(w http.ResponseWriter, data sourcesData) {
	var buf bytes.Buffer
	if err := sourcesTmpl.ExecuteTemplate(&buf, "layout", data); err != nil {
		log.Error().Err(err).Msg("render sources")
		http.Error(w, "could not render sources", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

// handleSourcesLive answers the refresh a page makes when an ingestion run
// starts or ends. It is out-of-band updates only, so nothing is swapped from
// under an editor in progress: page=manage re-sends the status block, counts,
// running note and every row except those named in open; page=overview re-sends
// the Overview.
func (s *Web) handleSourcesLive(w http.ResponseWriter, r *http.Request) {
	httplog.Quiet(r)
	data, err := s.sourcesData(r.Context(), "", true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data.Counts.OOB, data.Attention.OOB = true, true
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	name := "sourceLiveManage"
	if r.URL.Query().Get("page") == "overview" {
		data.Overview = s.overview(r.Context(), data)
		name = "sourceLiveOverview"
	} else {
		open := make(map[string]bool)
		for _, id := range strings.Split(r.URL.Query().Get("open"), ",") {
			open[id] = true
		}
		for i := range data.Rows {
			data.Rows[i].OOB = !open[data.Rows[i].ID]
		}
		data.LiveNote = data.Attention.NoteBar()
		data.LiveNote.OOB = true
	}
	if err := feedTmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Error().Err(err).Msg("render sources live")
	}
}

// handleSourceSelect opens one source's editor under its row.
func (s *Web) handleSourceSelect(w http.ResponseWriter, r *http.Request) {
	s.writeUnit(w, r, r.PathValue("id"), unitOpts{open: true})
}

// handleSourceNew opens the add form at the top of the list.
func (s *Web) handleSourceNew(w http.ResponseWriter, r *http.Request) {
	s.renderSourcesBody(w, r, "", &feedDetail{Adding: true, Type: string(config.FeedTypeRSS)})
}

// handleSourceAdd creates a feed. A failure re-renders the form with the
// message and everything the user typed still in it.
//
// A success leaves the add form open and empty rather than opening the feed it
// just made: feeds arrive in batches, and landing in the editor for one you have
// nothing more to say about means clicking "add" again for every single one.
// The new feed is in the list under the form either way, and the notice names
// what was added.
func (s *Web) handleSourceAdd(w http.ResponseWriter, r *http.Request) {
	feed, err := feedFromForm(r, "")
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
		Adding: true,
		Type:   string(config.FeedTypeRSS),
		Notice: "Added " + feed.Name + ". It is fetched on the next ingestion run.",
	})
}

var errRenameDuringRun = errors.New("finish or cancel the ingest run before renaming")

// handleSourceSave writes the editor back and answers with that source's unit,
// editor still open.
//
// A rename also re-files the feed's existing items in the same transaction. It
// is refused while an ingest run is live, since the run files new items under
// the name it resolved at start.
func (s *Web) handleSourceSave(w http.ResponseWriter, r *http.Request) {
	ctx, id := r.Context(), r.PathValue("id")
	previous, err := s.db.FeedByID(ctx, id)
	if err != nil {
		httpFeedError(w, err)
		return
	}
	feed, err := feedFromForm(r, previous.Type)
	if err != nil {
		s.writeUnit(w, r, id, unitOpts{override: editFormWithError(r, id, err)})
		return
	}
	renamed := previous.Name != feed.Name
	if renamed && s.ing.Status().Running {
		s.writeUnit(w, r, id, unitOpts{override: editFormWithError(r, id, errRenameDuringRun)})
		return
	}
	moved, err := s.db.UpdateFeed(ctx, id, feed)
	if err != nil {
		if isFeedInputError(err) {
			s.writeUnit(w, r, id, unitOpts{override: editFormWithError(r, id, err)})
			return
		}
		httpFeedError(w, err)
		return
	}

	notice := "Saved"
	if renamed {
		s.srcStats.invalidate()
	}
	if moved > 0 {
		notice += fmt.Sprintf(": renamed · re-filed %s", itemsPhrase(int(moved)))
	}
	s.afterFeedChange(ctx)
	s.writeUnit(w, r, id, unitOpts{override: &feedDetail{Notice: notice}, oob: true})
}

// enabledOn is whether a source is fetched: its own flag, else the defaults',
// else on.
func enabledOn(own *bool, defaults config.FeedDefaults) bool {
	switch {
	case own != nil:
		return *own
	case defaults.Enabled != nil:
		return *defaults.Enabled
	default:
		return true
	}
}

// handleSourceEnabled parks or unparks a feed from its row in the list, always
// as an explicit value. The response is the row's unit; the editor stays open
// when the request says it was (open=1), and the page asks before a dirty one
// is replaced. A request that sets undo=1 also gets a toast with an Undo.
func (s *Web) handleSourceEnabled(w http.ResponseWriter, r *http.Request) {
	ctx, id := r.Context(), r.PathValue("id")
	on := r.FormValue("enabled") == onChoice
	var flash flashData
	if r.FormValue("undo") == "1" {
		feed, err := s.db.FeedByID(ctx, id)
		if err != nil {
			httpFeedError(w, err)
			return
		}
		states, err := s.db.SetFeedsEnabled(ctx, []string{id}, on)
		if err != nil {
			httpFeedError(w, err)
			return
		}
		op, verb := store.BulkDisable, "Disabled "
		if on {
			op, verb = store.BulkEnable, "Enabled "
		}
		flash = s.undoFlash(verb+feed.Name, op, "", states)
	} else if err := s.db.SetFeedEnabled(ctx, id, &on); err != nil {
		httpFeedError(w, err)
		return
	}
	s.afterFeedChange(ctx)
	s.writeUnit(w, r, id, unitOpts{open: r.FormValue("open") == "1", oob: true, flash: flash})
}

// handleSourceDelete parks a feed out of sight. The row survives, so its fetch
// history stays attached and re-seeding won't bring it back. There is no
// confirmation: the response is the unit in its removed state and a toast whose
// Undo restores it.
func (s *Web) handleSourceDelete(w http.ResponseWriter, r *http.Request) {
	ctx, id := r.Context(), r.PathValue("id")
	feed, err := s.db.FeedByID(ctx, id)
	if err != nil {
		httpFeedError(w, err)
		return
	}
	states, err := s.db.SoftDeleteFeeds(ctx, []string{id})
	if err != nil {
		httpFeedError(w, err)
		return
	}
	s.afterFeedChange(ctx)
	s.writeUnit(w, r, id, unitOpts{oob: true, flash: s.undoFlash("Removed "+feed.Name, store.BulkRemove, "", states)})
}

// handleSourceRestore brings a removed feed back and offers to remove it again.
func (s *Web) handleSourceRestore(w http.ResponseWriter, r *http.Request) {
	ctx, id := r.Context(), r.PathValue("id")
	feed, err := s.db.FeedByID(ctx, id)
	if err != nil {
		httpFeedError(w, err)
		return
	}
	states, err := s.db.RestoreFeeds(ctx, []string{id})
	if err != nil {
		httpFeedError(w, err)
		return
	}
	s.afterFeedChange(ctx)
	s.writeUnit(w, r, id, unitOpts{oob: true, flash: s.undoFlash("Restored "+feed.Name, store.BulkRestore, "", states)})
}

// undoFlash records a change in the undo log and returns its toast. A change
// that touched no row has nothing to undo, so it gets no toast.
func (s *Web) undoFlash(msg string, op store.BulkOp, tag string, states []store.FeedState) flashData {
	if len(states) == 0 {
		return flashData{}
	}
	ids := make([]string, len(states))
	for i, st := range states {
		ids[i] = st.ID
	}
	return flashData{
		Msg:  msg,
		Undo: s.undo.put(undoEntry{op: op, tag: tag, states: states}, s.clock()),
		IDs:  strings.Join(ids, ","),
	}
}

func (s *Web) handleSourceDefaults(w http.ResponseWriter, r *http.Request) {
	d, err := s.db.FeedDefaults(r.Context())
	if err != nil {
		httpFeedError(w, err)
		return
	}
	s.renderFragment(w, "sourceDefaults", defaultsForm(d, ""))
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
		form := defaultsForm(defaults, err.Error())
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

// sourceImportLimit caps an uploaded feeds file.
const sourceImportLimit = 1 << 20

// feedCountHeader carries the feed count on a Sources mutation response.
const feedCountHeader = "X-Feed-Count"

// handleSourceImport adds the feeds of an uploaded seed file that the store has
// never seen, the same operation as `rabbithole sources import`. A file that
// cannot be used comes back as an inline message, like the add form's errors.
func (s *Web) handleSourceImport(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	r.Body = http.MaxBytesReader(w, r.Body, sourceImportLimit)
	fail := func(msg string) {
		s.writeSourcesBodyWith(w, r, "", nil, &importStatus{Err: msg})
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			fail("feeds file is too large (limit 1 MiB)")
			return
		}
		fail("choose a feeds YAML file to import")
		return
	}
	defer func() { _ = file.Close() }()
	raw, err := io.ReadAll(file)
	if err != nil {
		fail("could not read the uploaded file")
		return
	}
	doc, err := config.ParseFeeds(raw)
	if err != nil {
		fail(err.Error())
		return
	}
	result, err := s.db.SeedFeeds(r.Context(), *doc)
	if err != nil {
		httpFeedError(w, err)
		return
	}
	s.afterFeedChange(r.Context())
	s.writeSourcesBodyWith(w, r, "", nil, &importStatus{
		Msg: fmt.Sprintf("added %d feed(s), skipped %d already known (including deleted ones)",
			result.Added, result.Skipped),
		Warnings: result.Warnings,
	})
}

// handleSourceExport renders the live set as a seed file: the same YAML shape
// feeds.yaml uses, so it can be copied straight back out to one. With ids it
// renders just those sources and leaves the defaults out, since the file is a
// share and whoever imports it has defaults of their own.
func (s *Web) handleSourceExport(w http.ResponseWriter, r *http.Request) {
	var data exportData
	doc, err := s.db.FeedsDoc(r.Context())
	if err != nil {
		data.Err = err.Error()
		s.renderFragment(w, "sourceExport", data)
		return
	}
	if r.URL.Query().Has("ids") {
		ids, ok := parseBulkIDs(r.URL.Query().Get("ids"))
		if !ok {
			http.Error(w, bulkTooManyCopy, http.StatusBadRequest)
			return
		}
		data.Selected = true
		doc = config.FeedsDoc{Feeds: slices.DeleteFunc(doc.Feeds, func(f config.Feed) bool {
			return !slices.Contains(ids, f.ID)
		})}
		if len(doc.Feeds) == 0 {
			data.Msg = "None of the selected sources exist any more."
			s.renderFragment(w, "sourceExport", data)
			return
		}
	}
	raw, err := config.MarshalFeeds(doc)
	if err != nil {
		data.Err = err.Error()
		s.renderFragment(w, "sourceExport", data)
		return
	}
	data.YAML, data.FeedCount = raw, len(doc.Feeds)
	s.renderFragment(w, "sourceExport", data)
}

// navBadge is the Manage link's failing count, out of band. It reads only the
// feeds and their history, which is all the count needs, and is nil when they
// cannot be read.
func (s *Web) navBadge(ctx context.Context) *sourceCounts {
	failing, err := s.failingCount(ctx)
	if err != nil {
		log.Error().Err(err).Msg("read nav badge")
		return nil
	}
	return &sourceCounts{OOB: true, Failing: failing}
}

func (s *Web) failingCount(ctx context.Context) (int, error) {
	doc, err := s.db.FeedsDoc(ctx)
	if err != nil {
		return 0, err
	}
	resolved, err := config.ResolveFeeds(doc)
	if err != nil {
		return 0, err
	}
	health, err := s.db.FeedHealthByID(ctx, feedStripLen)
	if err != nil {
		return 0, err
	}
	now, n := s.clock(), 0
	for _, f := range resolved {
		if toFeedRow(f, health[f.ID], now).State == feedStateError {
			n++
		}
	}
	return n, nil
}

// sourcesData assembles the Sources page model: the resolved feed set joined with each feed's
// fetch history, plus the editor of the selected feed, if any. withStats adds
// each feed's volume and signal; the Library page has no use for them.
func (s *Web) sourcesData(ctx context.Context, selected string, withStats bool) (sourcesData, error) {
	doc, err := s.db.FeedsDoc(ctx)
	if err != nil {
		return sourcesData{}, err
	}
	resolved, err := config.ResolveFeeds(doc)
	if err != nil {
		return sourcesData{}, err
	}
	deleted, removedAt, err := s.db.DeletedFeedsAt(ctx)
	if err != nil {
		return sourcesData{}, err
	}

	defaultSince, defaultCap := blankFallbacks(doc.Defaults)
	sinceFrom, capFrom := fallbackOrigins(doc.Defaults)
	data := sourcesData{
		PromptUser:   s.pagePromptUser(ctx),
		Defaults:     defaultsSummary(doc.Defaults),
		DefaultSince: defaultSince,
		DefaultCap:   defaultCap,
		SinceFrom:    sinceFrom,
		CapFrom:      capFrom,
		DefaultOn:    enabledOn(nil, doc.Defaults),
		Counts:       sourceCounts{Total: len(resolved), Deleted: len(deleted)},
	}

	// History is a join, not a requirement: losing it still leaves a page you
	// can edit, so it degrades to a note rather than an error.
	health, err := s.db.FeedHealthByID(ctx, feedStripLen)
	if err != nil {
		log.Error().Err(err).Msg("read feed health")
		data.Error = "fetch history unavailable: " + err.Error()
	}

	now := s.clock()
	data.Rows = make([]feedRowData, 0, len(resolved)+len(deleted))
	for _, f := range resolved {
		row := toFeedRow(f, health[f.ID], now)
		if f.Enabled {
			data.Counts.Enabled++
		}
		switch row.State {
		case feedStateError:
			data.Counts.Failing++
		case feedStateFlaky:
			data.Counts.Flaky++
		}
		data.Rows = append(data.Rows, row)
	}
	if len(deleted) > 0 {
		// Deleted feeds aren't in doc, so they go through the cascade here to
		// render with the same effective values as the rest.
		gone, err := config.ResolveFeeds(config.FeedsDoc{Defaults: doc.Defaults, Feeds: deleted})
		if err != nil {
			return sourcesData{}, err
		}
		for _, f := range gone {
			row := removedRow(f, health[f.ID], now)
			if at, ok := removedAt[f.ID]; ok {
				row.DeletedAt, row.DeletedAgo = at, agoPhrase(at, now)
			}
			data.Rows = append(data.Rows, row)
		}
	}

	// One read of the newest run serves the stats cache key and the ingest bar;
	// a failed read leaves both as if there were no run.
	last, err := s.db.LastIngestRun(ctx)
	if err != nil {
		log.Error().Err(err).Msg("read ingest bar")
		last = nil
	}
	if withStats {
		s.addStats(ctx, &data, health, last, now)
	}

	bar := s.ingestBarWith(data.Counts.Enabled, last, true, true)
	// This bar carries the running note out of band, which only this page has.
	bar.Note = true
	data.Attention = newAttention(data.Rows, data.Counts.Total, bar)

	data.AllTags = strings.Join(withLibraryLabels(tagUniverse(resolved), s.library.Snapshot()), ",")
	// A removed source has no editor: restoring it is the only way back.
	for i := range data.Rows {
		if row := &data.Rows[i]; selected != "" && row.ID == selected && !row.Deleted {
			row.Editor = s.detailFor(ctx, selected, doc.Defaults)
			row.Selected = row.Editor != nil
			if row.Editor != nil && withStats {
				row.Editor.Side = row.side(health[row.ID], now)
			}
		}
	}
	own := ownTagsByID(doc.Feeds, deleted)
	for i := range data.Rows {
		data.Rows[i].OwnTagsAttr = strings.Join(own[data.Rows[i].ID], ",")
	}
	data.stampAll()
	return data, nil
}

// ownTagsByID maps each source to the tags it sets itself, which the merged
// list on its row does not tell apart from the defaults'.
func ownTagsByID(lists ...[]config.Feed) map[string][]string {
	out := make(map[string][]string)
	for _, feeds := range lists {
		for _, f := range feeds {
			id := f.ID
			if id == "" {
				id = config.FeedID(f.URL)
			}
			out[id] = f.Tags
		}
	}
	return out
}

// stamp copies the page's blank-field fallbacks onto an editor, wherever it
// came from (the store, a fresh add form, or a rejected submission being
// re-rendered), and words its hints. The template only has the editor as its
// dot, not the page.
func (d *sourcesData) stamp(e *feedDetail) {
	if e == nil {
		return
	}
	e.DefaultSince, e.DefaultCap = d.DefaultSince, d.DefaultCap
	e.SinceFrom, e.CapFrom = d.SinceFrom, d.CapFrom
	e.DefaultOn = d.DefaultOn
}

// stampAll stamps the add form and the open editor.
func (d *sourcesData) stampAll() {
	d.stamp(d.Adding)
	for i := range d.Rows {
		d.stamp(d.Rows[i].Editor)
	}
	if d.Unit != nil {
		d.stamp(d.Unit.Editor)
	}
}

// fallbackOrigins says where a blank field's fallback comes from: the defaults
// when they set it, the built-in otherwise.
func fallbackOrigins(d config.FeedDefaults) (since, cap string) {
	since, cap = originBuiltin, originBuiltin
	if d.Since != nil {
		since = originDefaults
	}
	if d.MaxItems != nil && *d.MaxItems > 0 {
		cap = originDefaults
	}
	return since, cap
}

// Origin words for the tuning hints.
const (
	originDefaults = "from defaults"
	originBuiltin  = "built-in"
	originOwn      = "set on this source"
	originTyped    = "set here"
)

// Insecure reports whether the address is plain http, which the editor flags.
func (d *feedDetail) Insecure() bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(d.URL)), "http://")
}

// SinceHint words the line under the Since field for what it holds now.
func (d *feedDetail) SinceHint() string {
	return tuningHint(d.Since, d.OwnSince, d.DefaultSince, d.SinceFrom)
}

// CapHint words the line under the Max items field for what it holds now.
func (d *feedDetail) CapHint() string {
	return tuningHint(d.MaxItems, d.OwnMax, d.DefaultCap, d.CapFrom)
}

// tuningHint says what a tuning field will do. Blank takes the fallback and
// names where it comes from; the value already stored on the source reads as
// its own; anything else is a pending change that names what it would replace.
func tuningHint(typed, own, fallback, from string) string {
	switch typed {
	case "":
		return fallback + " " + from
	case own:
		return typed + " " + originOwn
	default:
		return typed + " " + originTyped + " (otherwise " + fallback + " " + from + ")"
	}
}

// blankFallbacks resolves what a feed gets when it sets neither knob — the
// values the detail form shows as greyed placeholders. It walks the same chain
// resolveFeed does, minus the feed itself, which is exactly what a blank field
// means: defaults, then the built-in.
//
// The cap reads as "uncapped" rather than capLabel's em dash: in a placeholder
// a dash says "no value", where what's meant is "no limit".
func blankFallbacks(d config.FeedDefaults) (since, cap string) {
	since = shortDur(config.DefaultSince)
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

// withLibraryLabels appends the library's labels to the tags in use, skipping
// any that differ only in case: the first casing wins.
func withLibraryLabels(tags []string, snap library.Snapshot) []string {
	if !snap.Loaded || snap.Off {
		return tags
	}
	seen := make(map[string]bool, len(tags))
	for _, t := range tags {
		seen[strings.ToLower(t)] = true
	}
	for _, e := range libraryEntries(snap) {
		for _, l := range e.Source.Labels {
			if k := strings.ToLower(l); !seen[k] {
				seen[k] = true
				tags = append(tags, l)
			}
		}
	}
	return tags
}

// detailFor builds the editor for a source. The lookup goes to the store rather
// than the resolved list so the editor shows the values the source sets itself,
// not the ones it inherits.
func (s *Web) detailFor(ctx context.Context, id string, defaults config.FeedDefaults) *feedDetail {
	if id == "" {
		return nil
	}
	own, err := s.db.FeedByID(ctx, id)
	if err != nil {
		if !errors.Is(err, store.ErrFeedNotFound) {
			log.Error().Err(err).Str("feed", id).Msg("read feed for the editor")
		}
		return nil
	}

	detail := &feedDetail{
		ID:            own.ID,
		Name:          own.Name,
		URL:           own.URL,
		Type:          string(resolvedType(own.Type)),
		EnabledChoice: enabledChoice(own.Enabled),
		Tags:          own.Tags,
		TagsValue:     strings.Join(own.Tags, ","),
		InheritedTags: inheritedOnly(defaults.Tags, own.Tags),
	}
	if own.Since != nil {
		detail.Since = own.Since.Short()
		detail.OwnSince = detail.Since
	}
	if own.MaxItems != nil {
		detail.MaxItems = strconv.Itoa(*own.MaxItems)
		detail.OwnMax = detail.MaxItems
	}
	return detail
}

// unitOpts says how writeUnit answers a one-source request.
type unitOpts struct {
	// open renders the source's editor from the store.
	open bool
	// override is an editor to show instead: a form with an error, or a notice
	// laid over the stored one.
	override *feedDetail
	// oob adds the out-of-band updates a mutation owes the page: summary,
	// filter counts, nav badge and attention block.
	oob bool
	// flash is the toast marker sent after the unit, when it has a message.
	flash flashData
}

// writeUnit answers with one source's list unit: its row, and its editor when
// open. Every mutation of a single source lands here, so the swap touches that
// unit and nothing else.
func (s *Web) writeUnit(w http.ResponseWriter, r *http.Request, id string, o unitOpts) {
	selected := ""
	if o.open || o.override != nil {
		selected = id
	}
	data, err := s.sourcesData(r.Context(), selected, true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var row *feedRowData
	for i := range data.Rows {
		if data.Rows[i].ID == id {
			row = &data.Rows[i]
			break
		}
	}
	if row == nil {
		http.Error(w, store.ErrFeedNotFound.Error(), http.StatusNotFound)
		return
	}
	// An explicit editor (a form with an error, or a notice) wins over the one
	// loaded from the store, so what the user typed isn't thrown away.
	if o.override != nil && !row.Deleted {
		row.Editor = mergeDetail(row.Editor, o.override)
		row.Selected = true
	}
	data.Unit = row
	data.stampAll()
	if o.oob {
		data.Counts.OOB, data.Attention.OOB = true, true
		// The settings page reads the feed count here instead of loading itself again.
		w.Header().Set(feedCountHeader, strconv.Itoa(data.Counts.Total))
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := feedTmpl.ExecuteTemplate(w, "sourceUnit", data); err != nil {
		log.Error().Err(err).Msg("render source unit")
		return
	}
	if o.flash.Msg != "" {
		if err := feedTmpl.ExecuteTemplate(w, "sourceFlash", o.flash); err != nil {
			log.Error().Err(err).Msg("render source flash")
		}
	}
}

// renderSourcesBody re-renders the whole list and, out of band, the page's
// counts. Adding, importing and saving defaults land here: they can change any
// number of rows, and keeping the search box outside the swap means a filtered
// view survives.
func (s *Web) renderSourcesBody(w http.ResponseWriter, r *http.Request, selected string, adding *feedDetail) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	s.writeSourcesBody(w, r, selected, adding)
}

func (s *Web) writeSourcesBody(w http.ResponseWriter, r *http.Request, selected string, adding *feedDetail) {
	s.writeSourcesBodyWith(w, r, selected, adding, nil)
}

func (s *Web) writeSourcesBodyWith(
	w http.ResponseWriter, r *http.Request, selected string, adding *feedDetail, status *importStatus,
) {
	data, err := s.sourcesData(r.Context(), selected, true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data.Import = status
	data.Adding = adding
	data.stampAll()
	data.Counts.OOB, data.Attention.OOB = true, true
	// The settings page reads the feed count here instead of loading itself again.
	w.Header().Set(feedCountHeader, strconv.Itoa(data.Counts.Total))
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
	configured, err := ingest.ResolveFeeds(ctx, s.db)
	if err != nil {
		log.Warn().Err(err).Msg("resolving feeds after an edit failed")
		return
	}
	if err := s.db.SyncSourceTags(ctx, ingest.ConfiguredTags(configured)); err != nil {
		log.Warn().Err(err).Msg("syncing feed tags failed")
	}
}

func defaultsForm(d config.FeedDefaults, errMsg string) defaultsData {
	form := defaultsData{
		EnabledChoice: enabledChoice(d.Enabled),
		BuiltinSince:  shortDur(config.DefaultSince),
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
// nil pointer here and a NULL in the store — not a zero value. keep is the
// type the feed already has, empty when adding.
func feedFromForm(r *http.Request, keep config.FeedType) (config.Feed, error) {
	feed := config.Feed{
		Name: strings.TrimSpace(r.FormValue("name")),
		URL:  strings.TrimSpace(r.FormValue("url")),
		Tags: splitCommaList(r.FormValue("tags")),
	}
	typ, err := typeFromForm(config.FeedType(r.FormValue("type")), keep)
	if err != nil {
		return feed, err
	}
	feed.Type = typ
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

// typeFromForm maps the posted type onto what the store keeps. RSS is stored
// as unset, so feeds added here read back like every feed from before types
// existed, unless the feed already said rss explicitly, which is kept so its
// export doesn't change. A type the form doesn't offer is only accepted when
// it is the one the feed already has.
func typeFromForm(posted, keep config.FeedType) (config.FeedType, error) {
	switch {
	case resolvedType(posted) == config.FeedTypeRSS && resolvedType(keep) == config.FeedTypeRSS:
		return keep, nil
	case resolvedType(posted) == config.FeedTypeRSS:
		return "", nil
	case posted == keep:
		return keep, nil
	}
	for _, t := range sourceTypes {
		if posted == t {
			return t, nil
		}
	}
	return "", fmt.Errorf("type %q can't be fetched yet: pick one of rss, academic", posted)
}

// resolvedType is t with unset read as rss, the way the cascade resolves it.
func resolvedType(t config.FeedType) config.FeedType {
	if t == "" {
		return config.FeedTypeRSS
	}
	return t
}

func defaultsFromForm(r *http.Request) (config.FeedDefaults, error) {
	feed, err := feedFromForm(r, "")
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
		Type:          string(resolvedType(config.FeedType(r.FormValue("type")))),
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

// mergeDetail lays the handler's override over the editor loaded from the store.
// A notice on its own leaves the stored values alone; a form carrying an error
// replaces them, because those are the values the user is still editing.
func mergeDetail(stored, override *feedDetail) *feedDetail {
	if override.Err == "" {
		if stored == nil {
			return override
		}
		stored.Notice = override.Notice
		return stored
	}
	if stored != nil {
		override.InheritedTags, override.OwnSince, override.OwnMax = stored.InheritedTags, stored.OwnSince, stored.OwnMax
		override.Side = stored.Side
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
