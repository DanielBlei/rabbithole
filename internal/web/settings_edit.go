// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/inference"
	"github.com/DanielBlei/rabbithole/internal/profilemgr"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// The editing half of the settings page. General, Scoring and Advanced are one
// form: it carries expected_version, the review and save endpoints rebuild the
// candidate config from it on the server, and one save writes one version.

const (
	maxSettingsBody = 256 << 10 // a form, or a pasted config file
	testTimeout     = 8 * time.Second
	savedNote       = "The next ingest run uses it."
	staleMessage    = "Settings changed elsewhere. Reload to see the latest."
)

// SetEnvFile gives the settings page the .env file: it reports where key
// variables came from and, unless noWrite is set, takes a pasted key.
func (s *Web) SetEnvFile(e *config.EnvFile, noWrite bool) {
	if e != nil {
		s.env = e
	}
	s.envFile, s.noEnvWrite = e, noWrite
}

func (s *Web) canPasteKey() bool { return s.envFile != nil && !s.noEnvWrite }

// editState is the active config as an edit starts from it.
type editState struct {
	active   store.ConfigVersion
	has      bool
	version  int           // 0 while nothing is saved
	base     config.Config // the canonical form: only what was set
	parseErr error         // the saved version could not be read; base is empty
}

func (s *Web) editState(ctx context.Context) (editState, error) {
	v, ok, err := s.db.ActiveConfig(ctx)
	if err != nil {
		return editState{}, err
	}
	st := editState{active: v, has: ok, version: v.Version}
	if !ok {
		return st, nil
	}
	st.base, st.parseErr = config.ParseStoredCanonical([]byte(v.Content), v.SchemaVersion)
	if st.parseErr != nil {
		st.base = config.Config{}
	}
	return st, nil
}

type editError struct {
	Field string `json:"field,omitempty"`
	Msg   string `json:"msg"`
}

// editReply is the JSON every editing endpoint answers with.
type editReply struct {
	OK       bool        `json:"ok"`
	Message  string      `json:"message,omitempty"`
	Version  int         `json:"version,omitempty"`
	NoOp     bool        `json:"noop,omitempty"`
	Conflict bool        `json:"conflict,omitempty"`
	Errors   []editError `json:"errors,omitempty"`
	Ref      string      `json:"ref,omitempty"`
	Chip     string      `json:"chip,omitempty"`
	Tone     string      `json:"tone,omitempty"`
	Millis   int64       `json:"ms,omitempty"`
}

func writeReply(w http.ResponseWriter, status int, r editReply) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(r)
}

func refuse(w http.ResponseWriter, status int, msg string) {
	writeReply(w, status, editReply{Message: msg})
}

func stale(w http.ResponseWriter) {
	writeReply(w, http.StatusConflict, editReply{Conflict: true, Message: staleMessage})
}

func problemsReply(w http.ResponseWriter, ps []fieldProblem) {
	r := editReply{Message: "Some settings need a look before they can be saved."}
	for _, p := range ps {
		r.Errors = append(r.Errors, editError{Field: p.Field, Msg: p.Msg})
	}
	writeReply(w, http.StatusUnprocessableEntity, r)
}

// readForm parses a bounded POST body.
func readForm(w http.ResponseWriter, r *http.Request) (url.Values, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxSettingsBody)
	if err := r.ParseForm(); err != nil {
		refuse(w, http.StatusBadRequest, "That request was too large or could not be read.")
		return nil, false
	}
	return r.PostForm, true
}

// candidate is the form turned into a config ready to save.
type candidate struct {
	st      editState
	draft   config.Config
	changes []changeRow
}

