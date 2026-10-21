// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

func lines(prefix string, n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "%s %d\n", prefix, i)
	}
	return b.String()
}

func TestDiffLinesSmall(t *testing.T) {
	got := diffLines("a\nb\nc\n", "a\nx\nc\n")
	want := []diffLine{{" ", "a"}, {"-", "b"}, {"+", "x"}, {" ", "c"}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("diff = %v, want %v", got, want)
	}
	if diffLines("a\n", "a\n") != nil {
		t.Error("equal texts must give no diff")
	}
}

func TestDiffLinesLargeInputIsBounded(t *testing.T) {
	a, b := lines("old", 6000), lines("new", 6000)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	got := diffLines(a, b)
	elapsed := time.Since(start)
	runtime.ReadMemStats(&after)

	if elapsed > time.Second {
		t.Errorf("diff took %v", elapsed)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 8<<20 {
		t.Errorf("diff allocated %d bytes", alloc)
	}
	if len(got) != 1 || !strings.Contains(got[0].Text, "Too large to compare line by line") ||
		!strings.Contains(got[0].Text, "6000") {
		t.Errorf("diff = %v, want one summary line with the counts", got)
	}
}

func TestDiffLinesLargeSharedEndsStillCompare(t *testing.T) {
	shared := lines("same", 6000)
	got := diffLines(shared+"old\n"+shared, shared+"new\n"+shared)
	if len(got) > 2*diffContext+2 {
		t.Fatalf("diff has %d lines, want just the change and its context", len(got))
	}
	var del, add int
	for _, l := range got {
		switch l.Op {
		case "-":
			del++
		case "+":
			add++
		}
	}
	if del != 1 || add != 1 {
		t.Errorf("removed %d, added %d; want 1 and 1", del, add)
	}
}
