// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/rs/zerolog/log"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/profile"
	"github.com/DanielBlei/rabbithole/internal/rank"
	"github.com/DanielBlei/rabbithole/internal/store"
)

var settingsTmpl = pageTmpl("settings.html")

// settingsHistoryLimit bounds the saved-versions list on the Advanced tab.
const settingsHistoryLimit = 20

// Tab slugs, in the order the bar shows them.
const (
	tabGeneral    = "general"
	tabProfiles   = "profiles"
	tabScoring    = "scoring"
	tabAppearance = "appearance"
	tabWidgets    = "widgets"
	tabAccount    = "account"
	tabAdvanced   = "advanced"
)

// EnvSource says where an environment variable's value came from. The .env
// loader implements it; a nil source falls back to the process environment.
type EnvSource interface {
	Source(name string) string
}

// StoreInfo describes the store the server opened, for the Advanced tab.
// Location is already safe to show: shortened with ~ and without a password.
type StoreInfo struct {
	Engine   string // SQLite or Postgres
	Location string
	Origin   string // where the location came from, in words
}

// SetEnvSource tells the settings page how to tell a key set in the
// environment from one read out of .env.
func (s *Web) SetEnvSource(e EnvSource) { s.env = e }

// SetStoreInfo hands the settings page what boot knew about the store.
func (s *Web) SetStoreInfo(i StoreInfo) { s.storeInfo = &i }

type settingsTab struct{ Slug, Title string }

// settingRow is one label-and-value row in the read-only groups.
type settingRow struct {
	Name  string
	Help  string
	Value string
	Mono  bool   // the value is a path or identifier
	Tag   string // "Default" when the value is the built-in one
	Chip  string // status chip text, beside the value
	Tone  string // chip tone: ok | warn
}

func newRow(name, help, value string) settingRow {
	return settingRow{Name: name, Help: help, Value: value}
}

// historyRow is one saved config version.
type historyRow struct {
	Version int
	Source  string
	When    string
	Active  bool
}

type optView struct {
	Value, Label string
	On           bool
}

// fieldView is one editable field as the template draws it.
// tipView is one "?" bubble: its element id, its text and the accessible name
// of its button.
type tipView struct{ ID, Text, Label string }

type fieldView struct {
	Key, Name, Help, Kind string
	Tip                   *tipView // the "?" bubble, nil for none
	Note                  string   // a quiet line under the help
	Value                 string   // what the control shows: the saved value, else the default
	Raw                   string   // what the config holds, empty when the default is in force
	Default               string
	Defaults              string // the default for every provider as JSON, for the fields that differ
	Tag                   string
	Applies               string // space-separated providers that read it, empty for all
	Hidden                bool   // the provider in force does not read it
	On                    bool   // a switch's state
	Options               []optView
	Min, Max              int
	Hint                  string
	SysMode, SysText      string
	SysBuiltin            string // the built-in instructions, offered as the starting text for a custom one
	KeyName               string // the variable the reference names
	Chip, Tone            string // the key's status
	Pasteable             bool
	EnvPath               string // where a pasted key is written
}

type settingsData struct {
	Title       string
	Active      string
	Chrome      chromeData
	PromptUser  string
	Tab         string
	Tabs        []settingsTab
	Managed     string
	ConfigError string // the saved config could not be read

	Profiles profileSettingsData
	// DefaultProfile is the built-in profile Reset to defaults switches back to.
	DefaultProfile string

	Version   int // the version the form was built from, sent back on every save
	F         map[string]fieldView
	Feeds     int
	EnvPath   string // where a pasted key is written, empty when pasting is off
	Pasteable bool

	VersionLabel string // "Version 3" or "Built-in defaults"
	LastSource   string
	LastSaved    string

	History    []historyRow
	AtDefaults bool // nothing saved differs from the defaults
	Store      []settingRow
}

func (s *Web) settingsTabs(ctx context.Context) []settingsTab {
	tabs := []settingsTab{
		{tabGeneral, "General"}, {tabProfiles, "Profiles"}, {tabScoring, "Scoring"},
		{tabAppearance, "Appearance"}, {tabWidgets, "Widgets"},
	}
	if authFrom(ctx).Mode != "" {
		tabs = append(tabs, settingsTab{tabAccount, "Account"})
	}
	return append(tabs, settingsTab{tabAdvanced, "Advanced"})
}

