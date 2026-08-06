// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"errors"
	"html/template"
	"io/fs"
	"net/http"
	"os/user"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/httplog"
	"github.com/DanielBlei/rabbithole/internal/ingest"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// Each page gets its own template set — layout + the shared partials + that one
// page file — rather than one global set. The pages both define "content" and
// "scripts" blocks for the layout to fill; parsing them into a single set would
// make the second page's blocks clobber the first's. Per-page sets keep them
// apart. The partials are uniquely named, so loading all of them into every set
// is harmless (a page just ignores the ones it doesn't use). Parsed once at
// startup; a parse failure is a build-time mistake, so fail fast.
var (
	feedTmpl = pageTmpl("feed.html")
	mazeTmpl = pageTmpl("maze.html")
)

// pageTmpl builds the template set for one page: the shared layout and partials
// plus the named page file under templates/.
func pageTmpl(page string) *template.Template {
	return template.Must(template.New(page).ParseFS(
		templatesFS,
		"templates/layout.html",
		"templates/partials/*.html",
		"templates/"+page,
	))
}

// listLimit bounds how many rows the digest page pulls per render. The current
// page paginates this set client-side; real server-side paging is a follow-up.
const listLimit = 100

// maxSearchLen caps the search control's text. Long enough for a headline,
// short enough that the query string stays a link someone can share.
const maxSearchLen = 120

// Score tiers: high signal at 7 and above, mid at 4, low below.
const (
	highSignalScore = 7
	midSignalScore  = 4
)

// Web renders the HTML frontend over the same store the JSON API uses.
type Web struct {
	db      *store.Store
	cfg     *config.Config
	addr    string // the serve listen address, shown in the faux shell prompt
	user    string // shell-prompt name: cfg.User, or the OS user when blank
	cfgPath string // config file path, read on demand by the config viewer
	ing     *ingest.Manager
}

// New returns a Web backed by db, using cfg for request defaults. addr is the
// listen address the serve command bound, surfaced in the page's shell prompt.
// cfgPath is the config file the viewer reads and displays read-only. ing owns
// the in-process ingest runs the UI triggers and watches.
func New(db *store.Store, cfg *config.Config, addr, cfgPath string, ing *ingest.Manager) *Web {
	return &Web{db: db, cfg: cfg, addr: addr, user: promptUser(cfg.User), cfgPath: cfgPath, ing: ing}
}

// promptUser picks the shell-prompt name: the configured user, or the OS login
// name when config leaves it blank. If even that can't be read, it stays empty
// and the prompt renders as just "@rabbithole".
func promptUser(configured string) string {
	if configured != "" {
		return configured
	}
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return ""
}

