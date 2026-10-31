// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/rank"
)

// The editable run-config fields of the settings page. One table drives the
// markup, the form parsing and the review: each entry knows its dotted key
// (also its form name), how to read and write it on a config.Config, and which
// providers read it.

type fieldKind string

const (
	kText   fieldKind = "text"
	kNum    fieldKind = "num"
	kBool   fieldKind = "bool"
	kChoice fieldKind = "choice"
	kStep   fieldKind = "step"
	kSys    fieldKind = "sys"
	kKey    fieldKind = "key"
)

const (
	keyUser     = "user"
	keyProvider = "inference.provider"
	keyHost     = "inference.host"
	keyAPIKey   = "inference.api_key"
	keySystem   = "inference.system_prompt"
	// keySystemText is the instructions text that goes with the system prompt's mode.
	keySystemText = "inference.system_prompt_text"

	sysBuiltin = "default"
	sysCustom  = "custom"
	sysNone    = "none"
)

type fieldSpec struct {
	Key, Name, Help string
	Kind            fieldKind
	Applies         string // the provider-table name; empty when every provider reads it
	Options         []string
	Min, Max        int
	Hint            string // placeholder when the control is empty
	get             func(*config.Config) string
	set             func(*config.Config, string) string // returns a problem, or empty
}

func strSpec(key, name, help string, kind fieldKind, applies string, p func(*config.Config) *string) fieldSpec {
	return fieldSpec{
		Key: key, Name: name, Help: help, Kind: kind, Applies: applies,
		get: func(c *config.Config) string { return *p(c) },
		set: func(c *config.Config, v string) string { *p(c) = v; return "" },
	}
}

func intSpec(
	key, name, help string,
	kind fieldKind,
	applies string,
	lo, hi int,
	p func(*config.Config) *int,
) fieldSpec {
	return fieldSpec{
		Key: key, Name: name, Help: help, Kind: kind, Applies: applies, Min: lo, Max: hi,
		get: func(c *config.Config) string {
			if v := *p(c); v != 0 {
				return strconv.Itoa(v)
			}
			return ""
		},
		set: func(c *config.Config, v string) string {
			if v == "" {
				*p(c) = 0
				return ""
			}
			n, err := strconv.Atoi(v)
			if err != nil || n < lo || (hi > 0 && n > hi) {
				if hi > 0 {
					return fmt.Sprintf("Pick a whole number from %d to %d.", lo, hi)
				}
				return "Use a whole number above zero, or clear it to use the default."
			}
			*p(c) = n
			return ""
		},
	}
}

func boolSpec(key, name, help, applies string, p func(*config.Config) **bool) fieldSpec {
	return fieldSpec{
		Key: key, Name: name, Help: help, Kind: kBool, Applies: applies,
		get: func(c *config.Config) string {
			if b := *p(c); b != nil {
				return strconv.FormatBool(*b)
			}
			return ""
		},
		set: func(c *config.Config, v string) string {
			switch v {
			case "":
				*p(c) = nil
			case "true", "false":
				b := v == "true"
				*p(c) = &b
			default:
				return "Choose on or off."
			}
			return ""
		},
	}
}

var efforts = []string{"low", "medium", "high", "xhigh"}

