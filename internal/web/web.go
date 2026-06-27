package web

import (
	"errors"
	"html/template"
	"io/fs"
	"net/http"
	"os/user"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/DanielBlei/rabbithole/internal/config"
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

// Web renders the HTML frontend over the same store the JSON API uses.
type Web struct {
	db      *store.Store
	cfg     *config.Config
	addr    string // the serve listen address, shown in the faux shell prompt
	user    string // shell-prompt name: cfg.User, or the OS user when blank
	cfgPath string // config file path, read on demand by the config viewer
}

// New returns a Web backed by db, using cfg for request defaults. addr is the
// listen address the serve command bound, surfaced in the page's shell prompt.
// cfgPath is the config file the viewer reads and displays read-only.
func New(db *store.Store, cfg *config.Config, addr, cfgPath string) *Web {
	return &Web{db: db, cfg: cfg, addr: addr, user: promptUser(cfg.User), cfgPath: cfgPath}
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
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	return mux
}

// pageData is the top-level template model passed to layout.html.
type pageData struct {
	Title              string
	Active             string
	PromptUser         string // shell-prompt user in the pane title bar
	ServeCmd           string // the command the prompt shows running, incl. the real --addr
	Stats              statsData
	Sources            []string
	FilterSource       string
	FilterPublished    string
	FilterCustom       bool
	FilterFrom         string
	FilterTo           string
	FilterSort         string
	FilterShowUnread   bool
	FilterShowSeen     bool
	FilterShowHidden   bool
	FilterShowBookmark bool
	Rows               []rowData
}

type statsData struct {
	Available     int
	HighSignal    int // count of shown items scoring >=7 (the "read these" set)
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
}

func (s *Web) handleFeed(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	published := q.Get("published")
	from, to := q.Get("from"), q.Get("to")
	sort := q.Get("sort")
	if sort == "" {
		sort = store.SortByLatest
	}
	source := q.Get("source")

	after, before, custom := windowFor(published, from, to)
	if custom {
		published = ""
	}

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
			Source:     source,
			After:      after,
			Before:     before,
			Bookmarked: onlyBookmark,
			SortBy:     sort,
			Limit:      listLimit,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		rows = got
	}

	counts, err := s.db.Sources(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sources := make([]string, len(counts))
	for i, c := range counts {
		sources[i] = c.Source
	}

	data := pageData{
		Title:              "The Rabbit Hole",
		Active:             "feed",
		PromptUser:         s.user,
		ServeCmd:           "go run . serve --addr " + s.addr,
		Stats:              s.stats(r, rows, len(counts), store.ListFilter{Source: source, After: after, Before: before, Bookmarked: onlyBookmark}),
		Sources:            sources,
		FilterSource:       source,
		FilterPublished:    published,
		FilterCustom:       custom,
		FilterFrom:         from,
		FilterTo:           to,
		FilterSort:         sort,
		FilterShowUnread:   showUnread,
		FilterShowSeen:     showSeen,
		FilterShowHidden:   showHidden,
		FilterShowBookmark: onlyBookmark,
		Rows:               toRows(rows),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := feedTmpl.ExecuteTemplate(w, "layout", data); err != nil {
		// Status is likely already written; log rather than double-write.
		log.Error().Err(err).Msg("render feed")
	}
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
	st := statsData{SourcesActive: sourcesActive}

	if n, err := s.db.Count(r.Context(), countFilter); err == nil {
		st.Available = n
	}

	var sum, n int
	for _, row := range rows {
		if sc, ok := score(row); ok {
			sum += sc
			n++
			if sc >= 7 { // high tier — see tierOf
				st.HighSignal++
			}
		}
	}
	if n > 0 {
		st.AvgScore = float64(sum) / float64(n)
	}
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
	case score >= 7:
		return "high"
	case score >= 4:
		return "mid"
	default:
		return "low"
	}
}

// windowFor maps the published-window filter (today/7d/14d/30d or a custom
// from/to range) onto a created_at [after, before) bound. A custom range wins
// over the pills; an unset window leaves both sides open (all items).
func windowFor(published, from, to string) (after, before time.Time, custom bool) {
	if from != "" || to != "" {
		if from != "" {
			after, _ = time.Parse("2006-01-02", from)
		}
		if to != "" {
			if t, err := time.Parse("2006-01-02", to); err == nil {
				before = t.AddDate(0, 0, 1) // make the end date inclusive
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
