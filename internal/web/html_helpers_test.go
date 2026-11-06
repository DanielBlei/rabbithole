// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"html"
	"regexp"
	"testing"
)

// present, as a wanted attribute value, means the attribute is there with any
// value (or none, like a bare hidden).
const present = "\x00present"

var attrRe = regexp.MustCompile(`\s([A-Za-z_:][-\w:.]*)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'))?`)

// parseTag reads the attributes of one opening tag, in whatever order they
// were written. Values are unescaped; a bare attribute has the value "".
func parseTag(tag string) map[string]string {
	out := make(map[string]string)
	for _, m := range attrRe.FindAllStringSubmatch(tag, -1) {
		v := m[2]
		if v == "" {
			v = m[3]
		}
		out[m[1]] = html.UnescapeString(v)
	}
	return out
}

// tagWith finds the opening tag of the first element carrying attr="value" and
// returns its attributes, failing the test when there is none.
func tagWith(t *testing.T, body, attr, value string) map[string]string {
	t.Helper()
	re := regexp.MustCompile(
		`<[A-Za-z][^<>]*?\s` + regexp.QuoteMeta(attr) + `="` + regexp.QuoteMeta(value) + `"[^<>]*>`,
	)
	m := re.FindString(body)
	if m == "" {
		t.Errorf("no element with %s=%q; body=%s", attr, value, body)
		return nil
	}
	return parseTag(m)
}

// wantAttrs checks each named attribute of a tag, in any order. A value of
// present only requires the attribute to exist.
func wantAttrs(t *testing.T, what string, got, want map[string]string) {
	t.Helper()
	if got == nil {
		return
	}
	for name, v := range want {
		have, ok := got[name]
		switch {
		case !ok:
			t.Errorf("%s: no %s attribute; have %v", what, name, got)
		case v != present && have != v:
			t.Errorf("%s: %s = %q, want %q", what, name, have, v)
		}
	}
}

// lacksAttr fails when the tag carries the attribute.
func lacksAttr(t *testing.T, what string, got map[string]string, name string) {
	t.Helper()
	if _, ok := got[name]; ok && got != nil {
		t.Errorf("%s: unexpected %s attribute", what, name)
	}
}

func TestParseTagReadsAttributesInAnyOrder(t *testing.T) {
	a := parseTag(
		`<button class="a b" hx-vals='{"x":"1"}' hidden aria-label="Fetch &amp; co" disabled=""  data-x = "y">`,
	)
	want := map[string]string{
		"class": "a b", "hx-vals": `{"x":"1"}`, "hidden": "", "aria-label": "Fetch & co", "disabled": "", "data-x": "y",
	}
	if len(a) != len(want) {
		t.Fatalf("parsed %v, want %v", a, want)
	}
	for k, v := range want {
		if a[k] != v {
			t.Errorf("%s = %q, want %q", k, a[k], v)
		}
	}
}
