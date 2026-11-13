// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package rank

import (
	"context"
	"strings"
	"testing"

	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/profile"
)

const defaultProfile = `# Reading interest profile

I'm an AI enthusiast. Score articles by how interesting they'd be to me, and for keeping up
with where things are going.

## Interested
- Running open-source language models, ideally on my own machine
- Practical write-ups: how something was built, what worked, what didn't
- Kubernetes and cloud-native infrastructure
- New models and tools worth trying out
- The systems work underneath inference: kernels, quantisation, memory, schedulers

## Somewhat interested
- Machine learning research with a clear practical angle
- Developer tooling

## Not so interested
- Beginner tutorials and "what is an LLM" explainers
- Anything outside the lists above, however well written

## Not for me
- Marketing/press releases with no technical details
- Listicles and clickbait ("X will change everything")
- Technical details of business and strategy takes

Prefer depth and substance over popularity.
`

func heuristicScore(t *testing.T, prof, title, summary string) ItemScore {
	t.Helper()
	got, err := NewHeuristic().Score(context.Background(), prof, []feeds.Item{{ID: "x", Title: title, Summary: summary}})
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	return got[0]
}

func TestHeuristicScoresOverlap(t *testing.T) {
	h := NewHeuristic()
	prof := "I care about vLLM inference, RAG retrieval and Kubernetes serving."
	items := []feeds.Item{
		{ID: "match", Title: "vLLM inference on Kubernetes", Summary: "RAG retrieval tips"},
		{ID: "none", Title: "Sourdough baking guide", Summary: "flour and water"},
	}
	scores, err := h.Score(context.Background(), prof, items)
	if err != nil {
		t.Fatalf("heuristic score: %v", err)
	}
	byID := map[string]ItemScore{}
	for _, s := range scores {
		byID[s.ID] = s
	}
	if byID["match"].Score == 0 {
		t.Error("expected non-zero score for overlapping item")
	}
	if byID["none"].Score != 0 {
		t.Errorf("expected zero score for unrelated item, got %d", byID["none"].Score)
	}
	if byID["match"].Score <= byID["none"].Score {
		t.Error("matching item should outscore unrelated item")
	}
}

func TestHeuristicDefaultProfileOrdering(t *testing.T) {
	onTopic := heuristicScore(
		t,
		defaultProfile,
		"Serving models with vLLM on Kubernetes",
		"Quantization and schedulers for local inference",
	)
	press := heuristicScore(t, defaultProfile, "Announcing our press release", "Marketing for a business strategy")
	bread := heuristicScore(t, defaultProfile, "Sourdough baking guide", "flour and water")
	if onTopic.Score <= press.Score {
		t.Errorf("on-topic %d should outscore marketing %d", onTopic.Score, press.Score)
	}
	if press.Score > 1 || bread.Score > 1 {
		t.Errorf("marketing %d and sourdough %d should score near zero", press.Score, bread.Score)
	}
	deep := heuristicScore(t, defaultProfile, "A deep look at LLM serving", "technical depth on kernels")
	if !strings.Contains(deep.Reason, "kernel") {
		t.Errorf("negative bullets must not remove positive terms: %q", deep.Reason)
	}
	llm := heuristicScore(t, defaultProfile, "technical details", "depth and substance")
	if llm.Score != 0 {
		t.Errorf("words only in negative or closing prose score 0, got %d (%s)", llm.Score, llm.Reason)
	}
}

func TestHeuristicHeadings(t *testing.T) {
	canon := func(interested, less string) string {
		return profile.RenderCanonical(profile.EditorFields{Interested: interested, Less: less})
	}
	tests := []struct {
		name    string
		prof    string
		title   string
		want    bool
		wantNot bool
	}{
		{name: "interests", prof: "## Interests\n- rust compilers", title: "Rust compilers", want: true},
		{name: "interested", prof: "## Interested\n- rust compilers", title: "Rust compilers", want: true},
		{
			name:  "topics",
			prof:  "## Topics\n- rust compilers\n## Not for me\n- gardening",
			title: "gardening",
			want:  false,
		},
		{name: "dont like", prof: "## Things I don't like\n- gardening", title: "gardening", want: false},
		{name: "uninteresting", prof: "## Uninteresting\n- gardening", title: "gardening", want: false},
		{
			name:  "canonical interested",
			prof:  canon("- rust compilers", "- gardening"),
			title: "Rust compilers",
			want:  true,
		},
		{name: "canonical less", prof: canon("- rust compilers", "- gardening"), title: "gardening", want: false},
		{name: "no headings", prof: "rust compilers", title: "Rust compilers", want: true},
		{name: "only negative", prof: "## Not for me\n- gardening", title: "gardening", want: false},
		{name: "empty", prof: "", title: "gardening", want: false},
		{name: "negative prose ignored", prof: "## Not for me\nI dislike gardening", title: "gardening", want: false},
		{
			name:  "both lists positive wins",
			prof:  "## Interested\n- kubernetes\n## Less interested\n- kubernetes beginner tutorials",
			title: "kubernetes",
			want:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := heuristicScore(t, tt.prof, tt.title, "")
			if (got.Score > 0) != tt.want {
				t.Errorf("score %d (%s), want match=%v", got.Score, got.Reason, tt.want)
			}
		})
	}
}