// buildCandidate reads the form against the active config. It answers the
// request itself and returns false when it cannot go on: a stale form, a saved
// config that cannot be read, or values that fail validation.
func (s *Web) buildCandidate(w http.ResponseWriter, r *http.Request) (candidate, url.Values, bool) {
	form, ok := readForm(w, r)
	if !ok {
		return candidate{}, nil, false
	}
	st, err := s.editState(r.Context())
	if err != nil {
		refuse(w, http.StatusInternalServerError, "Could not read the saved settings.")
		return candidate{}, nil, false
	}
	if exp, err := strconv.Atoi(form.Get("expected_version")); err != nil || exp != st.version {
		stale(w)
		return candidate{}, nil, false
	}
	if st.parseErr != nil {
		refuse(w, http.StatusUnprocessableEntity,
			"The saved configuration cannot be read. Import a fixed file or roll back to an earlier version.")
		return candidate{}, nil, false
	}
	draft, problems := draftFrom(st.base, form)
	if problems = checkDraft(&draft, problems); len(problems) > 0 {
		problemsReply(w, problems)
		return candidate{}, nil, false
	}
	return candidate{st: st, draft: draft, changes: configChanges(st.base, draft)}, form, true
}

// unchanged reports whether saving would write the config that is already active.
func (c candidate) unchanged() bool { return c.draft.Hash() == c.st.base.Hash() }

// changeRow is one setting that differs between the active config and the draft.
type changeRow struct{ Name, Old, New string }

func configChanges(old, draft config.Config) []changeRow {
	var out []changeRow
	for i := range fieldSpecs {
		f := &fieldSpecs[i]
		o, n := f.get(&old), f.get(&draft)
		if o == n {
			continue
		}
		out = append(out, changeRow{Name: f.Name, Old: shownValue(f, o, &old), New: shownValue(f, n, &draft)})
	}
	return out
}

// shownValue is a setting in words for the review: the value, or the default in
// force, or that the provider no longer reads it.
func shownValue(f *fieldSpec, raw string, c *config.Config) string {
	provider := providerOf(c.Inference)
	if !f.readsField(provider) {
		return "Not used"
	}
	text := raw
	if raw == "" {
		text = defaultText(f, provider)
	}
	switch {
	case f.Key == keyProvider:
		text = providerLabel(text)
	case f.Kind == kBool:
		text = onOff(text == "true")
	case f.Kind == kSys:
		switch text {
		case "":
			text = "Built-in"
		case sysNone:
			text = "None"
		default:
			text = "Custom"
		}
	}
	if raw == "" {
		if text == "" {
			return "Default"
		}
		return "Default (" + text + ")"
	}
	return text
}

func onOff(b bool) string {
	if b {
		return "On"
	}
	return "Off"
}

// hostMove reports a change of server address while an API key reference is set
// both before and after: the key already in use would start going to a different
// server.
func hostMove(old, draft config.Config) (from, to string, moved bool) {
	if !hostSpec.readsField(providerOf(old.Inference)) || !hostSpec.readsField(providerOf(draft.Inference)) {
		return "", "", false
	}
	from, to = effectiveText(hostSpec, &old), effectiveText(hostSpec, &draft)
	return from, to, from != "" && to != "" && from != to && old.Inference.APIKey != "" && draft.Inference.APIKey != ""
}

const hostConfirmMessage = "Confirm that the API key may be sent to the new address before saving."

type reviewData struct {
	Changes     []changeRow
	Diff        []diffLine
	NoChange    bool
	NextVersion int
	HostFrom    string
	HostTo      string
	HostMoved   bool
	// Replies is set when the changes touch structured replies or the instructions.
	Replies bool
}

// touchesReplies reports whether the draft changes the reply settings.
func touchesReplies(old, draft config.Config) bool {
	for _, k := range []string{"inference.structured_output", keySystem} {
		f := mustSpec(k)
		if f.get(&old) != f.get(&draft) {
			return true
		}
	}
	return false
}

func yamlOf(c config.Config) string {
	raw, err := c.Marshal()
	if err != nil {
		return ""
	}
	return string(raw)
}

