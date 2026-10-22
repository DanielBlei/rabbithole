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

// Example is an explicitly rated item and the content needed to represent it.
type Example struct {
	ItemID      string
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

// Select returns at most MaxExamples for each polarity, newest item first.
// Publication date determines recency when present; otherwise first-seen date
// is used. Item ID provides a stable tie-break for identical timestamps.
func Select(examples []Example) Selection {
	var selected Selection
	for _, example := range examples {
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
