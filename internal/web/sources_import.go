// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/rs/zerolog/log"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// planOrder is the order the legend lists the statuses in.
var planOrder = []store.PlanStatus{
	store.PlanNew, store.PlanSubscribed, store.PlanRemoved, store.PlanConflict, store.PlanInvalid,
}

var planLabels = map[store.PlanStatus]string{
	store.PlanNew:        "New",
	store.PlanSubscribed: "Subscribed",
	store.PlanRemoved:    "Removed",
	store.PlanConflict:   "Name taken",
	store.PlanInvalid:    "Invalid",
}

// importDialog is the import sheet: the chooser until there is a plan.
type importDialog struct {
	Plan *importPlan
}

// importPlan is what the preview shows. The apply form posts to ApplyTo and
// swaps its reply into ApplyTarget; Cancel fetches CancelGet into CancelTarget.
// Where the plan is drawn (a dialog, a page) decides those four.
type importPlan struct {
	Title    string
	Name     string
	YAML     string
	Rows     []planRowView
	Counts   []planCount
	Err      string // the file could not be planned
	ApplyErr string // the apply was refused

	ApplyTo, ApplyTarget    string
	CancelGet, CancelTarget string
}

type planCount struct {
	Status string
	Label  string
	N      int
}

// planRowView is one file entry as the preview draws it. New rows start ticked;
// removed rows ask first because the file may predate the removal.
type planRowView struct {
	store.PlanRow
	Label      string
	Selectable bool
	Checked    bool
}

type planTag struct {
	Name  string
	Color int
}

// Tags are the entry's tags with their palette index, the same hash the list uses.
func (r planRowView) Tags() []planTag {
	out := make([]planTag, len(r.Feed.Tags))
	for i, t := range r.Feed.Tags {
		out[i] = planTag{Name: t, Color: tagColor(t)}
	}
	return out
}

// ApplyLabel is the apply button's text for the rows ticked on arrival.
func (p *importPlan) ApplyLabel() string {
	return applyLabel(p.count(store.PlanNew), 0)
}

// ApplyOff is whether nothing is ticked on arrival.
func (p *importPlan) ApplyOff() bool {
	return p.count(store.PlanNew) == 0
}

func (p *importPlan) count(st store.PlanStatus) int {
	for _, c := range p.Counts {
		if c.Status == string(st) {
			return c.N
		}
	}
	return 0
}

// applyLabel mirrors the script's label for the ticked rows.
func applyLabel(add, restore int) string {
	switch {
	case add > 0 && restore > 0:
		return fmt.Sprintf("Add %d, restore %d", add, restore)
	case restore > 0:
		return fmt.Sprintf("Restore %d", restore)
	default:
		return fmt.Sprintf("Add %d", add)
	}
}

// importBanner words an apply for the banner above the list.
func importBanner(r store.ApplyResult) string {
	var msg string
	switch {
	case r.Added > 0 && r.Restored > 0:
		msg = fmt.Sprintf("Added %s, restored %d", countSources(r.Added), r.Restored)
	case r.Restored > 0:
		msg = fmt.Sprintf("Restored %d", r.Restored)
	default:
		msg = "Added " + countSources(r.Added)
	}
	if r.Skipped > 0 {
		msg += fmt.Sprintf(", skipped %d that changed since the preview", r.Skipped)
	}
	return msg
}

func countSources(n int) string {
	if n == 1 {
		return "1 source"
	}
	return fmt.Sprintf("%d sources", n)
}

// newImportPlan is a plan drawn in the import dialog.
func newImportPlan(name, text string) *importPlan {
	title := "Preview"
	if name != "" {
		title = "Import " + name
	}
	return &importPlan{
		Title: title, Name: name, YAML: text,
		ApplyTo: "/sources/import/apply", ApplyTarget: "#srcBody",
		CancelGet: "/sources/import", CancelTarget: "#modalTop",
	}
}

// planFile parses text and plans it against the stored feeds. A file that
// cannot be used comes back as a plan holding Err; only a store failure is an
// error.
func (s *Web) planFile(ctx context.Context, name, text string) (*importPlan, error) {
	p := newImportPlan(name, text)
	doc, err := config.ParseFeeds([]byte(text))
	if err != nil {
		p.Err = err.Error()
		return p, nil
	}
	if len(doc.Feeds) == 0 {
		where := "this file"
		if name != "" {
			where = name
		}
		p.Err = fmt.Sprintf(
			"No sources found in %s. Expected a sources: or feeds: list with a name and url for each.", where)
		return p, nil
	}
	rows, err := s.db.PlanImport(ctx, *doc)
	if err != nil {
		return nil, err
	}
	n := make(map[store.PlanStatus]int)
	for _, row := range rows {
		n[row.Status]++
		pick := row.Status == store.PlanNew || row.Status == store.PlanRemoved
		p.Rows = append(p.Rows, planRowView{
			PlanRow: row, Label: planLabels[row.Status],
			Selectable: pick, Checked: row.Status == store.PlanNew,
		})
	}
	for _, st := range planOrder {
		p.Counts = append(p.Counts, planCount{Status: string(st), Label: planLabels[st], N: n[st]})
	}
	return p, nil
}