func (s *Web) handleSettingsReview(w http.ResponseWriter, r *http.Request) {
	c, _, ok := s.buildCandidate(w, r)
	if !ok {
		return
	}
	from, to, moved := hostMove(c.st.base, c.draft)
	d := reviewData{
		Changes: c.changes, Diff: diffLines(c.st.shown(), yamlWithoutIngest(c.draft)), NoChange: c.unchanged(),
		NextVersion: c.st.version + 1, HostFrom: from, HostTo: to, HostMoved: moved,
		Replies: touchesReplies(c.st.base, c.draft),
	}
	s.renderSettings(w, "settingsReview", d)
}

func (s *Web) renderSettings(w http.ResponseWriter, name string, data any) {
	var buf bytes.Buffer
	if err := settingsTmpl.ExecuteTemplate(&buf, name, data); err != nil {
		log.Error().Err(err).Str("fragment", name).Msg("render settings fragment")
		http.Error(w, "could not render that", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(buf.Bytes())
}

func (s *Web) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	c, form, ok := s.buildCandidate(w, r)
	if !ok {
		return
	}
	if _, _, moved := hostMove(c.st.base, c.draft); moved && form.Get("confirm_host") != "1" {
		problemsReply(w, []fieldProblem{{Field: keyHost, Msg: hostConfirmMessage}})
		return
	}
	if c.unchanged() {
		writeReply(
			w,
			http.StatusOK,
			editReply{OK: true, NoOp: true, Version: c.st.version, Message: "No changes to save."},
		)
		return
	}
	p, err := profilemgr.PrepareRun(c.draft)
	if err != nil {
		refuse(w, http.StatusUnprocessableEntity, "Those settings could not be saved.")
		return
	}
	saved, err := s.db.SaveConfig(r.Context(), string(p.Content), p.Hash, store.ConfigSourceUI, c.st.version)
	if errors.Is(err, store.ErrConfigConflict) {
		stale(w)
		return
	}
	if err != nil {
		log.Error().Err(err).Msg("save settings")
		refuse(w, http.StatusInternalServerError, "Could not save the settings.")
		return
	}
	writeReply(w, http.StatusOK, editReply{
		OK: true, Version: saved.Version, Message: fmt.Sprintf("Saved as version %d. %s", saved.Version, savedNote),
	})
}

// checkScorer builds the scorer the way an ingest run does, which runs the
// provider's startup checks, and discards it.
func checkScorer(ctx context.Context, cfg config.Config) error {
	_, err := inference.Resolve(ctx, cfg.Inference, cfg.Inference.ThinkEnabled(), cfg.Inference.EffectiveSystemPrompt())
	return err
}

// handleSettingsTest checks the form's current, unsaved scoring values. It
// writes nothing.
func (s *Web) handleSettingsTest(w http.ResponseWriter, r *http.Request) {
	form, ok := readForm(w, r)
	if !ok {
		return
	}
	st, err := s.editState(r.Context())
	if err != nil {
		refuse(w, http.StatusInternalServerError, "Could not read the saved settings.")
		return
	}
	draft, problems := draftFrom(st.base, form)
	if problems = checkDraft(&draft, problems); len(problems) > 0 {
		problemsReply(w, problems)
		return
	}
	if _, _, moved := hostMove(st.base, draft); moved && form.Get("confirm_host") != "1" {
		problemsReply(w, []fieldProblem{{Field: keyHost, Msg: hostConfirmMessage}})
		return
	}
	cfg := draft.Clone()
	if _, err := cfg.Prepare(); err != nil {
		writeReply(w, http.StatusOK, editReply{Message: plainError(err, cfg)})
		return
	}
	if providerOf(cfg.Inference) == config.ProviderHeuristic {
		writeReply(
			w,
			http.StatusOK,
			editReply{OK: true, Message: "Keyword matching uses no model, so there is nothing to reach."},
		)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), testTimeout)
	defer cancel()
	start := time.Now()
	check := s.scorerCheck
	if check == nil {
		check = checkScorer
	}
	if err := check(ctx, cfg); err != nil {
		writeReply(w, http.StatusOK, editReply{Message: plainError(err, cfg), Millis: time.Since(start).Milliseconds()})
		return
	}
	writeReply(w, http.StatusOK, editReply{
		OK: true, Message: "Reached " + cfg.Inference.Host + ".", Millis: time.Since(start).Milliseconds(),
	})
}

