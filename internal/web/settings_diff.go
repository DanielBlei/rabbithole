// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"fmt"
	"strings"
)

// diffLine is one line of a unified-style diff. Op is "+", "-", " " or "..." for
// a gap where unchanged lines were left out.
type diffLine struct {
	Op   string
	Text string
}

// diffContext is how many unchanged lines stay around each change.
const diffContext = 2

// diffBudget caps the longest-common-subsequence table, in cells, after the
// shared head and tail of the two texts are set aside. Past it the diff is a
// summary instead of a line by line comparison.
const diffBudget = 4_000_000

// diffLines compares two texts line by line with a longest common subsequence
// and keeps diffContext lines around every change. Lines shared at the start and
// end are matched first; when what is left would need a table over diffBudget
// cells, a one-line summary of the counts is returned instead.
func diffLines(a, b string) []diffLine {
	x, y := splitLines(a), splitLines(b)
	head := 0
	for head < len(x) && head < len(y) && x[head] == y[head] {
		head++
	}
	tail := 0
	for tail < len(x)-head && tail < len(y)-head && x[len(x)-1-tail] == y[len(y)-1-tail] {
		tail++
	}
	mid, midY := x[head:len(x)-tail], y[head:len(y)-tail]
	if len(mid)*len(midY) > diffBudget {
		return []diffLine{{Op: " ", Text: fmt.Sprintf(
			"Too large to compare line by line: %d lines changed or removed, %d changed or added.",
			len(mid), len(midY))}}
	}

	var all []diffLine
	for _, l := range x[:head] {
		all = append(all, diffLine{" ", l})
	}
	all = append(all, lcsDiff(mid, midY)...)
	for _, l := range x[len(x)-tail:] {
		all = append(all, diffLine{" ", l})
	}
	return withContext(all)
}

// lcsDiff is the full line diff of x and y.
func lcsDiff(x, y []string) []diffLine {
	m, n := len(x), len(y)
	w := n + 1
	t := make([]int32, (m+1)*w)
	for i := m - 1; i >= 0; i-- {
		for j := n - 1; j >= 0; j-- {
			if x[i] == y[j] {
				t[i*w+j] = t[(i+1)*w+j+1] + 1
			} else {
				t[i*w+j] = max(t[(i+1)*w+j], t[i*w+j+1])
			}
		}
	}
	var all []diffLine
	i, j := 0, 0
	for i < m && j < n {
		switch {
		case x[i] == y[j]:
			all = append(all, diffLine{" ", x[i]})
			i++
			j++
		case t[(i+1)*w+j] >= t[i*w+j+1]:
			all = append(all, diffLine{"-", x[i]})
			i++
		default:
			all = append(all, diffLine{"+", y[j]})
			j++
		}
	}
	for ; i < m; i++ {
		all = append(all, diffLine{"-", x[i]})
	}
	for ; j < n; j++ {
		all = append(all, diffLine{"+", y[j]})
	}
	return all
}

// withContext keeps the changed lines of a full diff and diffContext lines around
// each, marking what it left out with a gap. It returns nil when nothing changed.
func withContext(all []diffLine) []diffLine {
	keep := make([]bool, len(all))
	changed := false
	for k, l := range all {
		if l.Op == " " {
			continue
		}
		changed = true
		for d := -diffContext; d <= diffContext; d++ {
			if k+d >= 0 && k+d < len(all) {
				keep[k+d] = true
			}
		}
	}
	if !changed {
		return nil
	}
	var out []diffLine
	gap := false
	for k, l := range all {
		if !keep[k] {
			gap = true
			continue
		}
		if gap && len(out) > 0 {
			out = append(out, diffLine{Op: "..."})
		}
		gap = false
		out = append(out, l)
	}
	return out
}

func splitLines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