func TestHeuristicHeadingWeights(t *testing.T) {
	pos := heuristicScore(t, "## Interested\n- rust", "rust", "")
	weak := heuristicScore(t, "## Somewhat interested\n- rust", "rust", "")
	pre := heuristicScore(t, "rust", "rust", "")
	if pos.Score <= weak.Score {
		t.Errorf("positive %d should outscore weak %d", pos.Score, weak.Score)
	}
	if weak.Score != pre.Score {
		t.Errorf("preamble %d should weigh like weak %d", pre.Score, weak.Score)
	}
	neutral := heuristicScore(t, "## Additional context\n- rust", "rust", "")
	if neutral.Score != 0 {
		t.Errorf("context section should be ignored, got %d", neutral.Score)
	}
}

func TestHeuristicShortWords(t *testing.T) {
	if got := heuristicScore(
		t,
		"I'm an AI enthusiast.\n## Interested\n- rust",
		"AI news",
		"",
	); got.Score == 0 ||
		!strings.Contains(got.Reason, "AI") {
		t.Errorf("preamble should supply AI: %+v", got)
	}
	if got := heuristicScore(t, "## Interested\n- AI and ML", "ML tooling", ""); got.Score == 0 {
		t.Errorf("capitalised ML should match: %+v", got)
	}
	if got := heuristicScore(t, "## Interested\n- go deeper into rust", "Go release", ""); got.Score != 0 {
		t.Errorf("lowercase go must not become a term: %+v", got)
	}
	if got := heuristicScore(t, "## Interested\n- I write Go services", "Go release", ""); got.Score == 0 {
		t.Errorf("Go language should match: %+v", got)
	}
	if got := heuristicScore(t, "## Interested\n- Go services", "Go release", ""); got.Score != 0 {
		t.Errorf("Go opening a sentence is not the language: %+v", got)
	}
}

func TestNormalize(t *testing.T) {
	same := [][]string{
		{"release", "releases"}, {"use", "uses"}, {"cache", "caches"}, {"model", "models"},
		{"class", "classes"}, {"process", "processes"},
		{"scheduler", "schedulers"}, {"promise", "promises"},
		{"quantised", "quantized"}, {"quantising", "quantizing"}, {"quantisation", "quantization"},
		{"optimiser", "optimizer"}, {"policy", "policies"},
	}
	for _, p := range same {
		if a, b := normalize(p[0]), normalize(p[1]); a != b {
			t.Errorf("normalize(%q)=%q != normalize(%q)=%q", p[0], a, p[1], b)
		}
	}
	unchanged := []string{"news", "ios", "aws", "dns", "ops", "analysis", "status", "class", "go"}
	for _, w := range unchanged {
		if got := normalize(w); got != w {
			t.Errorf("normalize(%q) = %q, want unchanged", w, got)
		}
	}
}

func TestHeuristicReasons(t *testing.T) {
	prof := "## Interested\n- quantisation and schedulers\n- inference"
	got := heuristicScore(t, prof, "Quantization of models", "new schedulers for inference")
	for _, w := range []string{"quantization", "schedulers", "inference"} {
		if !strings.Contains(got.Reason, w) {
			t.Errorf("reason %q should show original spelling %q", got.Reason, w)
		}
	}
	if strings.Contains(got.Reason, "quantisation") {
		t.Errorf("reason shows the profile spelling, not the item's: %q", got.Reason)
	}
	if none := heuristicScore(t, prof, "Sourdough", "bread"); none.Reason != "no keyword overlap" {
		t.Errorf("reason = %q", none.Reason)
	}
	many := heuristicScore(t, "## Interested\n- alpha bravo charlie delta echo", "alpha bravo charlie delta echo", "")
	if n := len(strings.Split(strings.TrimPrefix(many.Reason, "matched: "), ", ")); n != 4 {
		t.Errorf("reason lists %d words, want 4: %q", n, many.Reason)
	}
}