// fieldSpecs lists every editable field, provider first: the others are read
// against the provider the form picks.
var fieldSpecs = []fieldSpec{
	strSpec(keyProvider, "Provider",
		"Who scores your articles.",
		kChoice, "", func(c *config.Config) *string { return &c.Inference.Provider }),
	strSpec(keyUser, "Prompt name", "The name in the shell prompt at the top of each page.",
		kText, "", func(c *config.Config) *string { return &c.User }),
	strSpec(keyHost, "Server address", "Where your model server listens, for example http://localhost:11434.",
		kText, "host", func(c *config.Config) *string { return &c.Inference.Host }),
	strSpec("inference.model", "Model", "The chat model, named exactly as your server knows it.",
		kText, "model", func(c *config.Config) *string { return &c.Inference.Model }),
	strSpec(keyAPIKey, "API key",
		"Only if the server asks for one.",
		kKey, "api_key", func(c *config.Config) *string { return &c.Inference.APIKey }),
	boolSpec("inference.think", "Reason before scoring", "Let the model reason first. Slower, usually more accurate.",
		"think", func(c *config.Config) **bool { return &c.Inference.Think }),
	strSpec("inference.effort", "Thinking effort",
		"How hard a hosted model reasons.",
		kChoice, "effort", func(c *config.Config) *string { return &c.Inference.Effort }),
	intSpec("inference.batch_size", "Articles per request", "Lower helps smaller models stay focused.",
		kStep, "batch_size", 1, 32, func(c *config.Config) *int { return &c.Inference.BatchSize }),
	intSpec("inference.max_parallel", "Requests at once", "Keep it at 1 for Ollama, which queues them anyway.",
		kStep, "max_parallel", 1, 16, func(c *config.Config) *int { return &c.Inference.MaxParallel }),
	boolSpec("inference.structured_output", "Structured replies",
		"Leave on unless scoring fails.",
		"structured_output", func(c *config.Config) **bool { return &c.Inference.StructuredOutput }),
	{
		Key: keySystem, Name: "Instructions",
		Help: "Built-in suits most setups.",
		Kind: kSys, Applies: "system_prompt",
		get: func(c *config.Config) string {
			sp := c.Inference.SystemPrompt
			switch {
			case sp.Disabled:
				return sysNone
			case sp.Text != "":
				return sp.Text
			}
			return ""
		},
		set: func(c *config.Config, v string) string {
			switch v {
			case "":
				c.Inference.SystemPrompt = config.SystemPromptSetting{}
			case sysNone:
				c.Inference.SystemPrompt = config.SystemPromptSetting{Disabled: true}
			default:
				c.Inference.SystemPrompt = config.SystemPromptSetting{Text: v}
			}
			return ""
		},
	},
	intSpec("inference.model_tuning.num_ctx", "Context window",
		"Ollama only. Empty uses the server's own limit.",
		kNum, "model_tuning.num_ctx", 1, 0, func(c *config.Config) *int { return &c.Inference.ModelTuning.NumCtx }),
	intSpec(
		"inference.model_tuning.max_tokens",
		"Reply limit",
		"Cap on one reply. Empty works it out from the budgets below.",
		kNum,
		"model_tuning.max_tokens",
		1,
		0,
		func(c *config.Config) *int { return &c.Inference.ModelTuning.MaxTokens },
	),
	intSpec("inference.model_tuning.tokens_per_item", "Reply budget per article",
		"Room the reply gets for each article in a batch.",
		kNum, "model_tuning.tokens_per_item", 1, 0,
		func(c *config.Config) *int { return &c.Inference.ModelTuning.TokensPerItem }),
	intSpec("inference.model_tuning.tokens_overhead", "Reply scaffolding",
		"Room for the structure around the answers.",
		kNum, "model_tuning.tokens_overhead", 1, 0,
		func(c *config.Config) *int { return &c.Inference.ModelTuning.TokensOverhead }),
	intSpec("inference.model_tuning.tokens_thinking", "Extra room when thinking",
		"Added to the reply budget while reasoning is on.",
		kNum, "model_tuning.tokens_thinking", 1, 0,
		func(c *config.Config) *int { return &c.Inference.ModelTuning.TokensThinking }),
	intSpec("inference.model_tuning.reason_max_chars", "Longest reason",
		"The most characters the model may use to explain a score.",
		kNum, "model_tuning.reason_max_chars", 1, 0,
		func(c *config.Config) *int { return &c.Inference.ModelTuning.ReasonMaxChars }),
}

