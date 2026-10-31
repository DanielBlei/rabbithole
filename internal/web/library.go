// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/httplog"
	"github.com/DanielBlei/rabbithole/internal/library"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// What the Library row shows for a source: add is ready to add, the rest are
// the store's verdict on the entry.
const (
	libStateAdd        = "add"
	libStateSubscribed = "subscribed"
	libStateRemoved    = "removed"
	libStateConflict   = "conflict"
)

const (
	libraryTitle   = "Add from the library"
	libraryApplyTo = "/sources/library/apply"
	libraryTarget  = "#libBody"

	libraryOffCopy     = "The Library is turned off on this server."
	libraryOfflineCopy = "Could not load the Library. Check your connection and try again."
)

var errUnknownGroup = errors.New("unknown library group")

// libraryPage is the Library section: the header, filter tools and every
// group's entries, or the reason there is nothing to show.
type libraryPage struct {
	Off     bool // the library is turned off on this server
	Offline bool // nothing has been fetched yet
	Notice  string

	Group string // the group shown, empty for all
	Title string
	Desc  string
	// Scope words the filter's empty states: "the Library" or "this group".
	Scope string

	Total, Added int
	Empty        bool // the library holds no entries
	AllAdded     bool // nothing left to add in the scope

	Sections []libSection
	Labels   []libLabel
	Filters  []libFilter

	// Plan replaces the list when set. Import is the banner above it.
	Plan   *importPlan
	Import *importStatus
	// MetaOOB sends the header's count line out of band, for a reply that
	// repaints only the body.
	MetaOOB bool
	// Badge is the Manage link's failing count, sent out of band by a reply to an
	// action. Restoring a source can change it.
	Badge *sourceCounts
}

// Meta is the line under the description.
func (p *libraryPage) Meta() string {
	return fmt.Sprintf("%s, %d added", plural(p.Total, "source", "sources"), p.Added)
}

type libSection struct {
	Name    string
	Title   string
	Desc    string
	Single  bool // the page shows only this group, so it needs no heading
	Entries []libEntry
}

// libEntry is one library source and the store's verdict on it.
type libEntry struct {
	Key      string
	ID       string
	Name     string
	URL      string
	Desc     string
	Kind     string // the ingestion type when it is not a plain feed
	Labels   []planTag
	MaxItems int
	State    string
	Chip     string
	Reason   string
	// Hay is what the filter's words are matched against, lower case.
	Hay string
	// Icon is the icon of the subscribed source with this id, zero otherwise.
	Icon iconRef
}

type libLabel struct {
	Name  string
	Count int
}

type libFilter struct {
	Key   string
	Label string
	Count int
}

// libNav is the Library group of the section nav.
type libNav struct {
	// OOB sends the group out of band, for a reply that is not the whole page.
	OOB    bool
	AllOn  bool
	Total  int
	Groups []libNavGroup // empty until the library has loaded

	// Fill is the URL that repaints the group once the library has loaded, set
	// while it has not; empty when the group is final.
	Fill string
}

type libNavGroup struct {
	Name  string
	Title string
	Count int
	On    bool
}

// libraryEntry is a library source with the file type and group it sits in.
type libraryEntry struct {
	Type   library.Type
	Group  library.Group
	Source library.Source
}

func libraryEntries(snap library.Snapshot) []libraryEntry {
	var out []libraryEntry
	for _, f := range snap.Library.Files {
		for _, g := range f.Groups {
			for _, src := range g.Sources {
				out = append(out, libraryEntry{Type: f.Type, Group: g, Source: src})
			}
		}
	}
	return out
}

// libraryFeed is the source the entry adds.
func libraryFeed(typ library.Type, src library.Source) config.Feed {
	f := config.Feed{Name: src.Name, URL: src.URL, Tags: slices.Clone(src.Labels)}
	if typ == library.TypeAcademic {
		f.Type = config.FeedTypeAcademic
	}
	if src.MaxItems > 0 {
		n := src.MaxItems
		f.MaxItems = &n
	}
	return f
}

// libraryDoc holds the entries whose keys are picked, in library order; a nil
// pick holds them all.
func libraryDoc(snap library.Snapshot, pick map[string]bool) config.FeedsDoc {
	var doc config.FeedsDoc
	for _, e := range libraryEntries(snap) {
		if pick != nil && !pick[config.FeedURLKey(e.Source.URL)] {
			continue
		}
		doc.Feeds = append(doc.Feeds, libraryFeed(e.Type, e.Source))
	}
	return doc
}

