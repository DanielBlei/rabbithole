// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package feedback

import (
	"reflect"
	"testing"
	"time"
)

func TestSelect(t *testing.T) {
	date := func(day int) *time.Time {
		value := time.Date(2026, time.October, day, 12, 0, 0, 0, time.UTC)
		return &value
	}
	firstSeen := func(day int) time.Time {
		return time.Date(2026, time.October, day, 12, 0, 0, 0, time.UTC)
	}
	zeroDate := new(time.Time)
	positive := func(id string, published *time.Time, seen time.Time) Example {
		return Example{ItemID: id, UserScore: 10, PublishedAt: published, FirstSeenAt: seen}
	}
	negative := func(id string, published *time.Time, seen time.Time) Example {
		return Example{ItemID: id, UserScore: 0, PublishedAt: published, FirstSeenAt: seen}
	}

	tests := []struct {
		name     string
		input    []Example
		positive []string
		negative []string
	}{
		{
			name: "empty history",
		},
		{
			name: "only positive feedback",
			input: []Example{
				positive("older", date(1), firstSeen(2)),
				positive("newer", date(3), firstSeen(1)),
			},
			positive: []string{"newer", "older"},
		},
		{
			name: "only negative feedback",
			input: []Example{
				negative("older", date(1), firstSeen(2)),
				negative("newer", date(3), firstSeen(1)),
			},
			negative: []string{"newer", "older"},
		},
		{
			name: "ignores intermediate scores",
			input: []Example{
				{ItemID: "mid-low", UserScore: 1},
				{ItemID: "mid-high", UserScore: 9},
				positive("up", nil, firstSeen(2)),
				negative("down", nil, firstSeen(1)),
			},
			positive: []string{"up"},
			negative: []string{"down"},
		},
		{
			name: "publication date takes precedence over first seen",
			input: []Example{
				positive("old-published", date(1), firstSeen(4)),
				positive("new-published", date(3), firstSeen(1)),
			},
			positive: []string{"new-published", "old-published"},
		},
		{
			name: "falls back to first seen when publication date is missing",
			input: []Example{
				positive("published", date(1), firstSeen(4)),
				positive("undated-new", nil, firstSeen(3)),
				positive("undated-old", nil, firstSeen(2)),
			},
			positive: []string{"undated-new", "undated-old", "published"},
		},
		{
			name: "zero publication date falls back to first seen",
			input: []Example{
				positive("zero-published", zeroDate, firstSeen(2)),
				positive("newer", nil, firstSeen(3)),
			},
			positive: []string{"newer", "zero-published"},
		},
		{
			name: "identical timestamps use item ID tie-break",
			input: []Example{
				positive("b", date(2), firstSeen(1)),
				positive("a", date(2), firstSeen(3)),
				negative("z", nil, firstSeen(2)),
				negative("x", nil, firstSeen(2)),
			},
			positive: []string{"a", "b"},
			negative: []string{"x", "z"},
		},
		{
			name: "caps each polarity at five newest examples",
			input: []Example{
				positive("p1", date(1), firstSeen(1)),
				positive("p2", date(2), firstSeen(2)),
				positive("p3", date(3), firstSeen(3)),
				positive("p4", date(4), firstSeen(4)),
				positive("p5", date(5), firstSeen(5)),
				positive("p6", date(6), firstSeen(6)),
				negative("n1", date(1), firstSeen(1)),
				negative("n2", date(2), firstSeen(2)),
				negative("n3", date(3), firstSeen(3)),
				negative("n4", date(4), firstSeen(4)),
				negative("n5", date(5), firstSeen(5)),
				negative("n6", date(6), firstSeen(6)),
			},
			positive: []string{"p6", "p5", "p4", "p3", "p2"},
			negative: []string{"n6", "n5", "n4", "n3", "n2"},
		},
		{
			name: "fewer than five examples are retained",
			input: []Example{
				positive("one", date(1), firstSeen(1)),
				positive("two", date(2), firstSeen(2)),
			},
			positive: []string{"two", "one"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Select(tt.input)
			if gotIDs := ids(got.Positive); !reflect.DeepEqual(gotIDs, tt.positive) {
				t.Errorf("positive IDs = %v, want %v", gotIDs, tt.positive)
			}
			if gotIDs := ids(got.Negative); !reflect.DeepEqual(gotIDs, tt.negative) {
				t.Errorf("negative IDs = %v, want %v", gotIDs, tt.negative)
			}
			if len(got.Positive) > MaxExamples || len(got.Negative) > MaxExamples {
				t.Errorf("selection exceeded per-polarity cap: %+v", got)
			}
		})
	}
}