func TestHeuristicFieldWeights(t *testing.T) {
	prof := "## Interested\n- rust"
	both := heuristicScore(t, prof, "rust", "rust rust")
	titleOnly := heuristicScore(t, prof, "rust", "")
	summaryOnly := heuristicScore(t, prof, "", "rust")
	if both.Score != titleOnly.Score {
		t.Errorf("a term counts once: both %d, title %d", both.Score, titleOnly.Score)
	}
	if titleOnly.Score <= summaryOnly.Score {
		t.Errorf("title %d should outscore summary %d", titleOnly.Score, summaryOnly.Score)
	}
}

func TestHeuristicFixedScale(t *testing.T) {
	prof := "## Interested\n- alpha bravo charlie delta"
	if got := heuristicScore(t, prof, "alpha", "").Score; got != 4 {
		t.Errorf("one title hit = %d, want 4", got)
	}
	if got := heuristicScore(t, prof, "alpha bravo charlie", "").Score; got != 7 {
		t.Errorf("three title hits = %d, want 7", got)
	}
	if got := heuristicScore(
		t,
		prof,
		"alpha bravo charlie delta",
		"alpha bravo charlie delta",
	).Score; got < 0 ||
		got > 10 {
		t.Errorf("score %d out of range", got)
	}
}

func TestHeuristicScoreIndependentOfBatch(t *testing.T) {
	prof := "## Interested\n- alpha bravo"
	target := feeds.Item{ID: "t", Title: "alpha bravo", Summary: "x"}
	h := NewHeuristic()
	alone, _ := h.Score(context.Background(), prof, []feeds.Item{target})
	batch, _ := h.Score(context.Background(), prof, []feeds.Item{
		{ID: "a", Title: "alpha alpha alpha bravo bravo"}, target, {ID: "b", Title: "nothing"},
	})
	if alone[0].Score != batch[1].Score {
		t.Errorf("score changed with batch: %d vs %d", alone[0].Score, batch[1].Score)
	}
}

func TestHeuristicUnicode(t *testing.T) {
	composed := "café"
	decomposed := "café"
	if got := heuristicScore(t, "## Interested\n- "+composed, "Best "+composed, ""); got.Score == 0 {
		t.Errorf("accented word should match itself: %+v", got)
	}
	if got := heuristicScore(t, "## Interested\n- "+decomposed, "Best "+decomposed, ""); got.Score == 0 {
		t.Errorf("decomposed accent should stay in the word: %+v", got)
	}
	if got := heuristicScore(t, "## Interested\n- 机器学习", "机器学习入门", ""); got.Score != 0 {
		t.Logf("CJK runs stay whole tokens, so a longer run does not match: %+v", got)
	}
	if got := heuristicScore(t, "## Interested\n- 机器学习", "机器学习 news", ""); got.Score == 0 {
		t.Errorf("CJK token should match: %+v", got)
	}
}

func TestHeuristicIntroUnderTheTitleSuppliesKeywords(t *testing.T) {
	got := profileTerms(profile.DefaultContent)
	if got["ai"] != 1 {
		t.Fatalf("ai weight = %d, want 1 from the introduction", got["ai"])
	}
}

func TestHeuristicNestedNegativeHeadings(t *testing.T) {
	prof := `# Avoid
## Topics
- gardening
## Never
### Interests
- baking
# Interests
- rust
## Not for me
### Topics
- marketing
## Interested
- compilers`
	for _, word := range []string{"gardening", "baking", "marketing"} {
		if got := heuristicScore(t, prof, word, ""); got.Score != 0 {
			t.Errorf("excluded descendant %q scored %d", word, got.Score)
		}
	}
	for _, word := range []string{"rust", "compilers"} {
		if got := heuristicScore(t, prof, word, ""); got.Score == 0 {
			t.Errorf("new section %q did not score", word)
		}
	}
}

func TestHeuristicSingularPluralScores(t *testing.T) {
	for _, pair := range [][2]string{{"class", "classes"}, {"process", "processes"}, {"cache", "caches"}} {
		for _, forms := range [][2]string{pair, {pair[1], pair[0]}} {
			prof := "## Interested\n- " + forms[0]
			got := heuristicScore(t, prof, forms[1], "")
			want := heuristicScore(t, prof, forms[0], "")
			if got.Score == 0 || got.Score != want.Score {
				t.Errorf("%q matching %q: score %d, want %d", forms[0], forms[1], got.Score, want.Score)
			}
		}
	}
}
