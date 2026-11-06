// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

// listboxData feeds the "listbox" partial: a themed single-choice menu backed
// by hidden radios named Name. Value is the current pick and Default the one
// that reads as unchanged; any other pick lights the button.
type listboxData struct {
	ID      string
	Label   string // accessible name
	Key     string // visible prefix on the button, optional
	Name    string // radio group name
	Value   string
	Default string
	Options []listboxOpt
	Inline  bool // shrink to the content instead of filling the row
	OOB     bool // render as an out-of-band update to the menu already on the page
}

// listboxOpt is one choice. Hint is the dim text beside the label, and Short
// is the button text when the label says more than the pick does.
type listboxOpt struct {
	Value string
	Label string
	Short string
	Hint  string
}

// Text is what the button shows when the option is picked.
func (o listboxOpt) Text() string {
	if o.Short != "" {
		return o.Short
	}
	return o.Label
}

// Current is the label of the picked option, shown on the button before the
// script runs.
func (d listboxData) Current() string {
	for _, o := range d.Options {
		if o.Value == d.Value {
			return o.Text()
		}
	}
	return ""
}

// On reports whether the pick differs from the default.
func (d listboxData) On() bool { return d.Value != d.Default }