func TestSelectPreservesOptionalContent(t *testing.T) {
	note := "worth reading"
	got := Select([]Example{{
		ItemID: "a", Title: "Title", Source: "Source", Summary: "",
		UserScore: 10, UserNote: &note,
	}})
	if len(got.Positive) != 1 {
		t.Fatalf("positive count = %d, want 1", len(got.Positive))
	}
	if got.Positive[0].Summary != "" || got.Positive[0].UserNote == nil || *got.Positive[0].UserNote != note {
		t.Errorf("optional content not preserved: %+v", got.Positive[0])
	}
}

func TestSelectionActive(t *testing.T) {
	example := func(score int) Example { return Example{ItemID: "id", UserScore: score} }
	tests := []struct {
		name     string
		positive int
		negative int
		want     bool
	}{
		{name: "no feedback", positive: 0, negative: 0, want: false},
		{name: "one rating is not enough", positive: 1, negative: 0, want: false},
		{name: "two ratings are not enough", positive: 1, negative: 1, want: false},
		{name: "three one-sided positive ratings", positive: 3, negative: 0, want: true},
		{name: "three one-sided negative ratings", positive: 0, negative: 3, want: true},
		{name: "mixed ratings", positive: 2, negative: 1, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var selected Selection
			for range tt.positive {
				selected.Positive = append(selected.Positive, example(10))
			}
			for range tt.negative {
				selected.Negative = append(selected.Negative, example(0))
			}
			if got := selected.Active(); got != tt.want {
				t.Errorf("Active() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSelectExcluding(t *testing.T) {
	date := func(day int) *time.Time {
		value := time.Date(2026, time.October, day, 12, 0, 0, 0, time.UTC)
		return &value
	}
	positive := func(id string, day int) Example {
		return Example{ItemID: id, UserScore: 10, PublishedAt: date(day)}
	}
	negative := func(id string, day int) Example {
		return Example{ItemID: id, UserScore: 0, PublishedAt: date(day)}
	}
	examples := []Example{
		positive("p1", 1), positive("p2", 2), positive("p3", 3),
		positive("p4", 4), positive("p5", 5), positive("p6", 6),
		negative("n1", 1), negative("n2", 2), negative("n3", 3),
	}

	t.Run("nil exclusion matches Select", func(t *testing.T) {
		if got, want := SelectExcluding(examples, nil), Select(examples); !reflect.DeepEqual(got, want) {
			t.Errorf("SelectExcluding(nil) = %+v, want %+v", got, want)
		}
	})

	t.Run("excludes before capping and backfills", func(t *testing.T) {
		excluded := map[string]struct{}{"p6": {}, "p5": {}, "p4": {}}
		got := SelectExcluding(examples, excluded)
		if gotIDs := ids(got.Positive); !reflect.DeepEqual(gotIDs, []string{"p3", "p2", "p1"}) {
			t.Errorf("positive IDs = %v, want p3, p2, p1 after exclusion", gotIDs)
		}
	})

	t.Run("excludes a matching link under a changed item ID", func(t *testing.T) {
		examples := []Example{
			{ItemID: "fresh-id", Link: "https://x.test/reissued", UserScore: 10, PublishedAt: date(2)},
			{ItemID: "kept", Link: "https://x.test/kept", UserScore: 10, PublishedAt: date(1)},
		}
		excluded := map[string]struct{}{"https://x.test/reissued": {}}
		got := SelectExcluding(examples, excluded)
		if gotIDs := ids(got.Positive); !reflect.DeepEqual(gotIDs, []string{"kept"}) {
			t.Errorf("positive IDs = %v, want only the non-matching link", gotIDs)
		}
	})

	t.Run("mixed feedback keeps active after exclusion", func(t *testing.T) {
		excluded := map[string]struct{}{"p1": {}, "n1": {}}
		got := SelectExcluding(examples, excluded)
		if !got.Active() {
			t.Fatalf("selection is inactive: %+v", got)
		}
		for _, example := range append(got.Positive, got.Negative...) {
			if _, skip := excluded[example.ItemID]; skip {
				t.Errorf("excluded item %s remained in feedback", example.ItemID)
			}
		}
	})

	t.Run("one-sided feedback below threshold after exclusion", func(t *testing.T) {
		oneSided := []Example{positive("p1", 1), positive("p2", 2), positive("p3", 3)}
		excluded := map[string]struct{}{"p1": {}, "p2": {}}
		got := SelectExcluding(oneSided, excluded)
		if got.Active() || len(got.Positive) != 1 || len(got.Negative) != 0 {
			t.Errorf("selection = %+v, want only p3 (inactive)", got)
		}
	})

	t.Run("excluding every example deactivates feedback", func(t *testing.T) {
		excluded := map[string]struct{}{}
		for _, example := range examples {
			excluded[example.ItemID] = struct{}{}
		}
		if got := SelectExcluding(examples, excluded); got.Active() {
			t.Errorf("selection = %+v, want inactive after excluding every example", got)
		}
	})
}

func ids(examples []Example) []string {
	var result []string
	for _, example := range examples {
		result = append(result, example.ItemID)
	}
	return result
}
