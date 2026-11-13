// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"strings"
	"testing"

	"github.com/DanielBlei/rabbithole/internal/store"
)

func TestToRowShowsFeedbackProvenance(t *testing.T) {
	score := 8
	positive, negative := 3, 2
	hash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	row := store.ItemRow{
		ID: "a", Source: "S", Title: "A", Link: "https://x/a",
		LLMScore: &score, LLMFeedbackPositive: &positive, LLMFeedbackNegative: &negative,
		LLMFeedbackHash: &hash,
	}
	got := toRow(row)
	if !strings.Contains(got.ScoreFeedback, "feedback 3P/2N @0123456789ab") {
		t.Errorf("ScoreFeedback = %q", got.ScoreFeedback)
	}
}

func TestToRowOmitsFeedbackProvenanceWithoutFeedback(t *testing.T) {
	score := 8
	got := toRow(store.ItemRow{ID: "a", Source: "S", Title: "A", Link: "https://x/a", LLMScore: &score})
	if got.ScoreFeedback != "" {
		t.Errorf("ScoreFeedback = %q, want empty", got.ScoreFeedback)
	}
}
