package web

import (
	"errors"
	"html/template"
	"io/fs"
	"net/http"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/DanielBlei/ai-searcher/internal/config"
	"github.com/DanielBlei/ai-searcher/internal/store"
)

// tmpl is parsed once at startup. The assets are embedded, so a parse failure is
// a build-time mistake — fail fast rather than handle it per request.
var tmpl = template.Must(template.New("web").ParseFS(
	templatesFS,
	"templates/*.html",
	"templates/partials/*.html",
))

// listLimit bounds how many rows the digest page pulls per render. The current
// page paginates this set client-side; real server-side paging is a follow-up.
const listLimit = 100

// Web renders the HTML frontend over the same store the JSON API uses.
type Web struct {
	db  *store.Store
	cfg *config.Config
}

// New returns a Web backed by db, using cfg for request defaults.
func New(db *store.Store, cfg *config.Config) *Web {
	return &Web{db: db, cfg: cfg}
}

// Routes returns the web handler: the digest page at "/" (exact) plus the
// embedded static assets at "/static/".
func (s *Web) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleDigest)
	mux.HandleFunc("POST /items/{id}/note", s.handleNote)

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
	Title           string
	Active          string
	Stats           statsData
	Sources         []string
	FilterSource    string
	FilterPublished string
	FilterCustom    bool
	FilterFrom      string
	FilterTo        string
	FilterSort      string
	Rows            []rowData
}

type statsData struct {
	UnreadToday   int
	AvgScore      float64
	SourcesActive int
}

// rowData is the per-item view model consumed by partials/row.html.
type rowData struct {
	ID       string // item id; scopes the per-row why/note radio group and field ids
	Time     string
	Title    string
	Link     string
	Score    int    // 0-10; drives the gauge bar width (--score)
	Tier     string // high | mid | low — gauge colour class
	GaugeBar string // bar glyphs (visually hidden; kept for the markup)
	Source   string
	Date     string
	Reason   string
	HasNote  bool
	Note     string
}

func (s *Web) handleDigest(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	published := q.Get("published")
	from, to := q.Get("from"), q.Get("to")
	sort := q.Get("sort")
	if sort == "" {
		sort = store.SortByScore
	}
	source := q.Get("source")

	after, before, custom := windowFor(published, from, to)
	if custom {
		published = ""
	}

	rows, err := s.db.List(r.Context(), store.ListFilter{
		Source: source,
		After:  after,
		Before: before,
		SortBy: sort,
		Limit:  listLimit,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
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
		Title:           "ai-searcher",
		Active:          "home",
		Stats:           s.stats(r, rows, len(counts)),
		Sources:         sources,
		FilterSource:    source,
		FilterPublished: published,
		FilterCustom:    custom,
		FilterFrom:      from,
		FilterTo:        to,
		FilterSort:      sort,
		Rows:            toRows(rows),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, "layout", data); err != nil {
		// Status is likely already written; log rather than double-write.
		log.Error().Err(err).Msg("render digest")
	}
}

// handleNote persists a user note for one item, then renders that item's row
// fragment back so htmx can swap it in place. The fragment comes back in view
// mode showing the just-saved note, so a later click on the note tab pulls the
// user's own text from the store rather than the placeholder.
func (s *Web) handleNote(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	note := r.FormValue("note")

	if err := s.db.UpdateUserState(r.Context(), id, store.UserPatch{UserNote: &note}); err != nil {
		if errors.Is(err, store.ErrItemNotFound) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	row, err := s.db.Get(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, "row", toRow(row)); err != nil {
		// Status is likely already written; log rather than double-write.
		log.Error().Err(err).Msg("render note row")
	}
}

// stats computes the dashboard tiles. UnreadToday is a dedicated count;
// AvgScore is the mean score over the rows currently shown; SourcesActive is
// the number of distinct sources in the store. (Rough v1 — refine later.)
func (s *Web) stats(r *http.Request, rows []store.ItemRow, sourcesActive int) statsData {
	st := statsData{SourcesActive: sourcesActive}

	unread, err := s.db.List(r.Context(), store.ListFilter{
		Status: store.StatusUnread,
		After:  startOfDay(time.Now()),
		Limit:  listLimit, // good enough for a count tile in v1
	})
	if err == nil {
		st.UnreadToday = len(unread)
	}

	var sum, n int
	for _, row := range rows {
		if sc, ok := score(row); ok {
			sum += sc
			n++
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
	sc, _ := score(row)
	rd := rowData{
		ID:       row.ID,
		Time:     timeOf(row.PublishedAt),
		Title:    row.Title,
		Link:     row.Link,
		Score:    sc,
		Tier:     tierOf(sc),
		GaugeBar: "",
		Source:   row.Source,
		Date:     dateOf(row.PublishedAt),
		Reason:   strOf(row.LLMScoreReason),
	}
	// A stored empty note reads the same as no note at all — both show the
	// "No note yet" placeholder and the "+ add" affordance.
	if row.UserNote != nil && *row.UserNote != "" {
		rd.HasNote = true
		rd.Note = *row.UserNote
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
