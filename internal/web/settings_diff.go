// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import "strings"

// diffLine is one line of a unified-style diff. Op is "+", "-", " " or "..." for
// a gap where unchanged lines were left out.
type diffLine struct {
	Op   string
	Text string
}

// diffContext is how many unchanged lines stay around each change.
const diffContext = 2

// diffLines compares two texts line by line with a longest common subsequence
// and keeps diffContext lines around every change. Configs are a few dozen
// lines, so the quadratic table is fine.
func diffLines(a, b string) []diffLine {
	x, y := splitLines(a), splitLines(b)
	m, n := len(x), len(y)
	t := make([][]int, m+1)
	for i := range t {
		t[i] = make([]int, n+1)
	}
	for i := m - 1; i >= 0; i-- {
		for j := n - 1; j >= 0; j-- {
			if x[i] == y[j] {
				t[i][j] = t[i+1][j+1] + 1
			} else {
				t[i][j] = max(t[i+1][j], t[i][j+1])
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
		case t[i+1][j] >= t[i][j+1]:
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