// tips is the one place for the "?" bubble copy, keyed by a field's key or by
// the id a template names for a group or a row. A key is never shown twice on a
// page: the visible helper line says something else.
var tips = map[string]string{
	keyProvider: "Ollama and vLLM are servers you run yourself. OpenAI is hosted: article titles and " +
		"summaries are sent to it and usage is billed to your account. " +
		"Keywords needs no server and only counts words from your profile.",
	keyAPIKey: "Local servers usually need none. Hosted ones do, and bill to it. " +
		"The config stores only the name of an environment variable, never the key. " +
		"Paste a key writes the value to your .env file and never shows it again, " +
		"so anyone who can read that file can use it.",
	"inference.think": "Reasoning models spend extra tokens thinking before they answer, so runs take longer " +
		"and a hosted model costs more. The extra room is added to the reply budget under Advanced. " +
		"Models without reasoning ignore it.",
	"inference.effort": "Used while reasoning is on, and only by OpenAI. Higher is slower and costs more " +
		"because the model spends more tokens thinking. If a model rejects the setting, none is sent.",
	"inference.batch_size": "Several articles in one request is faster and cheaper, but small models start mixing up " +
		"which score belongs to which article. Keep 1 for models under about 8B.",
	"inference.max_parallel": "Higher scores several batches at the same time, which finishes sooner. " +
		"Hosted APIs may start refusing requests at their rate limit, and a local server can run out of memory.",
	"inference.model_tuning.num_ctx": "How much text the model reads at once. The server's own window is often small " +
		"and silently drops the start of long batches. Raise it if scores ignore your profile or the first articles.",
	"inference.structured_output": "On, the server forces the reply into the exact JSON shape. " +
		"Off, the model writes freely and the app digs the JSON out of the text, " +
		"repairing cut-off replies where it can. Turn it off only if scores come back as junk.",
	keySystem: "None sends no instructions at all. Use it when the model already carries its own, " +
		"for example an Ollama Modelfile with a SYSTEM prompt, so the app does not override them. " +
		"With structured replies off, nothing then tells the model the reply format.",

	"test": "Uses the values on screen, saved or not, and never saves or scores articles. " +
		"It may send a tiny test request to the model, which a hosted provider can bill.",
	"tuning": "Set these once and leave them. Only options your provider reads are shown. " +
		"These cap how long a reply may be. The cap is the reply limit if set, otherwise " +
		"scaffolding plus the per-article budget times the batch size, plus the extra room when thinking. " +
		"A reply that hits the cap is cut off mid-answer and the scores after the cut can be lost, " +
		"so raise budgets if scores go missing.",
	"versions": "Rolling back adds a new version with the older settings and keeps every version after it, " +
		"so nothing is lost. Compare shows what a rollback would change first. " +
		"The next ingest run uses the active version.",
	"backup": "Import shows what would change before anything is saved, then saves it as a new version " +
		"you can roll back. Exports never include keys, only the names of their variables.",
	"feeds": "Import adds the feeds in a file right away, with no review. Feeds already known, " +
		"including deleted ones, are skipped, so nothing existing changes.",
	"reset-all": "You pick what to reset first. Settings come back as unsaved changes until you review and save them. " +
		"Profile, look and widgets reset at once and reload the page, asking first if you have unsaved changes. " +
		"The profile switches back to the built-in one and none is deleted.",
	"profile-active": "Edits to a profile apply to items scored from now on; existing scores stay. " +
		"Rescore recent articles replaces the scores and reasons of the last 7 days and may make paid or slow model calls. " +
		"Ratings, notes, read state and bookmarks stay.",
	"profiles": "Use makes a profile the one that scores new items. Duplicate makes an editable copy. " +
		"Delete removes a profile for good and asks first. If it was active, the built-in Default takes over " +
		"and scores already recorded stay.",
	"type": "Picks are kept in this browser only, so another browser or device starts from the defaults. " +
		"The preview follows your picks.",
	"weather": "Forecast and pollen come from Open-Meteo. A location you share is sent to it, and to BigDataCloud " +
		"to name the place. The place you pick is kept in this browser only.",
	"stay": "Keeps a signed cookie in this browser so a server restart does not log you out. " +
		"It lasts up to 30 days. Turn it off on a shared computer.",
	"everywhere": "Ends every other session, including ones set to stay signed in, on any device. " +
		"This browser stays signed in.",
}

// sourceTips is the "?" bubble copy of the Sources pages, keyed by the id a
// template names. It is kept apart from tips, which every Settings page shows.
var sourceTips = map[string]string{
	"src-ingest": "Ingestion fetches every enabled feed, then scores new items against your profile.",
	"src-runs": "A run fetches every enabled feed, then scores the new items. " +
		"Each feed uses its own since and max items.",
	"src-volume": "Stored items per week, last 8 weeks. Retention and the since window cut older history.",
	"src-signal": "Last 30 days. Signal is the score from your profile, not read or thumbs counts.",
	"src-defaults": "What every feed uses unless it sets the value itself. " +
		"Leave a field blank and it falls through to the built-in.",
	"src-import": "Choose a sources.yaml file, or drop one anywhere on this page. " +
		"You see what changes before anything is added.",
}

// tipFor is the "?" bubble for a key, nil when it has none. The templates
// reach it as `tip`; a key that names no copy renders nothing, and a test
// fails when a bubble's copy sits on no page.
func tipFor(key string) *tipView {
	label := "More about this setting"
	text, ok := tips[key]
	if !ok {
		if text, ok = sourceTips[key]; !ok {
			return nil
		}
		label = "More about this"
	}
	return &tipView{ID: "tip-" + strings.NewReplacer(".", "-").Replace(key), Text: text, Label: label}
}

func init() {
	for i := range fieldSpecs {
		switch fieldSpecs[i].Key {
		case keyProvider:
			fieldSpecs[i].Options = config.Providers
		case "inference.effort":
			fieldSpecs[i].Options = efforts
		}
	}
}

var specByKey = func() map[string]*fieldSpec {
	m := make(map[string]*fieldSpec, len(fieldSpecs))
	for i := range fieldSpecs {
		m[fieldSpecs[i].Key] = &fieldSpecs[i]
	}
	return m
}()

// mustSpec is the field with the key; a key that names no field is a bug the
// first test run finds.
func mustSpec(key string) *fieldSpec {
	f, ok := specByKey[key]
	if !ok {
		panic("settings: no field " + key)
	}
	return f
}