// plainError is a failure in one short line with the key, if it leaked, masked.
func plainError(err error, cfg config.Config) string {
	msg := err.Error()
	if key, kerr := cfg.Inference.ResolvedAPIKey(); kerr == nil && key != "" {
		msg = strings.ReplaceAll(msg, key, "****")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "No answer after " + testTimeout.String() + ". Is the server running?"
	}
	msg = strings.TrimPrefix(strings.TrimPrefix(msg, "backend validation: "), "backend init: ")
	msg = strings.Join(strings.Fields(msg), " ")
	if len(msg) > 240 {
		msg = msg[:240] + "..."
	}
	return msg
}

// handleSettingsKey writes a pasted key to .env under its canonical variable.
// The value is never echoed, logged or stored anywhere else.
func (s *Web) handleSettingsKey(w http.ResponseWriter, r *http.Request) {
	if !s.canPasteKey() {
		refuse(w, http.StatusForbidden,
			"Pasting a key is turned off. Set "+config.RefFor(keyAPIKey)+" where the server starts.")
		return
	}
	form, ok := readForm(w, r)
	if !ok {
		return
	}
	st, err := s.editState(r.Context())
	if err != nil {
		refuse(w, http.StatusInternalServerError, "Could not read the saved settings.")
		return
	}
	if exp, err := strconv.Atoi(form.Get("expected_version")); err != nil || exp != st.version {
		stale(w)
		return
	}
	value := strings.TrimSpace(form.Get("value"))
	if value == "" {
		refuse(w, http.StatusUnprocessableEntity, "Paste the key first.")
		return
	}
	name := config.RefFor(keyAPIKey)
	if s.envFile.Source(name) == config.SourceEnvironment {
		refuse(w, http.StatusConflict, name+" is set in the environment. Change it where it is set.")
		return
	}
	if err := s.envFile.Set(name, value); err != nil {
		refuse(w, http.StatusUnprocessableEntity, strings.ReplaceAll(err.Error(), value, "****"))
		return
	}
	chip, tone := s.keyChip(name)
	writeReply(w, http.StatusOK, editReply{OK: true, Ref: "$" + name, Chip: chip, Tone: tone,
		Message: "Saved to " + s.envFile.Path + "."})
}

