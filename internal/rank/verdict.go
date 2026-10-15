// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package rank

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"unicode"

	"github.com/rs/zerolog"

	"github.com/DanielBlei/rabbithole/internal/feeds"
)

// minReasonLetters is the fewest letters a real rationale has; junk like ":"
// or ":0" has none, while "Off topic." or a short CJK reason clears it.
const minReasonLetters = 3

// jsonDebris are fragments of the verdict's own JSON that only end up inside a
// reason when the model lost track of where it was in the output.
var jsonDebris = []string{"}]", "]}", `{"`, `"index"`, `"score"`}

// degenerateReason names the rule a reason breaks, or returns "" for a usable one.
func degenerateReason(reason string) string {
	for _, d := range jsonDebris {
		if strings.Contains(reason, d) {
			return "json debris in reason"
		}
	}
	letters := 0
	for _, r := range reason {
		if unicode.IsLetter(r) {
			letters++
		}
	}
	if letters < minReasonLetters {
		return "reason has no words"
	}
	// A desynced model starts the reason where it expected the key's colon.
	if strings.HasPrefix(strings.TrimSpace(reason), ":") {
		return "reason starts with a colon"
	}
	return ""
}

// verdictGuard drops verdicts with an unusable reason, so the retry paths in
// scoreBatch and scoreOne re-ask instead of storing them as real scores.
type verdictGuard struct {
	Scorer
	rejected atomic.Int64
}

func (g *verdictGuard) Score(ctx context.Context, profile string, items []feeds.Item) ([]ItemScore, error) {
	scores, err := g.Scorer.Score(ctx, profile, items)
	if err != nil {
		return nil, err
	}
	titles := make(map[string]string, len(items))
	for _, it := range items {
		titles[it.ID] = it.Title
	}
	kept := make([]ItemScore, 0, len(scores))
	var lastErr error
	for _, sc := range scores {
		rule := degenerateReason(sc.Reason)
		if rule == "" {
			kept = append(kept, sc)
			continue
		}
		g.rejected.Add(1)
		lastErr = fmt.Errorf("unusable verdict (%s): reason=%q", rule, truncate(sc.Reason, 80))
		zerolog.Ctx(ctx).Warn().Str("item", truncate(titles[sc.ID], 80)).Str("rule", rule).
			Str("reason", truncate(sc.Reason, 80)).Msg("dropping unusable verdict, re-asking")
	}
	if len(kept) == 0 && lastErr != nil {
		return nil, lastErr
	}
	return kept, nil
}
