// Package rank defines the scoring interface and the prompt/parse helpers
// shared by the inference backends.
package rank

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/DanielBlei/ai-searcher/internal/feeds"
)

// ItemScore is the relevance verdict for a single item.
type ItemScore struct {
	ID     string
	Score  int    // 0..10
	Reason string // short rationale
}

// Scorer evaluates feed items against an interest profile.
type Scorer interface {
	// Score returns one ItemScore per input item (order not guaranteed).
	Score(ctx context.Context, profile string, items []feeds.Item) ([]ItemScore, error)
	// Validate confirms the backend is reachable and configured correctly.
	Validate(ctx context.Context) error
}

// SystemPrompt instructs the model to emit strict JSON scores.
const SystemPrompt = `You are a personal reading assistant. Given a reader's interest
profile and a list of articles (title + source + summary), rate how worth reading each
article is FOR THIS SPECIFIC READER.

Scoring guide (0-10):
- 9-10: directly on-target, deep, novel, high signal
- 6-8:  relevant and substantive
- 3-5:  tangential or shallow
- 0-2:  off-topic, beginner, clickbait, or marketing

Reward depth, novelty and concrete technical substance. Penalize clickbait, beginner
tutorials, vendor marketing and listicles.

Respond with ONLY a JSON object, no prose, no code fences:
{"scores":[{"index":<int>,"score":<int 0-10>,"reason":"<=15 word rationale"}]}
Include exactly one entry per article, using the article's index.`

// BuildUserPrompt renders the profile and a batch of items into the user message.
// Items are numbered 1..N; the model refers to them by that index.
func BuildUserPrompt(profile string, items []feeds.Item) string {
	var b strings.Builder
	b.WriteString("READER INTEREST PROFILE:\n")
	b.WriteString(strings.TrimSpace(profile))
	b.WriteString("\n\nARTICLES:\n")
	for i, it := range items {
		fmt.Fprintf(&b, "%d. [%s] %s\n", i+1, it.Source, it.Title)
		if it.Summary != "" {
			fmt.Fprintf(&b, "   %s\n", it.Summary)
		}
	}
	return b.String()
}

type rawScores struct {
	Scores []struct {
		Index  int     `json:"index"`
		Score  float64 `json:"score"` // some models emit fractional scores despite the int 0-10 prompt
		Reason string  `json:"reason"`
	} `json:"scores"`
}

// ParseScores extracts the JSON verdict from a model response and maps the
// 1-based indices back onto items. It tolerates code fences and leading/trailing
// prose by slicing to the outermost JSON object. Errors include a snippet of
// the raw response so a misbehaving model's actual output is visible in logs.
func ParseScores(raw string, items []feeds.Item) ([]ItemScore, error) {
	jsonStr, err := extractJSONObject(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: raw=%q", err, truncate(raw, 200))
	}
	var parsed rawScores
	if err := json.Unmarshal([]byte(jsonStr), &parsed); err != nil {
		return nil, fmt.Errorf("parse scores json: %w: raw=%q", err, truncate(jsonStr, 200))
	}
	if len(parsed.Scores) == 0 {
		return nil, fmt.Errorf("no scores in response: raw=%q", truncate(jsonStr, 200))
	}
	out := make([]ItemScore, 0, len(parsed.Scores))
	indices := make([]int, len(parsed.Scores))
	for i, s := range parsed.Scores {
		indices[i] = s.Index
		idx := s.Index - 1
		switch {
		case len(items) == 1:
			// A single-item batch has only one possible target regardless of
			// the index the model reports — small models sometimes hardcode
			// index 0 (or another constant) no matter the array size.
			idx = 0
		case idx < 0 || idx >= len(items):
			continue
		}
		out = append(out, ItemScore{
			ID:     items[idx].ID,
			Score:  clamp(int(math.Round(s.Score)), 0, 10),
			Reason: strings.TrimSpace(s.Reason),
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no valid scores mapped from response: got indices %v, want 1-%d", indices, len(items))
	}
	return out, nil
}

// extractJSONObject returns the substring from the first '{' to the last '}'.
func extractJSONObject(raw string) (string, error) {
	start := strings.IndexByte(raw, '{')
	end := strings.LastIndexByte(raw, '}')
	if start < 0 || end < 0 || end < start {
		return "", fmt.Errorf("no JSON object found in response")
	}
	return raw[start : end+1], nil
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