func (s *Web) handleSettingsRollback(w http.ResponseWriter, r *http.Request) {
	form, ok := readForm(w, r)
	if !ok {
		return
	}
	version, err1 := strconv.Atoi(form.Get("version"))
	expected, err2 := strconv.Atoi(form.Get("expected_version"))
	if err1 != nil || err2 != nil {
		refuse(w, http.StatusBadRequest, "That request could not be read.")
		return
	}
	ctx := r.Context()
	target, err := s.db.ConfigVersion(ctx, version)
	if errors.Is(err, store.ErrConfigVersionNotFound) {
		refuse(w, http.StatusNotFound, "That version no longer exists.")
		return
	}
	if err != nil {
		log.Error().Err(err).Msg("read settings version")
		refuse(w, http.StatusInternalServerError, "Could not read that version.")
		return
	}
	// An unreadable row must not become the active config.
	restored, perr := config.ParseStoredCanonical([]byte(target.Content), target.SchemaVersion)
	if perr != nil {
		refuse(w, http.StatusUnprocessableEntity,
			fmt.Sprintf("Version %d cannot be read, so it cannot be restored.", version))
		return
	}
	st, err := s.editState(ctx)
	if err != nil {
		refuse(w, http.StatusInternalServerError, "Could not read the saved settings.")
		return
	}
	if expected != st.version {
		stale(w)
		return
	}
	if st.parseErr == nil && st.has && restored.EffectiveHash() == st.base.EffectiveHash() {
		writeReply(w, http.StatusOK, editReply{
			OK: true, NoOp: true, Version: st.version, Message: "Already the active settings.",
		})
		return
	}
	if from, to, moved := hostMove(st.base, restored); moved && form.Get("confirm_host") != "1" {
		moveReply := editReply{Message: fmt.Sprintf(
			"Version %d moves the server address from %s to %s, and an API key is set. "+
				"Confirm that the key may be sent to the new address before rolling back.", version, from, to),
			Errors: []editError{{Field: keyHost, Msg: hostConfirmMessage}}}
		writeReply(w, http.StatusUnprocessableEntity, moveReply)
		return
	}
	saved, err := s.db.RollbackConfig(ctx, version, expected)
	switch {
	case errors.Is(err, store.ErrConfigConflict):
		stale(w)
	case errors.Is(err, store.ErrConfigVersionNotFound):
		refuse(w, http.StatusNotFound, "That version no longer exists.")
	case err != nil:
		log.Error().Err(err).Msg("roll back settings")
		refuse(w, http.StatusInternalServerError, "Could not roll back.")
	default:
		writeReply(w, http.StatusOK, editReply{
			OK:      true,
			Version: saved.Version,
			Message: fmt.Sprintf(
				"Rolled back to version %d, saved as version %d. %s",
				version,
				saved.Version,
				savedNote,
			),
		})
	}
}

type versionDiffData struct {
	Version int
	Active  int
	Diff    []diffLine
}

// handleSettingsVersionDiff shows what rolling back to a version would change.
func (s *Web) handleSettingsVersionDiff(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	v, err := s.db.ConfigVersion(r.Context(), n)
	if errors.Is(err, store.ErrConfigVersionNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "could not read that version", http.StatusInternalServerError)
		return
	}
	st, err := s.editState(r.Context())
	if err != nil {
		http.Error(w, "could not read the saved settings", http.StatusInternalServerError)
		return
	}
	s.renderSettings(w, "settingsVersionDiff", versionDiffData{
		Version: n, Active: st.version, Diff: diffLines(st.shown(), shownYAML(v.Content, v.SchemaVersion)),
	})
}

// handleSettingsExport downloads the active config as yaml (default) or json,
// the saved settings or with defaults (?defaults=1). It holds references to key
// variables, never keys.
func (s *Web) handleSettingsExport(w http.ResponseWriter, r *http.Request) {
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "yaml"
	}
	if format != "yaml" && format != "json" {
		http.Error(w, "format must be yaml or json", http.StatusBadRequest)
		return
	}
	defaults := r.URL.Query().Get("defaults") == "1"
	st, err := s.editState(r.Context())
	if err != nil {
		http.Error(w, "could not read the saved settings", http.StatusInternalServerError)
		return
	}
	var cur *config.ExportVersion
	if st.has {
		cur = &config.ExportVersion{Content: st.active.Content, SchemaVersion: st.active.SchemaVersion,
			Version: st.active.Version, Source: st.active.Source, CreatedAt: st.active.CreatedAt}
	}
	body, err := config.RenderExport(cur, format, defaults)
	if err != nil {
		http.Error(w, "could not read the saved settings", http.StatusInternalServerError)
		return
	}
	name, ctype := "rabbithole-config", "application/yaml; charset=utf-8"
	if defaults {
		name += "-defaults"
	}
	if format == "json" {
		ctype = "application/json; charset=utf-8"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.%s"`, name, format))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, body)
}