// libraryStates plans every library entry against the stored sources and
// returns the verdicts by config.FeedURLKey.
func (s *Web) libraryStates(ctx context.Context, snap library.Snapshot) (map[string]store.PlanRow, error) {
	doc := libraryDoc(snap, nil)
	states := make(map[string]store.PlanRow, len(doc.Feeds))
	if len(doc.Feeds) == 0 {
		return states, nil
	}
	rows, err := s.db.PlanImport(ctx, doc)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		states[r.Key] = r
	}
	return states, nil
}

// libraryState is the row's state word and the chip text it wears.
func libraryState(row store.PlanRow) (state, chip string) {
	switch row.Status {
	case store.PlanNew:
		return libStateAdd, ""
	case store.PlanSubscribed:
		return libStateSubscribed, planLabels[row.Status]
	case store.PlanRemoved:
		return libStateRemoved, planLabels[row.Status]
	default:
		return libStateConflict, planLabels[row.Status]
	}
}

// libraryPageFor builds the page for one group, or for all of them when group
// is empty. An unknown group is errUnknownGroup.
func (s *Web) libraryPageFor(ctx context.Context, snap library.Snapshot, group string) (*libraryPage, error) {
	p := &libraryPage{Title: "Library", Group: group, Scope: "the Library", Notice: snap.Notice}
	switch {
	case snap.Off:
		p.Off, p.Desc = true, libraryOffCopy
		return p, nil
	case !snap.Loaded:
		p.Offline, p.Desc = true, libraryOfflineCopy
		return p, nil
	}
	p.Desc = "All sources in the Library"
	if group != "" {
		g, ok := libraryGroup(snap, group)
		if !ok {
			return nil, errUnknownGroup
		}
		p.Title, p.Desc, p.Scope = g.Title, g.Description, "this group"
	}
	states, err := s.libraryStates(ctx, snap)
	if err != nil {
		return nil, err
	}

	icons := s.iconRefs(ctx)
	counts := make(map[string]int)
	stateCounts := make(map[string]int)
	addable := 0
	for _, f := range snap.Library.Files {
		for _, g := range f.Groups {
			if group != "" && g.Name != group {
				continue
			}
			sec := libSection{Name: g.Name, Title: g.Title, Desc: g.Description, Single: group != ""}
			for _, src := range g.Sources {
				key := config.FeedURLKey(src.URL)
				e := libEntry{
					Key: key, ID: config.FeedID(src.URL), Name: src.Name, URL: src.URL,
					Desc: src.Description, MaxItems: src.MaxItems,
				}
				if f.Type != library.TypeRSS {
					e.Kind = string(f.Type)
				}
				e.Icon = icons[e.ID]
				e.State, e.Chip = libraryState(states[key])
				e.Reason = states[key].Reason
				for _, l := range src.Labels {
					e.Labels = append(e.Labels, planTag{Name: l, Color: tagColor(l)})
					counts[l]++
				}
				e.Hay = strings.ToLower(strings.Join(
					append([]string{src.Name, src.Description, src.URL, g.Title}, src.Labels...), " "))
				p.Total++
				stateCounts[e.State]++
				switch e.State {
				case libStateSubscribed:
					p.Added++
				case libStateAdd:
					addable++
				}
				sec.Entries = append(sec.Entries, e)
			}
			p.Sections = append(p.Sections, sec)
		}
	}
	p.Empty = p.Total == 0
	p.AllAdded = p.Total > 0 && addable == 0
	p.Filters = []libFilter{
		{Key: "all", Label: "All", Count: p.Total},
		{Key: libStateSubscribed, Label: "Subscribed", Count: stateCounts[libStateSubscribed]},
		{Key: libStateRemoved, Label: "Removed", Count: stateCounts[libStateRemoved]},
		{Key: libStateAdd, Label: "Add", Count: stateCounts[libStateAdd]},
	}
	for i := range p.Sections {
		slices.SortFunc(p.Sections[i].Entries, func(a, b libEntry) int {
			if c := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
				return c
			}
			return strings.Compare(a.Name, b.Name)
		})
	}
	slices.SortFunc(p.Sections, func(a, b libSection) int {
		if c := strings.Compare(strings.ToLower(a.Title), strings.ToLower(b.Title)); c != 0 {
			return c
		}
		return strings.Compare(a.Title, b.Title)
	})
	for name, n := range counts {
		p.Labels = append(p.Labels, libLabel{Name: name, Count: n})
	}
	slices.SortFunc(p.Labels, func(a, b libLabel) int {
		if c := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	return p, nil
}

func libraryGroup(snap library.Snapshot, name string) (library.Group, bool) {
	for _, f := range snap.Library.Files {
		for _, g := range f.Groups {
			if g.Name == name {
				return g, true
			}
		}
	}
	return library.Group{}, false
}

// libNavFor is the nav's Library group. section and group say which item is
// current; the groups appear once the library has loaded.
func libNavFor(snap library.Snapshot, section, group string) libNav {
	on := section == "library"
	nav := libNav{AllOn: on && group == ""}
	if libraryPending(snap) {
		nav.Fill = libraryFillURL("nav", section, group, 1)
	}
	if !snap.Loaded || snap.Off {
		return nav
	}
	for _, f := range snap.Library.Files {
		for _, g := range f.Groups {
			nav.Total += len(g.Sources)
			nav.Groups = append(nav.Groups, libNavGroup{
				Name: g.Name, Title: g.Title, Count: len(g.Sources), On: on && g.Name == group,
			})
		}
	}
	slices.SortFunc(nav.Groups, func(a, b libNavGroup) int {
		if c := strings.Compare(strings.ToLower(a.Title), strings.ToLower(b.Title)); c != 0 {
			return c
		}
		return strings.Compare(a.Title, b.Title)
	})
	return nav
}

// libraryGroupOf is the group the page that sent a request is showing, as the
// URL names it; libraryPageFor says whether it exists. An htmx request names
// that page in HX-Current-URL, which lets a reply repaint the same scope
// without the plan form carrying it.
func libraryGroupOf(r *http.Request) string {
	u, err := url.Parse(r.Header.Get("HX-Current-URL"))
	if err != nil {
		return ""
	}
	name, _ := strings.CutPrefix(u.Path, "/sources/library/")
	if name == u.Path {
		return ""
	}
	return name
}

func (s *Web) handleLibrary(w http.ResponseWriter, r *http.Request) {
	s.renderLibraryPage(w, r, "")
}

func (s *Web) handleLibraryGroup(w http.ResponseWriter, r *http.Request) {
	s.renderLibraryPage(w, r, r.PathValue("group"))
}

func (s *Web) renderLibraryPage(w http.ResponseWriter, r *http.Request, group string) {
	ctx := r.Context()
	waitCtx, cancel := context.WithTimeout(ctx, s.libraryWait)
	snap := s.library.Get(waitCtx)
	cancel()
	page, err := s.libraryPageFor(ctx, snap, group)
	if errors.Is(err, errUnknownGroup) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		httpFeedError(w, err)
		return
	}
	data, err := s.sourcesPageData(ctx, "", "library")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data.Title = page.Title
	data.Library = page
	data.LibNav = libNavFor(snap, "library", group)
	s.writeSourcesPage(w, data)
}

