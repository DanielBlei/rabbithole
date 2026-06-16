package rank

import (
	"context"
	"regexp"
	"sort"
	"strings"

	"github.com/DanielBlei/ai-searcher/internal/feeds"
)

// Heuristic is a model-free Scorer that ranks by keyword overlap with the
// profile. It exists for offline testing and as a zero-dependency fallback.
type Heuristic struct{}

// NewHeuristic returns a keyword-overlap scorer.
func NewHeuristic() *Heuristic { return &Heuristic{} }

// Validate always succeeds; the heuristic scorer has no backend.
func (h *Heuristic) Validate(context.Context) error { return nil }

var (
	wordRE = regexp.MustCompile(`[a-z0-9]+`)
	// common words that carry no topical signal
	stop = map[string]bool{
		"the": true, "and": true, "for": true, "with": true, "that": true, "this": true,
		"are": true, "not": true, "you": true, "have": true, "from": true, "over": true,
		"into": true, "out": true, "about": true, "what": true, "your": true, "more": true,
		"score": true, "high": true, "low": true, "medium": true, "interested": true,
	}
)

// Score rates each item by how many distinct profile keywords it contains.
func (h *Heuristic) Score(_ context.Context, profile string, items []feeds.Item) ([]ItemScore, error) {
	keywords := tokenize(profile)
	out := make([]ItemScore, 0, len(items))
	for _, it := range items {
		text := tokenizeText(it.Title + " " + it.Summary)
		var matched []string
		for kw := range keywords {
			if text[kw] {
				matched = append(matched, kw)
			}
		}
		sort.Strings(matched)
		score := clamp(len(matched), 0, 10)
		reason := "no keyword overlap"
		if len(matched) > 0 {
			top := matched
			if len(top) > 4 {
				top = top[:4]
			}
			reason = "matched: " + strings.Join(top, ", ")
		}
		out = append(out, ItemScore{ID: it.ID, Score: score, Reason: reason})
	}
	return out, nil
}

func tokenize(s string) map[string]bool {
	set := make(map[string]bool)
	for _, w := range wordRE.FindAllString(strings.ToLower(s), -1) {
		if len(w) >= 3 && !stop[w] {
			set[w] = true
		}
	}
	return set
}

func tokenizeText(s string) map[string]bool { return tokenize(s) }