type importData struct {
	Error       string
	Dropped     []string
	Defaulted   int      // settings equal to their default, which stay defaults
	Warnings    []string // deprecated keys the file uses
	Moves       bool     // a literal key in the file would be moved to .env
	Ignored     bool     // import-only keys in the file are not applied from the page
	NoChange    bool
	SinceOnly   bool // only ingest.since differs: applying saves no version
	SinceFrom   string
	SinceTo     string
	Diff        []diffLine
	NextVersion int
	HostFrom    string
	HostTo      string
	HostMoved   bool
	YAML        string
}

func (s *Web) handleSettingsImportDialog(w http.ResponseWriter, _ *http.Request) {
	s.renderSettings(w, "settingsImport", nil)
}

// importSource reads the pasted or uploaded file from a request.
func importSource(w http.ResponseWriter, r *http.Request) (text string, form url.Values, ok bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxSettingsBody)
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		if err := r.ParseMultipartForm(maxSettingsBody); err != nil {
			refuse(w, http.StatusBadRequest, "That file is too large or could not be read.")
			return "", nil, false
		}
		text = r.FormValue("yaml")
		if f, _, err := r.FormFile("file"); err == nil {
			defer func() { _ = f.Close() }()
			raw, rerr := io.ReadAll(io.LimitReader(f, maxSettingsBody))
			if rerr != nil {
				refuse(w, http.StatusBadRequest, "That file could not be read.")
				return "", nil, false
			}
			text = string(raw)
		}
		return text, r.Form, true
	}
	if err := r.ParseForm(); err != nil {
		refuse(w, http.StatusBadRequest, "That request was too large or could not be read.")
		return "", nil, false
	}
	return r.PostForm.Get("yaml"), r.PostForm, true
}

// prepareUpload parses an uploaded config the way the CLI does. The file's
// import-only keys (the store, the profile and feed paths) are not applied from
// the page: they name files on the server. ingest.since is applied, to the feed
// defaults.
func (s *Web) prepareUpload(text string) (profilemgr.Prepared, error) {
	file, err := config.ParseFile([]byte(text))
	if err != nil {
		return profilemgr.Prepared{}, err
	}
	// The page never reads a server path: a system_prompt is the prompt text.
	if config.LooksLikePath(file.Inference.SystemPrompt.Text) {
		return profilemgr.Prepared{}, errors.New(
			"inference.system_prompt looks like a file path; a web import cannot read files, paste the prompt text instead",
		)
	}
	p, err := profilemgr.Prepare(file, "this file", log.Logger)
	if err != nil {
		return profilemgr.Prepared{}, err
	}
	if len(p.Secrets) > 0 && !s.canPasteKey() {
		return profilemgr.Prepared{}, errors.New(
			"this file holds a literal api_key, and writing keys to .env is turned off; use a $RABBITHOLE_..._API_KEY reference",
		)
	}
	p.Legacy = config.Legacy{Since: p.Legacy.Since}
	return p, nil
}

func uploadHasImportOnlyKeys(text string) bool {
	file, err := config.ParseFile([]byte(text))
	if err != nil {
		return false
	}
	return file.Profile != "" || file.Ingest.Feeds != "" || file.Store != (config.StoreConfig{})
}

func (s *Web) handleSettingsImportPreview(w http.ResponseWriter, r *http.Request) {
	text, _, ok := importSource(w, r)
	if !ok {
		return
	}
	st, err := s.editState(r.Context())
	if err != nil {
		refuse(w, http.StatusInternalServerError, "Could not read the saved settings.")
		return
	}
	d := importData{YAML: text, NextVersion: st.version + 1}
	p, err := s.prepareUpload(text)
	if err != nil {
		d.Error = importMessage(err)
		s.renderSettings(w, "settingsImportPreview", d)
		return
	}
	d.Dropped = p.Dropped
	d.Defaulted = len(p.Defaulted)
	d.Moves = len(p.Secrets) > 0
	d.Ignored = uploadHasImportOnlyKeys(text)
	same := p.EffectiveHash == st.base.EffectiveHash()
	from, sinceChanged, err := profilemgr.SinceChange(r.Context(), s.db, p)
	if err != nil {
		refuse(w, http.StatusInternalServerError, "Could not read the saved settings.")
		return
	}
	if sinceChanged {
		d.SinceFrom, d.SinceTo = sinceText(from), p.Legacy.Since.Short()
	}
	d.NoChange = same && !sinceChanged
	d.SinceOnly = same && sinceChanged
	d.Warnings = p.Warnings
	d.Diff = diffLines(st.shown(), yamlWithoutIngest(p.Run))
	d.HostFrom, d.HostTo, d.HostMoved = hostMove(st.base, p.Run)
	s.renderSettings(w, "settingsImportPreview", d)
}