// libraryForm reads the posted form, a multipart one when the plan form sent it.
func libraryForm(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, sourceImportLimit)
	var err error
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		err = r.ParseMultipartForm(sourceImportLimit)
	} else {
		err = r.ParseForm()
	}
	if err != nil {
		http.Error(w, "could not read the form", http.StatusBadRequest)
		return false
	}
	return true
}

func pickedKeys(r *http.Request) map[string]bool {
	pick := make(map[string]bool)
	for _, key := range r.PostForm["pick"] {
		pick[key] = true
	}
	return pick
}

// writeLibraryBody answers an action with the list repainted, or the plan in
// its place, and the header's count line out of band.
func (s *Web) writeLibraryBody(
	w http.ResponseWriter, r *http.Request, snap library.Snapshot, plan *importPlan, status *importStatus,
) {
	page, err := s.libraryPageFor(r.Context(), snap, libraryGroupOf(r))
	if errors.Is(err, errUnknownGroup) {
		// The group left the library since the page loaded: show everything.
		page, err = s.libraryPageFor(r.Context(), snap, "")
	}
	if err != nil {
		httpFeedError(w, err)
		return
	}
	if page.Offline && status == nil {
		status = &importStatus{Err: libraryOfflineCopy}
	}
	page.Plan, page.Import, page.MetaOOB = plan, status, true
	page.Badge = s.navBadge(r.Context())
	s.renderFragment(w, "libraryReply", page)
}

func newLibraryPlan() *importPlan {
	return &importPlan{Title: libraryTitle, ApplyTo: libraryApplyTo, ApplyTarget: libraryTarget, AddOnly: true}
}

