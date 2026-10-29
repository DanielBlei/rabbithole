// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/rs/zerolog/log"

	"github.com/DanielBlei/rabbithole/internal/store"
)

// maxBulkIDs caps how many sources one bulk request or selected export names.
const maxBulkIDs = 500

// Copy of the bulk replies.
const (
	bulkNothingCopy  = "Nothing to change."
	bulkNoTagCopy    = "Tag name is required."
	bulkGoneCopy     = "That can no longer be undone."
	undoStaleCopy    = "That changed since. Nothing to undo."
	bulkNoSourcesMsg = "No sources selected."
	bulkTooManyCopy  = "Too many sources selected (limit 500)."
)

// flashData is the toast marker a mutation response carries: a message and,
// for a change that can be taken back, the undo token and the comma-joined ids
// of the sources it covers.
type flashData struct {
	Msg  string
	Undo string
	IDs  string
}

// bulkAction is what a bulk operation needs to know beyond its name.
type bulkAction struct {
	op store.BulkOp
	// applies reports whether the action can change a row in this state; tag
	// is the normalised tag of a tag action.
	applies func(r feedRowData, tag string) bool
}

var bulkActions = map[store.BulkOp]bulkAction{
	// Enable and disable only touch rows whose resolved value changes, so a row
	// that already behaves that way keeps following the defaults.
	store.BulkEnable:  {store.BulkEnable, func(r feedRowData, _ string) bool { return !r.Deleted && !r.Enabled }},
	store.BulkDisable: {store.BulkDisable, func(r feedRowData, _ string) bool { return !r.Deleted && r.Enabled }},
	store.BulkRemove:  {store.BulkRemove, func(r feedRowData, _ string) bool { return !r.Deleted }},
	store.BulkRestore: {store.BulkRestore, func(r feedRowData, _ string) bool { return r.Deleted }},
	// A tag the source already shows, from the defaults, is not added to it.
	store.BulkAddTag: {store.BulkAddTag, func(r feedRowData, tag string) bool {
		return !r.Deleted && !slices.ContainsFunc(r.Tags, func(t string) bool { return strings.EqualFold(t, tag) })
	}},
	store.BulkRemoveTag: {store.BulkRemoveTag, func(r feedRowData, _ string) bool { return !r.Deleted }},
}

// parseBulkIDs splits a comma-joined id list, dropping blanks and repeats. ok
// is false for a list over maxBulkIDs.
func parseBulkIDs(raw string) (ids []string, ok bool) {
	for part := range strings.SplitSeq(raw, ",") {
		if id := strings.TrimSpace(part); id != "" && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids, len(ids) <= maxBulkIDs
}

// handleSourcesBulk applies one action to the selected sources in a single
// transaction and answers with the rows it changed, the page counts and a toast
// marker carrying the undo token. The ids are filtered against the sources as
// they are now, so a stale selection never acts on a row the action does not
// fit.
func (s *Web) handleSourcesBulk(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	op := store.BulkOp(r.PostFormValue("action"))
	action, ok := bulkActions[op]
	if !ok {
		http.Error(w, "Unknown bulk action: "+r.PostFormValue("action"), http.StatusBadRequest)
		return
	}
	ids, ok := parseBulkIDs(r.PostFormValue("ids"))
	if !ok {
		http.Error(w, bulkTooManyCopy, http.StatusBadRequest)
		return
	}
	if len(ids) == 0 {
		http.Error(w, bulkNoSourcesMsg, http.StatusBadRequest)
		return
	}
	var tag string
	if op == store.BulkAddTag || op == store.BulkRemoveTag {
		var err error
		if tag, err = store.BulkTag(r.PostFormValue("tag")); err != nil {
			http.Error(w, bulkNoTagCopy, http.StatusBadRequest)
			return
		}
	}

	data, err := s.sourcesData(ctx, "", false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	live := make(map[string]feedRowData, len(data.Rows))
	for _, row := range data.Rows {
		live[row.ID] = row
	}
	var apply []string
	for _, id := range ids {
		if row, ok := live[id]; ok && action.applies(row, tag) {
			apply = append(apply, id)
		}
	}

	var changed []store.FeedState
	if len(apply) > 0 {
		changed, err = s.runBulk(r, op, apply, tag)
		if err != nil {
			s.bulkError(w, err)
			return
		}
	}
	if len(changed) == 0 {
		s.writeFlash(w, flashData{Msg: bulkNothingCopy})
		return
	}
	s.afterFeedChange(ctx)
	s.writeBulkReply(w, r, stateIDs(changed), s.undoFlash(bulkMessage(op, tag, len(changed)), op, tag, changed))
}

// runBulk calls the store method for op.
func (s *Web) runBulk(r *http.Request, op store.BulkOp, ids []string, tag string) ([]store.FeedState, error) {
	ctx := r.Context()
	switch op {
	case store.BulkEnable:
		return s.db.SetFeedsEnabled(ctx, ids, true)
	case store.BulkDisable:
		return s.db.SetFeedsEnabled(ctx, ids, false)
	case store.BulkRemove:
		return s.db.SoftDeleteFeeds(ctx, ids)
	case store.BulkRestore:
		return s.db.RestoreFeeds(ctx, ids)
	case store.BulkAddTag:
		return s.db.AddFeedsTag(ctx, ids, tag)
	case store.BulkRemoveTag:
		return s.db.RemoveFeedsTag(ctx, ids, tag)
	}
	return nil, fmt.Errorf("unknown bulk action %q", op)
}

// bulkError maps a store failure: a tag that normalises away is the user's to
// fix, anything else is ours.
func (s *Web) bulkError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrFeedInvalid) {
		http.Error(w, bulkNoTagCopy, http.StatusBadRequest)
		return
	}
	httpFeedError(w, err)
}

