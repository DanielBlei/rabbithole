// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

// Package feedback selects explicit user ratings as examples for future
// adaptive scoring.
package feedback

import (
	"sort"
	"time"
)

// MaxExamples is the maximum number of examples selected for either polarity.
const MaxExamples = 5

// MinExamples is the fewest explicit ratings that activate adaptive scoring.
// Two or fewer ratings could be a single accidental click, while three is
// enough signal to be useful and still permits one-sided feedback.
const MinExamples = 3

// Example is an explicitly rated item and the content needed to represent it.
type Example struct {
	ItemID      string
	Link        string
	Title       string
	Source      string
	Summary     string
	UserScore   int
	UserNote    *string
	PublishedAt *time.Time
	FirstSeenAt time.Time
}

// Selection holds the most recent positive and negative examples. Ratings
// other than the explicit thumbs-up and thumbs-down values are ignored.
type Selection struct {
	Positive []Example
	Negative []Example
}

// Active reports whether the selection carries enough explicit ratings to be
// used as adaptive scoring context.
func (s Selection) Active() bool {
	return len(s.Positive)+len(s.Negative) >= MinExamples
}

// Select returns at most MaxExamples for each polarity, newest item first.
// Publication date determines recency when present; otherwise first-seen date
// is used. Item ID provides a stable tie-break for identical timestamps.
func Select(examples []Example) Selection {
	return SelectExcluding(examples, nil)
}

// SelectExcluding is Select with any example whose item ID or link is in
// excluded removed before ordering and capping. Callers pass the operation's
// full candidate set so an article never appears as an example for its own
// score, including when a feed reissues the same link under a new GUID.
// Filtering before the caps keeps the selection deterministic and lets Active
// report the true number of usable ratings.
func SelectExcluding(examples []Example, excluded map[string]struct{}) Selection {
	var selected Selection
	for _, example := range examples {
		if isExcluded(example, excluded) {
			continue
		}
		switch example.UserScore {
		case 10:
			selected.Positive = append(selected.Positive, example)
		case 0:
			selected.Negative = append(selected.Negative, example)
		}
	}
	selectRecent(selected.Positive)
	selectRecent(selected.Negative)
	selected.Positive = capExamples(selected.Positive)
	selected.Negative = capExamples(selected.Negative)
	return selected
}

func isExcluded(example Example, excluded map[string]struct{}) bool {
	if _, skip := excluded[example.ItemID]; skip {
		return true
	}
	if example.Link == "" {
		return false
	}
	_, skip := excluded[example.Link]
	return skip
}

func selectRecent(examples []Example) {
	sort.Slice(examples, func(i, j int) bool {
		left, right := itemDate(examples[i]), itemDate(examples[j])
		if !left.Equal(right) {
			return left.After(right)
		}
		return examples[i].ItemID < examples[j].ItemID
	})
}

func itemDate(example Example) time.Time {
	if example.PublishedAt != nil && !example.PublishedAt.IsZero() {
		return *example.PublishedAt
	}
	return example.FirstSeenAt
}

func capExamples(examples []Example) []Example {
	if len(examples) > MaxExamples {
		examples = examples[:MaxExamples]
	}
	return examples
}