// handleLibraryPlan previews adding the picked entries. The doc is built here
// from the cached library, never taken from the request.
func (s *Web) handleLibraryPlan(w http.ResponseWriter, r *http.Request) {
	if !libraryForm(w, r) {
		return
	}
	snap := s.library.Get(r.Context())
	doc := libraryDoc(snap, pickedKeys(r))
	if len(doc.Feeds) == 0 {
		s.writeLibraryBody(w, r, snap, nil, &importStatus{Err: "Nothing selected."})
		return
	}
	p, err := s.planDoc(r.Context(), newLibraryPlan(), doc)
	if err != nil {
		httpFeedError(w, err)
		return
	}
	s.writeLibraryBody(w, r, snap, p, nil)
}

// newPicks keeps the picks whose entry is still new. The Library only adds: a
// removed entry comes back through Restore, which keeps the user's edits, never
// through an apply that would overwrite them. dropped counts the rest.
func (s *Web) newPicks(
	ctx context.Context, snap library.Snapshot, pick map[string]bool,
) (kept map[string]bool, dropped int, err error) {
	states, err := s.libraryStates(ctx, snap)
	if err != nil {
		return nil, 0, err
	}
	kept = make(map[string]bool, len(pick))
	for key, on := range pick {
		if !on {
			continue
		}
		if states[key].Status == store.PlanNew {
			kept[key] = true
		} else {
			dropped++
		}
	}
	return kept, dropped, nil
}

// handleLibraryApply adds the picked entries. The whole library is planned
// again, so a pick that changed since the preview is skipped, not applied blind.
func (s *Web) handleLibraryApply(w http.ResponseWriter, r *http.Request) {
	if !libraryForm(w, r) {
		return
	}
	ctx := r.Context()
	snap := s.library.Get(ctx)
	pick := pickedKeys(r)
	suggestion := r.PostFormValue("view") == "suggestion"
	var why string
	if suggestion {
		why = s.suggestionWhy(ctx, snap, pick)
	}
	kept, dropped, err := s.newPicks(ctx, snap, pick)
	if err != nil {
		httpFeedError(w, err)
		return
	}
	result, err := s.db.ApplyImport(ctx, libraryDoc(snap, nil), kept)
	result.Skipped += dropped
	if suggestion {
		s.writeSuggestion(w, r, snap, pick, why, result, err)
		return
	}
	if applyErr := importApplyErr(result, err); applyErr != "" {
		doc := libraryDoc(snap, pick)
		if len(doc.Feeds) == 0 {
			s.writeLibraryBody(w, r, snap, nil, &importStatus{Err: applyErr})
			return
		}
		p, perr := s.planDoc(ctx, newLibraryPlan(), doc)
		if perr != nil {
			httpFeedError(w, perr)
			return
		}
		p.ApplyErr = applyErr
		s.writeLibraryBody(w, r, snap, p, nil)
		return
	}
	if err != nil {
		httpFeedError(w, err)
		return
	}
	s.afterFeedChange(ctx)
	s.writeLibraryBody(w, r, snap, nil, &importStatus{Msg: importBanner(result)})
}

// suggestionReply is the answer to the Overview's Add: the suggestion's row,
// and the coverage section out of band. The nav badge counts failing sources,
// which an add cannot change, and the rest of the Overview catches up on its
// next refresh.
type suggestionReply struct {
	Row      ovSuggestion
	Coverage ovCoverage
}

// writeSuggestion answers the Overview's Add with the row in its new state.
func (s *Web) writeSuggestion(
	w http.ResponseWriter, r *http.Request, snap library.Snapshot,
	pick map[string]bool, why string, result store.ApplyResult, err error,
) {
	ctx := r.Context()
	var row ovSuggestion
	for _, e := range libraryEntries(snap) {
		if key := config.FeedURLKey(e.Source.URL); pick[key] {
			row = ovSuggestion{
				Key: key, ID: config.FeedID(e.Source.URL), Name: e.Source.Name, URL: e.Source.URL, Why: why,
			}
			break
		}
	}
	if row.Key == "" {
		http.Error(w, "unknown library source", http.StatusBadRequest)
		return
	}
	if msg := importApplyErr(result, err); msg != "" {
		row.Err = msg
	} else if err != nil {
		httpFeedError(w, err)
		return
	} else {
		row.Done = true
		s.afterFeedChange(ctx)
	}
	row.Icon = s.iconRefs(ctx)[row.ID]
	states, err := s.libraryStates(ctx, snap)
	if err != nil {
		httpFeedError(w, err)
		return
	}
	cov := libraryCoverage(snap, states)
	cov.OOB = true
	s.renderFragment(w, "ovSuggestionReply", suggestionReply{Row: row, Coverage: cov})
}