// Routes returns the web handler: the digest ("feed") page — the landing page,
// served at both "/" and "/feed" — the Maze day-to-day tools page at "/maze",
// their htmx mutation routes, and the embedded static assets at "/static/".
func (s *Web) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleFeed)
	mux.HandleFunc("GET /feed", s.handleFeed)
	mux.HandleFunc("GET /maze", s.handleMaze)
	mux.HandleFunc("GET /config", s.handleConfig)
	mux.HandleFunc("GET /sources", s.handleSources)
	mux.HandleFunc("GET /sources/new", s.handleSourceNew)
	mux.HandleFunc("GET /sources/defaults", s.handleSourceDefaults)
	mux.HandleFunc("GET /sources/export", s.handleSourceExport)
	mux.HandleFunc("GET /sources/{id}", s.handleSourceSelect)
	mux.HandleFunc("POST /sources", s.handleSourceAdd)
	mux.HandleFunc("POST /sources/defaults", s.handleSourceSaveDefaults)
	mux.HandleFunc("POST /sources/{id}", s.handleSourceSave)
	mux.HandleFunc("POST /sources/{id}/enabled", s.handleSourceEnabled)
	mux.HandleFunc("POST /sources/{id}/restore", s.handleSourceRestore)
	mux.HandleFunc("GET /sources/{id}/confirm-delete", s.handleSourceConfirmDelete)
	mux.HandleFunc("DELETE /sources/{id}", s.handleSourceDelete)
	mux.HandleFunc("GET /ingest", s.handleIngest)
	mux.HandleFunc("GET /ingest/status", s.handleIngestStatus)
	mux.HandleFunc("GET /ingest/chrome", s.handleIngestChrome)
	mux.HandleFunc("GET /ingest/runs/{id}/log", s.handleIngestRunLog)
	mux.HandleFunc("POST /ingest/run", s.handleIngestRun)
	mux.HandleFunc("POST /ingest/cancel", s.handleIngestCancel)
	mux.HandleFunc("POST /items/{id}/note", s.handleNote)
	mux.HandleFunc("POST /items/{id}/seen", s.handleSeen)
	mux.HandleFunc("POST /items/{id}/hide", s.handleHide)
	mux.HandleFunc("POST /items/{id}/bookmark", s.handleBookmark)
	mux.HandleFunc("POST /todos", s.handleAddTodo)
	mux.HandleFunc("POST /todos/{id}/toggle", s.handleToggleTodo)
	mux.HandleFunc("POST /todos/{id}/tags", s.handleSetTodoTags)
	mux.HandleFunc("DELETE /todos/{id}", s.handleDeleteTodo)
	mux.HandleFunc("POST /ideas", s.handleAddIdea)
	mux.HandleFunc("POST /ideas/reorder", s.handleReorderIdeas)
	mux.HandleFunc("POST /ideas/{id}", s.handleUpdateIdea)
	mux.HandleFunc("DELETE /ideas/{id}", s.handleDeleteIdea)

	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		// staticFS is embedded with a known "static" dir — can't happen.
		panic(err)
	}
	// Assets are quiet: one line per file on every cold load, none of it signal.
	mux.Handle("GET /static/", httplog.QuietHandler(
		http.StripPrefix("/static/", http.FileServer(http.FS(static)))))
	return mux
}

// pageData is the top-level template model passed to layout.html.
type pageData struct {
	Title              string
	Active             string
	Chrome             chromeData // shared topbar/rail state (ingest chip + rail action)
	PromptUser         string     // shell-prompt user in the pane title bar
	ServeCmd           string     // the command the prompt shows running, incl. the real --addr
	Stats              statsData
	Sources            []pickChip // every source in the store, with the picked ones flagged
	SourcesPick        pickState  // what the source button reads while closed
	FilterPublished    string
	FilterPublishedTxt string // the window named for the filter's closed button
	FilterCustom       bool
	FilterFrom         string
	FilterTo           string
	FilterSort         string
	FilterShowUnread   bool
	FilterShowSeen     bool
	FilterShowHidden   bool
	FilterShowBookmark bool
	FilterSearch       string     // the search control's text, echoed back into its field and button
	Tags               []pickChip // every tag in the store, with the picked ones flagged
	// Tags and View name themselves on the button rather than wearing their
	// value, so their pickState is what the tooltip lists and what decides
	// whether they count towards Narrowed — not a label.
	TagsPick pickState
	ViewPick pickState
	SortPick pickState // what the sort button reads while closed
	// Narrowed is whether anything in the bar is filtering, which is the whole
	// question the "clear" button answers — so it decides whether one renders.
	Narrowed  bool
	ListLimit int // listLimit, so the pager can say when the set below is truncated
	Rows      []rowData
	Empty     emptyData // the zero-state, filled only when Rows is empty
}

// pickChip is one option in a multi-select filter menu.
type pickChip struct {
	Value string
	On    bool
}

// pickState is what a filter's closed button wears: the selection's first
// value, a "+N" for the rest, the full list as a tooltip, and whether the
// filter is narrowing anything (which lights the amber dot). Every control in
// the bar carries one, so they all read the same way shut.
type pickState struct {
	Label string
	More  string
	Title string
	On    bool
}

// emptyData drives the zero-state the pane renders when no rows come back. Kind
// separates the three reasons a feed can be empty, because they want different
// words and a different next action: nothing ingested yet (never), a run that
// brought nothing home (dry), or a view that filters everything out (filtered).
type emptyData struct {
	Kind string // never | dry | filtered
	Cmd  string // the faux shell line that came back with nothing
}