// bulkMessage is the toast for a bulk that changed n sources.
func bulkMessage(op store.BulkOp, tag string, n int) string {
	count := plural(n, "source", "sources")
	switch op {
	case store.BulkEnable:
		return "Enabled " + count
	case store.BulkDisable:
		return "Disabled " + count
	case store.BulkRemove:
		return "Removed " + count
	case store.BulkRestore:
		return "Restored " + count
	case store.BulkAddTag:
		return `Added tag "` + tag + `" to ` + count
	default:
		return `Removed tag "` + tag + `" from ` + count
	}
}

// handleSourcesUndo reverses the change a toast offered. The token is spent
// whatever happens next. Rows edited since the change are left alone, and the
// toast says how many came back.
func (s *Web) handleSourcesUndo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	token := r.PathValue("token")
	entry, ok := s.undo.take(token, s.clock())
	if !ok {
		http.Error(w, bulkGoneCopy, http.StatusGone)
		return
	}
	applied, err := s.db.RevertFeeds(ctx, entry.op, entry.tag, entry.states)
	if err != nil {
		// A failure that is ours leaves the Undo usable; a bad tag never will be.
		if !errors.Is(err, store.ErrFeedInvalid) {
			s.undo.putBack(token, entry)
		}
		s.bulkError(w, err)
		return
	}
	if applied > 0 {
		s.afterFeedChange(ctx)
	}
	var flash flashData
	switch total := len(entry.states); {
	case applied == total:
	case total == 1:
		flash.Msg = undoStaleCopy
	default:
		flash.Msg = fmt.Sprintf("Undid %d of %d. The rest changed since.", applied, total)
	}
	s.writeBulkReply(w, r, stateIDs(entry.states), flash)
}

// writeBulkReply answers a bulk or an undo: each named row as an out-of-band
// replacement, the page counts, and the toast marker.
func (s *Web) writeBulkReply(w http.ResponseWriter, r *http.Request, ids []string, flash flashData) {
	data, err := s.sourcesData(r.Context(), "", true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for i := range data.Rows {
		data.Rows[i].OOB = slices.Contains(ids, data.Rows[i].ID)
	}
	data.Counts.OOB, data.Attention.OOB = true, true
	data.Flash = flash
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The settings page reads the feed count here instead of loading itself again.
	w.Header().Set(feedCountHeader, strconv.Itoa(data.Counts.Total))
	if err := feedTmpl.ExecuteTemplate(w, "sourceBulk", data); err != nil {
		log.Error().Err(err).Msg("render source bulk")
	}
}

// writeFlash answers with the toast marker alone.
func (s *Web) writeFlash(w http.ResponseWriter, flash flashData) {
	s.renderFragment(w, "sourceFlash", flash)
}

func stateIDs(states []store.FeedState) []string {
	ids := make([]string, len(states))
	for i, st := range states {
		ids[i] = st.ID
	}
	return ids
}