// hostSpec is the server address field, the one a host move watches.
var hostSpec = mustSpec(keyHost)

// readsField reports whether the provider reads the field.
func (f *fieldSpec) readsField(provider string) bool {
	return f.Applies == "" || config.Applies(provider, f.Applies)
}

// providers lists the providers that read the field, for the page to show and
// hide rows without asking the server.
func (f *fieldSpec) providers() []string {
	var out []string
	for _, p := range config.Providers {
		if f.readsField(p) {
			out = append(out, p)
		}
	}
	return out
}

// defaultsByProvider holds the defaults of every provider, built once: they do
// not change while the server runs.
var defaultsByProvider = sync.OnceValue(func() map[string]config.Config {
	m := make(map[string]config.Config, len(config.Providers))
	for _, p := range config.Providers {
		m[p] = config.DefaultsFor(p)
	}
	return m
})

// defaultText is the value a field takes when nothing is set, for the provider.
func defaultText(f *fieldSpec, provider string) string {
	if f.Key == keySystem {
		return ""
	}
	d, ok := defaultsByProvider()[provider]
	if !ok {
		d = config.DefaultsFor(provider)
	}
	return f.get(&d)
}

// effectiveText is what the field is in force: what was set, else the default.
func effectiveText(f *fieldSpec, c *config.Config) string {
	if v := f.get(c); v != "" {
		return v
	}
	return defaultText(f, providerOf(c.Inference))
}

// fieldProblem is one thing wrong with a submitted value.
type fieldProblem = config.FieldError

// draftFrom applies a submitted form on top of base, the active canonical
// config, and returns the canonical candidate. Anything the form does not carry
// (digest_dir) is left as it was. A blank value
// means the default, and so does a value equal to the default for a field that
// was not set before, so defaults keep flowing to a config that never pinned one.
func draftFrom(base config.Config, form url.Values) (config.Config, []fieldProblem) {
	draft := base.Clone()
	var problems []fieldProblem
	add := func(key, msg string) { problems = append(problems, fieldProblem{Field: key, Msg: msg}) }

	provider := base.Inference.Provider
	if form.Has(keyProvider) {
		provider = strings.TrimSpace(form.Get(keyProvider))
	}
	if provider == config.ProviderOllama && base.Inference.Provider == "" {
		provider = ""
	}
	if form.Get(keyProvider+"__reset") == "1" {
		provider = ""
	}
	draft.Inference.Provider = provider
	eff := providerOf(draft.Inference)

	for i := range fieldSpecs {
		f := &fieldSpecs[i]
		if f.Key == keyProvider || !form.Has(f.Key) {
			continue
		}
		v := strings.TrimSpace(form.Get(f.Key))
		if f.Kind == kSys {
			v = systemValue(form, add)
		}
		if form.Get(f.Key+"__reset") == "1" || (v != "" && v == defaultText(f, eff) && f.get(&base) == "") {
			v = ""
		}
		if msg := f.set(&draft, v); msg != "" {
			add(f.Key, msg)
		}
	}
	return draft, problems
}

// systemValue folds the instructions mode and text into one value: empty for the
// built-in (also when the text is the built-in itself), sysNone for none,
// otherwise the text.
func systemValue(form url.Values, add func(key, msg string)) string {
	switch form.Get(keySystem) {
	case sysNone:
		return sysNone
	case sysCustom:
		text := strings.TrimSpace(form.Get(keySystemText))
		switch {
		case text == "":
			add(keySystem, "Write your instructions, or pick Built-in.")
		case text == strings.TrimSpace(rank.DefaultSystemPrompt):
			return ""
		case config.LooksLikePath(text):
			add(keySystem, "That looks like a file path. Paste the instructions themselves.")
		}
		return text
	}
	return ""
}

// checkDraft validates the candidate the way an import does, through
// Canonical, and adds the checks only the form needs.
func checkDraft(draft *config.Config, problems []fieldProblem) []fieldProblem {
	if _, err := draft.Canonical(); err != nil {
		for _, fe := range config.FieldErrors(err) {
			if !slices.ContainsFunc(problems, func(p fieldProblem) bool { return p.Field == fe.Field }) {
				problems = append(problems, fe)
			}
		}
	}
	bad := func(key string) bool {
		return slices.ContainsFunc(problems, func(p fieldProblem) bool { return p.Field == key })
	}
	in := draft.Inference
	if config.Applies(providerOf(in), "host") && in.Host != "" && !bad(keyHost) {
		if u, err := url.Parse(in.Host); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			problems = append(problems, fieldProblem{Field: keyHost,
				Msg: "That is not a web address. Start it with http:// or https://."})
		}
	}
	return problems
}