const (
	emptyNever    = "never"
	emptyDry      = "dry"
	emptyFiltered = "filtered"
)

type statsData struct {
	Available     int
	HighSignal    int // count of shown items in the high tier (the "read these" set)
	HighThreshold int // the tier bound, for the tile label and the JS recalc
	AvgScore      float64
	SourcesActive int
}

// rowData is the per-item view model consumed by partials/row.html.
type rowData struct {
	ID         string // item id; scopes the per-row why/note radio group and field ids
	Time       string
	Title      string
	Link       string
	Score      int    // 0-10; drives the gauge bar width (--score)
	Scored     bool   // whether the item has any score; gates data-score for the live avg recalc
	Tier       string // high | mid | low — gauge colour class
	GaugeBar   string // bar glyphs (visually hidden; kept for the markup)
	Source     string
	Date       string
	Reason     string        // raw markdown source
	ReasonHTML template.HTML // Reason rendered to sanitised HTML for display
	// LLM-specific attribution for the "why" footnote, kept separate from Score
	// (which is the effective score — user's rating wins over the model's).
	HasLLMScore bool
	LLMScore    int
	ScoreModel  string
	HasNote     bool
	Note        string        // raw markdown source (also the textarea value)
	NoteHTML    template.HTML // Note rendered to sanitised HTML for the view
	// Triage state, derived from the item's Status: Seen == read, Hidden ==
	// skipped. Read-only here; the seen/hide mutation routes are a follow-up.
	Seen   bool
	Hidden bool
	// Bookmarked is the "keep for later" flag — orthogonal to triage status,
	// toggled via the bookmark route, and the basis for the future library page.
	Bookmarked bool
	// TagsAttr is the source feed's tags as recorded, comma-joined — the row's
	// data-tags, which the client-side tag filter matches against.
	TagsAttr string
}