// sinceText is a feed defaults lookback for display, "none" when unset.
func sinceText(d config.Duration) string {
	if d == 0 {
		return "none"
	}
	return d.Short()
}

// importMessage keeps an import failure to words a person can act on.
func importMessage(err error) string {
	var lines []string
	for _, fe := range config.FieldErrors(err) {
		lines = append(lines, fe.Msg)
	}
	if len(lines) > 0 {
		return "This file is not a valid config. " + strings.Join(lines, " ")
	}
	msg := strings.Join(strings.Fields(err.Error()), " ")
	if len(msg) > 240 {
		msg = msg[:240] + "..."
	}
	return msg
}

func (s *Web) handleSettingsImportApply(w http.ResponseWriter, r *http.Request) {
	text, form, ok := importSource(w, r)
	if !ok {
		return
	}
	st, err := s.editState(r.Context())
	if err != nil {
		refuse(w, http.StatusInternalServerError, "Could not read the saved settings.")
		return
	}
	if exp, err := strconv.Atoi(form.Get("expected_version")); err != nil || exp != st.version {
		stale(w)
		return
	}
	p, err := s.prepareUpload(text)
	if err != nil {
		refuse(w, http.StatusUnprocessableEntity, importMessage(err))
		return
	}
	if _, _, moved := hostMove(st.base, p.Run); moved && form.Get("confirm_host") != "1" {
		refuse(w, http.StatusUnprocessableEntity, hostConfirmMessage)
		return
	}
	out, err := profilemgr.ApplyImport(r.Context(), s.db, s.envFile, p, "an uploaded file",
		store.ConfigSourceImport+"upload",
		profilemgr.ImportOpts{Log: log.Logger, ExpectedVersion: &st.version})
	if errors.Is(err, store.ErrConfigConflict) {
		stale(w)
		return
	}
	if err != nil {
		refuse(w, http.StatusUnprocessableEntity, importMessage(err))
		return
	}
	if !out.Saved {
		msg := "No changes to save."
		if out.FeedSince {
			msg = "Feed defaults updated. No new version was saved."
		}
		writeReply(w, http.StatusOK, editReply{OK: true, NoOp: true, Version: out.Version, Message: msg})
		return
	}
	writeReply(w, http.StatusOK, editReply{OK: true, Version: out.Version,
		Message: fmt.Sprintf("Imported as version %d. %s", out.Version, savedNote)})
}

// shownYAML renders stored config text for the page: parsed and rendered again
// without the deprecated ingest block (digest_dir), which the page never edits
// but a save keeps. Text that does not parse is shown as it is.
func shownYAML(content string, schemaVersion int) string {
	cfg, err := config.ParseStoredCanonical([]byte(content), schemaVersion)
	if err != nil {
		return content
	}
	return yamlWithoutIngest(cfg)
}

func yamlWithoutIngest(c config.Config) string {
	c.Ingest = config.IngestConfig{}
	return yamlOf(c)
}

// shown is the active version's text for the page, empty when nothing is saved.
func (st editState) shown() string {
	if !st.has {
		return ""
	}
	return shownYAML(st.active.Content, st.active.SchemaVersion)
}