// handleLibraryRestore undeletes a removed source as it was, keeping any edits
// made to it. The stored id is found from the posted key, never taken from the
// request.
func (s *Web) handleLibraryRestore(w http.ResponseWriter, r *http.Request) {
	if !libraryForm(w, r) {
		return
	}
	ctx := r.Context()
	snap := s.library.Get(ctx)
	states, err := s.libraryStates(ctx, snap)
	if err != nil {
		httpFeedError(w, err)
		return
	}
	row, ok := states[r.PostFormValue("key")]
	if !ok {
		http.Error(w, "unknown library source", http.StatusBadRequest)
		return
	}
	if row.Status != store.PlanRemoved || row.ID == "" {
		s.writeLibraryBody(w, r, snap, nil, &importStatus{
			Err: row.Feed.Name + " is not among your removed sources any more.",
		})
		return
	}
	if err := s.db.RestoreFeed(ctx, row.ID); err != nil {
		httpFeedError(w, err)
		return
	}
	s.afterFeedChange(ctx)
	name := row.Feed.Name
	if f, err := s.db.FeedByID(ctx, row.ID); err == nil {
		name = f.Name
	}
	s.writeLibraryBody(w, r, snap, nil, &importStatus{Msg: "Restored " + name})
}

// libraryRefresh is the Try again reply: the section, and the nav group that
// gains its entries.
type libraryRefresh struct {
	Page *libraryPage
	Nav  libNav
}

// handleLibraryRefresh fetches the library again and repaints the section.
func (s *Web) handleLibraryRefresh(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	snap := s.library.Refresh(ctx)
	group := libraryGroupOf(r)
	page, err := s.libraryPageFor(ctx, snap, group)
	if errors.Is(err, errUnknownGroup) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		httpFeedError(w, err)
		return
	}
	nav := libNavFor(snap, "library", group)
	nav.OOB, nav.Fill = true, ""
	s.renderFragment(w, "libraryRefresh", libraryRefresh{Page: page, Nav: nav})
}

const (
	// libraryWaitDefault bounds how long the Library page waits for a library
	// that has not loaded yet; the page then shows its retry state.
	libraryWaitDefault = 1500 * time.Millisecond

	// libraryFillTries bounds how often a placeholder asks for its content.
	libraryFillTries = 6
)

// libraryPending is whether the Library parts of a page are still to come: the
// library is on and nothing has loaded yet.
func libraryPending(snap library.Snapshot) bool {
	return !snap.Off && !snap.Inert && !snap.Loaded
}

// libraryFillURL is the repaint a placeholder asks for. try counts the asks,
// this one included.
func libraryFillURL(part, section, group string, try int) string {
	q := url.Values{"part": {part}, "try": {strconv.Itoa(try)}}
	if section != "" {
		q.Set("section", section)
	}
	if group != "" {
		q.Set("group", group)
	}
	return "/sources/libfill?" + q.Encode()
}

// libraryFill is the answer to a placeholder's repaint: the Overview's Library
// sections once they can be built, or the placeholder again with the next try.
type libraryFill struct {
	Library *overviewLibrary
	Fill    string
}

// handleLibraryFill repaints a Library part of the Sources page that was drawn
// as a placeholder, from the cached library, never waiting for the network. A
// part still missing after libraryFillTries asks is left as the plain page draws
// it, so a library that never loads is not polled for.
func (s *Web) handleLibraryFill(w http.ResponseWriter, r *http.Request) {
	httplog.Quiet(r)
	q := r.URL.Query()
	try, _ := strconv.Atoi(q.Get("try"))
	try = min(max(try, 1), libraryFillTries)
	snap := s.library.Snapshot()
	pending := libraryPending(snap) && try < libraryFillTries
	switch q.Get("part") {
	case "nav":
		section := "other"
		if q.Get("section") == "library" {
			section = "library"
		}
		nav := libNavFor(snap, section, q.Get("group"))
		nav.Fill = ""
		if pending {
			nav.Fill = libraryFillURL("nav", section, q.Get("group"), try+1)
		}
		s.renderFragment(w, "sourcesLibNav", nav)
	case "overview":
		data, err := s.sourcesData(r.Context(), "", false)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		var fill libraryFill
		if data.Counts.Total > 0 {
			if fill.Library = s.overviewLibrary(
				r.Context(),
				snap,
				data.Rows,
				s.iconRefs(r.Context()),
			); fill.Library == nil &&
				pending {
				fill.Fill = libraryFillURL("overview", "", "", try+1)
			}
		}
		s.renderFragment(w, "ovLibraryFill", fill)
	default:
		http.Error(w, "unknown part", http.StatusBadRequest)
	}
}