func (s *Web) handleFeed(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	published := q.Get("published")
	from, to := q.Get("from"), q.Get("to")
	sort := q.Get("sort")
	if sort == "" {
		sort = store.SortByLatest
	}
	after, before, custom := windowFor(published, from, to)
	if custom {
		published = ""
	}

	// The search control's text, matched against title, source and tags.
	// Trimmed (a trailing space would hide every row) and capped, so a stray
	// paste doesn't end up as the URL.
	search := strings.TrimSpace(q.Get("search"))
	if runes := []rune(search); len(runes) > maxSearchLen {
		search = string(runes[:maxSearchLen])
	}

	// Source and tag chips: multi-select, OR within each set, AND across them.
	// Repeated params rather than one comma-joined value, so a feed name with a
	// comma in it stays one name.
	pickedSources := cleanPicks(q["source"])
	pickedTags := cleanPicks(q["tag"])

	// Status visibility is unit-based multiselect: the Unread/Seen/Hidden chips
	// each toggle one status independently (rather than unread-always + "show
	// also" extras). "view" is a hidden marker the filter form always submits,
	// so an unchecked Unread box reads as "hide unread" instead of being
	// indistinguishable from a bare GET / — which defaults to unread-only.
	submitted := q.Get("view") == "1"
	showUnread := q.Get("unread") == "1"
	showSeen := q.Get("seen") == "1"
	showHidden := q.Get("hidden") == "1"
	if !submitted {
		showUnread = true // default landing view: unread only
	}
	// Bookmarked is an orthogonal AND-filter, independent of the status set. The
	// chip auto-selects all three statuses client-side so the whole library
	// shows on entry; the user then narrows by unchecking a status.
	onlyBookmark := q.Get("bookmarked") == "1"

	var statuses []string
	if showUnread {
		statuses = append(statuses, store.StatusUnread)
	}
	if showSeen {
		statuses = append(statuses, store.StatusRead)
	}
	if showHidden {
		statuses = append(statuses, store.StatusSkipped)
	}

	// Every status chip cleared → nothing to show; skip the query rather than
	// pass an empty Statuses set, which List reads as "no status filter" (all).
	var rows []store.ItemRow
	if len(statuses) > 0 {
		got, err := s.db.List(r.Context(), store.ListFilter{
			Statuses:   statuses,
			Sources:    pickedSources,
			Tags:       pickedTags,
			After:      after,
			Before:     before,
			Bookmarked: onlyBookmark,
			Search:     search,
			SortBy:     sort,
			Limit:      listLimit,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		rows = got
	}

	// The chips offer what the store holds, not what this render returned:
	// options drawn from the rows would vanish as soon as you picked one, and
	// there'd be no way back to the feed you just filtered out.
	counts, err := s.db.Sources(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tagOptions, err := s.db.Tags(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	items := toRows(rows)
	chrome := s.chrome(r.Context())
	data := pageData{
		Title:      "The Rabbit Hole",
		Active:     "feed",
		Chrome:     chrome,
		PromptUser: s.user,
		ServeCmd:   "go run . serve --addr " + s.addr,
		Stats: s.stats(
			r,
			rows,
			len(counts),
			// Every narrowing filter belongs in the pool count: "available" is
			// how many items the bar's selection matches, which is what the
			// pager's "first 100 of N" is claiming.
			store.ListFilter{
				Sources:    pickedSources,
				Tags:       pickedTags,
				After:      after,
				Before:     before,
				Bookmarked: onlyBookmark,
				Search:     search,
			},
		),
		Sources:            pickChips(sourceNames(counts), pickedSources),
		SourcesPick:        worn(pickedSources, "All"),
		FilterPublished:    published,
		FilterPublishedTxt: publishedLabel(published, custom),
		FilterCustom:       custom,
		FilterFrom:         from,
		FilterTo:           to,
		FilterSort:         sort,
		FilterShowUnread:   showUnread,
		FilterShowSeen:     showSeen,
		FilterShowHidden:   showHidden,
		FilterShowBookmark: onlyBookmark,
		FilterSearch:       search,
		Tags:               pickChips(tagOptions, pickedTags),
		TagsPick:           worn(pickedTags, "All"),
		ListLimit:          listLimit,
		Rows:               items,
	}
	data.ViewPick = wornView(showUnread, showSeen, showHidden, onlyBookmark)
	// Sort always has exactly one value, so it wears it plainly, spelled the way
	// its own chips spell it; latest is the default and so isn't a narrowing.
	data.SortPick = pickState{Label: sortLabel(sort), On: sort != store.SortByLatest}
	data.Narrowed = data.SourcesPick.On || data.TagsPick.On || data.ViewPick.On ||
		published != "" || custom || search != "" || sort != store.SortByLatest
	if len(items) == 0 {
		data.Empty = s.emptyState(r.Context(), chrome.IngNever, data.cmd())
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := feedTmpl.ExecuteTemplate(w, "layout", data); err != nil {
		// Status is likely already written; log rather than double-write.
		log.Error().Err(err).Msg("render feed")
	}
}

// emptyState explains an empty feed. The store-wide count runs on this path
// only, so a normal render is unchanged. Items in the store mean the active
// filters are what's hiding them; an empty store is either a feed nothing has
// ever been ingested into, or a run that came home with nothing. A failed count
// degrades to the filtered wording, which advises nothing.
func (s *Web) emptyState(ctx context.Context, neverIngested bool, cmd string) emptyData {
	e := emptyData{Kind: emptyFiltered, Cmd: cmd}
	n, err := s.db.Count(ctx, store.ListFilter{})
	if err != nil {
		log.Warn().Err(err).Msg("counting items for the feed zero-state")
		return e
	}
	if n == 0 {
		e.Kind = emptyDry
		if neverIngested {
			e.Kind = emptyNever
		}
	}
	return e
}

// cmd renders the active filters as a shell line in the filter bar's own
// vocabulary. The zero-state shows it as the command that came back with
// nothing, so an empty page says which view is empty.
func (d pageData) cmd() string {
	cmd := "rabbithole feed"
	for _, f := range []struct {
		on   bool
		flag string
	}{
		{d.FilterShowUnread, "--unread"},
		{d.FilterShowSeen, "--seen"},
		{d.FilterShowHidden, "--hidden"},
		{d.FilterShowBookmark, "--bookmarked"},
	} {
		if f.on {
			cmd += " " + f.flag
		}
	}
	// Every status chip cleared is itself the reason the page is empty — say so
	// rather than rendering a bare command that looks like it should have worked.
	if !d.FilterShowUnread && !d.FilterShowSeen && !d.FilterShowHidden {
		cmd += " --status none"
	}
	switch {
	case d.FilterCustom:
		if d.FilterFrom != "" {
			cmd += " --from " + d.FilterFrom
		}
		if d.FilterTo != "" {
			cmd += " --to " + d.FilterTo
		}
	case d.FilterPublished != "":
		cmd += " --published " + d.FilterPublished
	}
	// Repeated flags for the multi-selects, the way the query string repeats
	// them. Quoted, since feed names and the search text carry spaces.
	for _, chip := range d.Sources {
		if chip.On {
			cmd += " --source " + strconv.Quote(chip.Value)
		}
	}
	for _, chip := range d.Tags {
		if chip.On {
			cmd += " --tag " + strconv.Quote(chip.Value)
		}
	}
	if d.FilterSearch != "" {
		cmd += " --search " + strconv.Quote(d.FilterSearch)
	}
	return cmd
}

// httpStoreError maps a store error to an HTTP response: a missing item is a
// 404, anything else a 500.
func httpStoreError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrItemNotFound) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

// handleNote persists a user note for one item, then renders that item's row
// fragment back so htmx can swap it in place. The fragment comes back in view
// mode showing the just-saved note, so a later click on the note tab pulls the
// user's own text from the store rather than the placeholder.
func (s *Web) handleNote(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	note := r.FormValue("note")

	if err := s.db.UpdateUserState(r.Context(), id, store.UserPatch{UserNote: &note}); err != nil {
		httpStoreError(w, err)
		return
	}

	row, err := s.db.Get(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := feedTmpl.ExecuteTemplate(w, "row", toRow(row)); err != nil {
		// Status is likely already written; log rather than double-write.
		log.Error().Err(err).Msg("render note row")
	}
}

// handleSeen toggles an item between read (seen) and unread; handleHide toggles
// between skipped (hidden) and unread. Both re-render the row fragment so htmx
// can swap the new state in place.
func (s *Web) handleSeen(w http.ResponseWriter, r *http.Request) {
	s.toggleStatus(w, r, store.StatusRead)
}

func (s *Web) handleHide(w http.ResponseWriter, r *http.Request) {
	s.toggleStatus(w, r, store.StatusSkipped)
}

// toggleStatus flips the item's status: if it already equals target, it returns
// to unread (so the same control un-toggles); otherwise it's set to target.
// seen and hide are mutually exclusive states off the unread baseline — setting
// one clears the other. The re-rendered row shows the new state.
func (s *Web) toggleStatus(w http.ResponseWriter, r *http.Request, target string) {
	id := r.PathValue("id")

	cur, err := s.db.Get(r.Context(), id)
	if err != nil {
		httpStoreError(w, err)
		return
	}

	next := target
	if cur.Status == target {
		next = store.StatusUnread
	}
	if err := s.db.UpdateUserState(r.Context(), id, store.UserPatch{Status: &next}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Only status changed, so render the in-memory row rather than re-fetching.
	cur.Status = next
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := feedTmpl.ExecuteTemplate(w, "row", toRow(cur)); err != nil {
		// Status is likely already written; log rather than double-write.
		log.Error().Err(err).Msg("render toggle row")
	}
}

// handleBookmark toggles an item's "keep for later" flag, then re-renders the
// row fragment so htmx can swap the new state (the gold corner fold) in place.
// Bookmarking is orthogonal to triage status, so unlike toggleStatus it leaves
// read/skip alone — it only flips bookmarked.
func (s *Web) handleBookmark(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	cur, err := s.db.Get(r.Context(), id)
	if err != nil {
		httpStoreError(w, err)
		return
	}

	next := !cur.Bookmarked
	if err := s.db.UpdateUserState(r.Context(), id, store.UserPatch{Bookmarked: &next}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Only the bookmark flag changed, so render the in-memory row.
	cur.Bookmarked = next
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := feedTmpl.ExecuteTemplate(w, "row", toRow(cur)); err != nil {
		// Status is likely already written; log rather than double-write.
		log.Error().Err(err).Msg("render bookmark row")
	}
}

// stats computes the dashboard tiles. UnreadToday is a dedicated count;
// AvgScore is the mean score over the rows currently shown; SourcesActive is
// the number of distinct sources in the store. Available is the total item
// count under the active source/date selection (pool stat via Store.Count,
// passed in as countFilter), regardless of triage status. (Rough v1 — refine
// later.)
func (s *Web) stats(r *http.Request, rows []store.ItemRow, sourcesActive int, countFilter store.ListFilter) statsData {
	st := statsData{SourcesActive: sourcesActive, HighThreshold: highSignalScore}

	if n, err := s.db.Count(r.Context(), countFilter); err == nil {
		st.Available = n
	}

	var sum, n int
	for _, row := range rows {
		if sc, ok := score(row); ok {
			sum += sc
			n++
			if sc >= highSignalScore {
				st.HighSignal++
			}
		}
	}
	if n > 0 {
		st.AvgScore = float64(sum) / float64(n)
	}
	return st
}

// maxPicks bounds how many values one multi-select filter accepts, so a
// hand-written URL can't turn a menu of chips into a thousand-term query.
const maxPicks = 50

// cleanPicks takes a repeated query param and returns the values worth
// filtering on: trimmed, blanks and duplicates dropped, capped.
func cleanPicks(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		if out = append(out, v); len(out) == maxPicks {
			break
		}
	}
	return out
}

func sourceNames(counts []store.SourceCount) []string {
	out := make([]string, len(counts))
	for i, c := range counts {
		out[i] = c.Source
	}
	return out
}

// pickChips pairs a filter's options with the current selection, so the
// template can render each chip checked or not without a lookup of its own.
func pickChips(options, picked []string) []pickChip {
	on := make(map[string]bool, len(picked))
	for _, p := range picked {
		on[p] = true
	}
	chips := make([]pickChip, len(options))
	for i, opt := range options {
		chips[i] = pickChip{Value: opt, On: on[opt]}
	}
	return chips
}

// worn builds a filter's closed-button state. The first pick is shown and the
// rest become a "+N" in its own element, so ellipsising a long name can never
// eat the count; the full list is the button's tooltip. empty is what the
// button reads when nothing is picked — "All", the same word the menu's
// unnarrowed chip uses.
func worn(picked []string, empty string) pickState {
	st := pickState{Label: empty, On: len(picked) > 0}
	if len(picked) == 0 {
		return st
	}
	st.Label = picked[0]
	if len(picked) > 1 {
		st.More = "+" + strconv.Itoa(len(picked)-1)
	}
	st.Title = strings.Join(picked, ", ")
	return st
}

// wornView is the View menu's version of worn: the statuses on show, with
// "saved" first when the bookmark filter is on. Unread-only is the landing
// view, so it wears its name without counting as narrowed.
func wornView(unread, seen, hidden, bookmarked bool) pickState {
	var picked []string
	if bookmarked {
		picked = append(picked, "saved")
	}
	for _, unit := range []struct {
		on   bool
		name string
	}{{unread, "unread"}, {seen, "seen"}, {hidden, "hidden"}} {
		if unit.on {
			picked = append(picked, unit.name)
		}
	}
	st := worn(picked, "none")
	st.On = !(unread && !seen && !hidden && !bookmarked)
	return st
}

func toRows(rows []store.ItemRow) []rowData {
	out := make([]rowData, len(rows))
	for i, row := range rows {
		out[i] = toRow(row)
	}
	return out
}

// toRow builds the view model for a single item. Used both for the full page
// render and for the row fragment a mutation handler swaps back via htmx.
func toRow(row store.ItemRow) rowData {
	sc, scored := score(row)
	rd := rowData{
		ID:         row.ID,
		Time:       timeOf(row.PublishedAt),
		Title:      row.Title,
		Link:       row.Link,
		Score:      sc,
		Scored:     scored,
		Tier:       tierOf(sc),
		GaugeBar:   "",
		Source:     row.Source,
		Date:       dateOf(row.PublishedAt),
		Reason:     strOf(row.LLMScoreReason),
		Seen:       row.Status == store.StatusRead,
		Hidden:     row.Status == store.StatusSkipped,
		Bookmarked: row.Bookmarked,
		TagsAttr:   strings.Join(row.Tags, ","),
	}
	rd.ReasonHTML = renderMarkdown(rd.Reason)
	// The "why" footnote attributes the reason to the model's own score, not the
	// effective Score above — so the user can see "model said 8, I rated 3".
	if row.LLMScore != nil {
		rd.HasLLMScore = true
		rd.LLMScore = *row.LLMScore
		rd.ScoreModel = strOf(row.LLMScoreModel)
	}
	// A stored empty note reads the same as no note at all — both show the
	// "No note yet" placeholder and the "+ add" affordance.
	if row.UserNote != nil && *row.UserNote != "" {
		rd.HasNote = true
		rd.Note = *row.UserNote
		rd.NoteHTML = renderMarkdown(rd.Note)
	}
	return rd
}

// score returns the effective 0-10 score (user score wins over the model's) and
// whether the item was scored at all.
func score(row store.ItemRow) (int, bool) {
	switch {
	case row.UserScore != nil:
		return *row.UserScore, true
	case row.LLMScore != nil:
		return *row.LLMScore, true
	default:
		return 0, false
	}
}

func tierOf(score int) string {
	switch {
	case score >= highSignalScore:
		return "high"
	case score >= midSignalScore:
		return "mid"
	default:
		return "low"
	}
}

// windowFor maps the published-window filter (today/7d/14d/30d or a custom
// from/to range) onto a published [after, before) bound. A custom range wins
// over the pills; an unset window leaves both sides open (all items).
// Both paths resolve in local time — a picked date means the user's calendar
// day, and the store converts to UTC on the way in.
func windowFor(published, from, to string) (after, before time.Time, custom bool) {
	if from != "" || to != "" {
		// A date we can't read is ignored, leaving that end of the range open.
		if from != "" {
			if t, err := time.ParseInLocation("2006-01-02", from, time.Local); err == nil {
				after = t
			} else {
				log.Warn().Str("from", from).Msg("ignoring invalid from date")
			}
		}
		if to != "" {
			if t, err := time.ParseInLocation("2006-01-02", to, time.Local); err == nil {
				before = t.AddDate(0, 0, 1) // make the end date inclusive
			} else {
				log.Warn().Str("to", to).Msg("ignoring invalid to date")
			}
		}
		return after, before, true
	}

	now := time.Now()
	switch published {
	case "today":
		after = startOfDay(now)
	case "7d":
		after = now.AddDate(0, 0, -7)
	case "14d":
		after = now.AddDate(0, 0, -14)
	case "30d":
		after = now.AddDate(0, 0, -30)
	}
	return after, before, false
}

// publishedLabel names the active window on the filter's closed button. No
// window is "All" — the unnarrowed default, the same word Source and Tags use.
func publishedLabel(published string, custom bool) string {
	switch {
	case custom:
		return "custom"
	case published == "":
		return "All"
	default:
		return published
	}
}

// sortLabel names the active sort on the filter's closed button, spelled the
// way the chips inside the menu spell it rather than the way the store does.
func sortLabel(sort string) string {
	switch sort {
	case store.SortByOldest:
		return "Oldest"
	case store.SortByScore:
		return "Score"
	default:
		return "Latest"
	}
}

func startOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

func timeOf(t *time.Time) string {
	if t == nil {
		return "--:--"
	}
	return t.Format("15:04")
}

func dateOf(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format("2 Jan")
}

func strOf(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