func (s *Web) handleSettingsIndex(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/settings/"+tabGeneral, http.StatusFound)
}

func (s *Web) handleSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tab := r.PathValue("tab")
	tabs := s.settingsTabs(ctx)
	known := false
	for _, t := range tabs {
		known = known || t.Slug == tab
	}
	if !known {
		http.NotFound(w, r)
		return
	}
	data, err := s.settingsData(ctx, tab, tabs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := settingsTmpl.ExecuteTemplate(&buf, "layout", data); err != nil {
		log.Error().Err(err).Msg("render settings")
		http.Error(w, "could not render settings", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

func (s *Web) settingsData(ctx context.Context, tab string, tabs []settingsTab) (settingsData, error) {
	// A saved version that no longer parses must not take the page down: the
	// viewer below is how it gets diagnosed. The built-in defaults stand in.
	st, err := s.editState(ctx)
	if err != nil {
		return settingsData{}, err
	}
	feeds, err := s.db.Feeds(ctx)
	if err != nil {
		return settingsData{}, err
	}
	cfg := st.base
	d := settingsData{
		Title: "Settings", Active: "settings", Chrome: s.chrome(ctx),
		PromptUser: promptUser(cfg.User), Tab: tab, Tabs: tabs, Managed: s.managedBy,
		Profiles: s.profileSettings(ctx, nil), DefaultProfile: profile.DefaultID,
		Feeds: len(feeds), Version: st.version, Pasteable: s.canPasteKey(),
	}
	if d.Pasteable {
		d.EnvPath = s.envFile.Path
	}
	if st.parseErr != nil {
		d.ConfigError = st.parseErr.Error()
	}
	d.F = s.fieldViews(cfg)

	hist, err := s.db.ConfigHistory(ctx, settingsHistoryLimit)
	if err != nil {
		return settingsData{}, err
	}
	for _, h := range hist {
		d.History = append(d.History, historyRow{
			Version: h.Version, Source: configSourceLabel(h.Source),
			When: h.CreatedAt.Local().Format("2 Jan 2006, 15:04"), Active: st.has && h.Version == st.active.Version,
		})
	}
	d.VersionLabel = "Built-in defaults"
	d.LastSource, d.LastSaved = "Nothing saved yet", "Never"
	if st.has {
		d.VersionLabel = fmt.Sprintf("Version %d", st.active.Version)
		d.LastSource = configSourceLabel(st.active.Source)
		d.LastSaved = st.active.CreatedAt.Local().Format("2 Jan 2006, 15:04")
	}
	d.AtDefaults = st.parseErr == nil && yamlOf(st.base) == yamlOf(config.Config{})
	d.Store = s.storeRows()
	return d, nil
}

// providerDefaultsJSON is the field's default under each provider, as a JSON
// object, for the page to show the right default when the provider changes.
// Fields whose default does not depend on the provider carry none.
func providerDefaultsJSON(f *fieldSpec) string {
	if f.Key == keyProvider || f.Key == keyUser || f.Kind == kSys || f.Kind == kKey {
		return ""
	}
	m := make(map[string]string, len(config.Providers))
	same := true
	for _, p := range config.Providers {
		m[p] = defaultText(f, p)
		same = same && m[p] == m[config.Providers[0]]
	}
	if same {
		return ""
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func providerLabel(p string) string {
	if n := providerNames[p]; n != "" {
		return n
	}
	return p
}

// fieldViews turns the saved config into what each control shows.
func (s *Web) fieldViews(saved config.Config) map[string]fieldView {
	provider := providerOf(saved.Inference)
	out := make(map[string]fieldView, len(fieldSpecs))
	for i := range fieldSpecs {
		f := &fieldSpecs[i]
		raw := f.get(&saved)
		def := defaultText(f, provider)
		if f.Key == keyProvider {
			def = config.ProviderOllama
		}
		v := fieldView{
			Key: f.Key, Name: f.Name, Help: f.Help, Kind: string(f.Kind), Raw: raw, Default: def,
			Value: raw, Min: f.Min, Max: f.Max, Hint: f.Hint,
			Hidden: !f.readsField(provider),
		}
		v.Tip = tipFor(f.Key)
		if f.Applies != "" {
			v.Applies = strings.Join(f.providers(), " ")
		}
		v.Defaults = providerDefaultsJSON(f)
		if raw == "" && f.Kind != kKey {
			v.Tag = "Default"
			v.Value = def
		}
		switch f.Kind {
		case kBool:
			v.On = v.Value == "true"
		case kChoice:
			for _, o := range f.Options {
				v.Options = append(v.Options, optView{Value: o, Label: providerLabelOr(f.Key, o), On: o == v.Value})
			}
		case kSys:
			v.Value = ""
			v.SysMode, v.SysBuiltin = sysBuiltin, rank.DefaultSystemPrompt
			switch raw {
			case "":
			case sysNone:
				v.SysMode = sysNone
			default:
				v.SysMode, v.SysText, v.Raw = sysCustom, raw, sysCustom
			}
		case kKey:
			s.fillKey(&v, raw)
		case kStep, kNum:
			if f.Key == "inference.model_tuning.num_ctx" || f.Key == "inference.model_tuning.max_tokens" {
				v.Hint = "Not set"
			}
		}
		if f.Key == keyUser && raw == "" {
			v.Note = "From your computer login."
		}
		out[f.Key] = v
	}
	return out
}

func providerLabelOr(key, value string) string {
	if key == keyProvider {
		return providerLabel(value)
	}
	return value
}

// fillKey sets the key reference and its status chip. Only the variable name is
// ever read; the value is not looked at.
func (s *Web) fillKey(v *fieldView, ref string) {
	v.Pasteable = s.canPasteKey()
	if v.Pasteable {
		v.EnvPath = s.envFile.Path
	}
	if ref == "" {
		// A key pasted before the reference was saved is already in .env.
		switch s.keySource(config.RefFor(keyAPIKey)) {
		case config.SourceEnvFile:
			v.Chip, v.Tone = "In .env, not saved yet", "warn"
		case config.SourceEnvironment:
			v.Chip, v.Tone = "In environment, not saved yet", "warn"
		default:
			v.Chip, v.Tone = "No key set", "dim"
		}
		return
	}
	name, ok := config.RefName(ref)
	if !ok {
		v.Chip, v.Tone = "Not a variable name", "warn"
		return
	}
	v.KeyName = name
	v.Chip, v.Tone = s.keyChip(name)
}

// keyChip names where the variable's value comes from, by name only.
func (s *Web) keyChip(name string) (chip, tone string) {
	switch s.keySource(name) {
	case config.SourceEnvironment:
		return "Set in environment", "ok"
	case config.SourceEnvFile:
		return "Set in .env", "ok"
	}
	return "Missing", "warn"
}

var providerNames = map[string]string{
	config.ProviderOllama:    "Ollama",
	config.ProviderVLLM:      "vLLM",
	config.ProviderOpenAI:    "OpenAI",
	config.ProviderHeuristic: "Keywords",
}

// providerOf is the provider in force: an unset one means the default.
func providerOf(in config.InferenceConfig) string {
	if in.Provider == "" {
		return config.ProviderOllama
	}
	return in.Provider
}

func (s *Web) keySource(name string) string {
	if s.env != nil {
		return s.env.Source(name)
	}
	if v, ok := os.LookupEnv(name); ok && v != "" {
		return config.SourceEnvironment
	}
	return config.SourceMissing
}

func (s *Web) storeRows() []settingRow {
	if s.storeInfo == nil {
		return []settingRow{newRow("Store", "Chosen before the app starts.", "Not reported")}
	}
	i := s.storeInfo
	loc := newRow("Location", "", i.Location)
	loc.Mono = true
	return []settingRow{
		newRow("Engine", "", i.Engine),
		loc,
		newRow("Set by", "", i.Origin),
	}
}

// configSourceLabel turns a stored source into words.
func configSourceLabel(src string) string {
	switch {
	case src == store.ConfigSourceUI:
		return "Settings page"
	case src == store.ConfigSourceImport+"upload":
		return "Imported from an uploaded file"
	case strings.HasPrefix(src, store.ConfigSourceImport):
		return "Imported from " + filepath.Base(strings.TrimPrefix(src, store.ConfigSourceImport))
	case strings.HasPrefix(src, store.ConfigSourceRollback):
		return "Rollback to version " + strings.TrimPrefix(src, store.ConfigSourceRollback)
	}
	return src
}
