// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
)

func renderListbox(t *testing.T, d listboxData) string {
	t.Helper()
	var b bytes.Buffer
	if err := feedTmpl.ExecuteTemplate(&b, "listbox", d); err != nil {
		t.Fatalf("render listbox: %v", err)
	}
	return b.String()
}

func sampleListbox() listboxData {
	return listboxData{
		ID: "sortMenu", Label: "Sort sources", Key: "Sort", Name: "srcSort",
		Value: "name", Default: "name", Inline: true,
		Options: []listboxOpt{
			{Value: "name", Label: "Name"},
			{Value: "perweek", Label: "Stored items per week", Hint: "busiest first"},
			{Value: "last", Label: "Last item"},
		},
	}
}

func TestListboxMarkup(t *testing.T) {
	body := renderListbox(t, sampleListbox())

	wantAttrs(t, "menu", tagWith(t, body, "id", "sortMenu"), map[string]string{
		"data-stp-menu": present, "data-default": "name", "class": "stp-menu stp-menu--inline",
	})
	wantAttrs(
		t,
		"button",
		parseTag(regexp.MustCompile(`<button[^>]*data-stp-menubtn[^>]*>`).FindString(body)),
		map[string]string{
			"data-stp-menubtn": present,
			"aria-haspopup":    "listbox",
			"aria-expanded":    "false",
			"aria-label":       "Sort sources",
		},
	)
	wantAttrs(t, "list", tagWith(t, body, "role", "listbox"), map[string]string{
		"aria-label": "Sort sources", "hidden": present,
	})
	if got := strings.Count(body, `role="option"`); got != 3 {
		t.Errorf("options = %d, want 3", got)
	}
	wantAttrs(t, "option", tagWith(t, body, "data-value", "perweek"), map[string]string{
		"role": "option", "aria-selected": "false", "data-label": "Stored items per week",
	})
	for _, want := range []string{
		`<span class="stp-menu__k">Sort</span>`,
		`<span class="stp-menu__cur" data-stp-menucur>Name</span>`,
		`<span class="stp-menu__p">busiest first</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("listbox lacks %q", want)
		}
	}
	if strings.Contains(body, "is-on") {
		t.Error("button is lit while the pick is the default")
	}
}

func TestListboxRadios(t *testing.T) {
	d := sampleListbox()
	d.Value = "perweek"
	body := renderListbox(t, d)

	radios := regexp.MustCompile(`<input[^>]*type="radio"[^>]*>`).FindAllString(body, -1)
	if len(radios) != 3 {
		t.Fatalf("radios = %d, want 3; body=%s", len(radios), body)
	}
	for i, v := range []string{"name", "perweek", "last"} {
		a := parseTag(radios[i])
		wantAttrs(t, "radio "+v, a, map[string]string{"name": "srcSort", "value": v})
		_, checked := a["checked"]
		if checked != (v == "perweek") {
			t.Errorf("radio %s checked = %v", v, checked)
		}
	}
	if !regexp.MustCompile(`<div class="stp-sr" hidden>`).MatchString(body) {
		t.Error("radios are not in a hidden .stp-sr")
	}
	if !strings.Contains(body, `data-stp-menucur>Stored items per week</span>`) {
		t.Error("button does not show the picked label")
	}
}

func TestListboxOnWhenNotDefault(t *testing.T) {
	d := sampleListbox()
	d.Value = "last"
	body := renderListbox(t, d)
	if !regexp.MustCompile(`<button[^>]*class="stp-menu__btn is-on"`).MatchString(body) {
		t.Errorf("button lacks is-on for a non-default pick; body=%s", body)
	}
}

func TestListboxBlockWithoutKey(t *testing.T) {
	d := sampleListbox()
	d.Key, d.Inline = "", false
	body := renderListbox(t, d)
	if strings.Contains(body, "stp-menu__k") || strings.Contains(body, "stp-menu--inline") {
		t.Errorf("key or inline class rendered without being asked; body=%s", body)
	}
}

func TestLayoutLoadsListboxBeforeSources(t *testing.T) {
	w, _ := configTestWeb(t)
	body := get(t, w, "/settings/general")
	lb := strings.Index(body, `/static/js/listbox.js`)
	st := strings.Index(body, `/static/js/settings.js`)
	if lb < 0 || st < 0 {
		t.Fatalf("scripts missing: listbox=%d settings=%d", lb, st)
	}
	if lb > st {
		t.Errorf("listbox.js must load before settings.js (%d, %d)", lb, st)
	}

	body = get(t, newTestWeb(t), "/sources")
	lb = strings.Index(body, `/static/js/listbox.js`)
	src := strings.Index(body, `/static/js/sources.js`)
	modal := strings.Index(body, `/static/js/modal.js`)
	if lb < 0 || src < 0 || modal < 0 {
		t.Fatalf("scripts missing: listbox=%d sources=%d modal=%d", lb, src, modal)
	}
	if lb > src || src > modal {
		t.Errorf("sources.js must sit between listbox.js and modal.js (%d, %d, %d)", lb, src, modal)
	}
}