// readImport reads the file to import: the multipart "file" part, or the "yaml"
// field. name is what to call it in the title. A non-empty msg is why it could
// not be read.
func readImport(w http.ResponseWriter, r *http.Request) (text, name, msg string) {
	r.Body = http.MaxBytesReader(w, r.Body, sourceImportLimit)
	var err error
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		err = r.ParseMultipartForm(sourceImportLimit)
	} else {
		err = r.ParseForm()
	}
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return "", "", "feeds file is too large (limit 1 MiB)"
		}
		return "", "", "could not read the uploaded file"
	}
	name = r.PostFormValue("name")
	if file, head, err := r.FormFile("file"); err == nil {
		defer func() { _ = file.Close() }()
		raw, err := io.ReadAll(file)
		if err != nil {
			return "", head.Filename, fmt.Sprintf("Could not read %s.", head.Filename)
		}
		return string(raw), head.Filename, ""
	}
	text = r.PostFormValue("yaml")
	if text == "" {
		return "", name, "choose a feeds YAML file to import"
	}
	return text, name, ""
}

// importApplyErr is the message to show beside the plan when an apply is
// refused but the file is still worth reviewing, or "" when err is anything else.
func importApplyErr(result store.ApplyResult, err error) string {
	switch {
	case errors.Is(err, store.ErrNothingToImport) && result.Skipped > 0:
		return "Everything you picked changed since the preview. Review the list and try again."
	case errors.Is(err, store.ErrNothingToImport):
		return "Nothing selected."
	case errors.Is(err, store.ErrFeedURLTaken), errors.Is(err, store.ErrFeedNameTaken):
		return "Something changed while importing. Review the list and try again."
	default:
		return ""
	}
}

func (s *Web) handleSourceImportDialog(w http.ResponseWriter, _ *http.Request) {
	s.renderFragment(w, "sourceImport", importDialog{})
}

// renderImportError shows why the file could not be used, in the dialog's own
// error state. The status stays 200 so the swap lands.
func (s *Web) renderImportError(w http.ResponseWriter, name, msg string) {
	p := newImportPlan(name, "")
	p.Err = msg
	s.renderFragment(w, "sourceImport", importDialog{Plan: p})
}

// handleSourceImportPreview plans a feeds file against the stored set and
// writes nothing.
func (s *Web) handleSourceImportPreview(w http.ResponseWriter, r *http.Request) {
	text, name, msg := readImport(w, r)
	if msg != "" {
		s.renderImportError(w, name, msg)
		return
	}
	p, err := s.planFile(r.Context(), name, text)
	if err != nil {
		httpFeedError(w, err)
		return
	}
	s.renderFragment(w, "sourceImport", importDialog{Plan: p})
}

// handleSourceImportApply adds or restores the ticked entries. The file is
// carried back in the form and planned again, so a row that changed since the
// preview is skipped, not applied blind.
func (s *Web) handleSourceImportApply(w http.ResponseWriter, r *http.Request) {
	text, name, msg := readImport(w, r)
	retarget := func() {
		w.Header().Set("HX-Retarget", "#modalTop")
		w.Header().Set("HX-Reswap", "innerHTML")
	}
	if msg != "" {
		retarget()
		s.renderImportError(w, name, msg)
		return
	}
	doc, err := config.ParseFeeds([]byte(text))
	if err != nil {
		retarget()
		s.renderImportError(w, name, err.Error())
		return
	}
	pick := make(map[string]bool)
	for _, key := range r.PostForm["pick"] {
		pick[key] = true
	}
	result, err := s.db.ApplyImport(r.Context(), *doc, pick)
	if applyErr := importApplyErr(result, err); applyErr != "" {
		p, perr := s.planFile(r.Context(), name, text)
		if perr != nil {
			httpFeedError(w, perr)
			return
		}
		p.ApplyErr = applyErr
		retarget()
		s.renderFragment(w, "sourceImport", importDialog{Plan: p})
		return
	}
	if err != nil {
		httpFeedError(w, err)
		return
	}
	s.afterFeedChange(r.Context())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if _, err := w.Write(
		[]byte(`<span id="sourcesImportDone" hx-swap-oob="innerHTML:#modalTop"></span>`),
	); err != nil {
		log.Error().Err(err).Msg("write import response")
		return
	}
	s.writeSourcesBodyWith(w, r, "", nil, &importStatus{Msg: importBanner(result)})
}
