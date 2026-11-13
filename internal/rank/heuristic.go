// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package rank

import (
	"context"
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/DanielBlei/rabbithole/internal/feeds"
)

// Heuristic is a model-free Scorer that ranks by keyword overlap with the
// profile. It exists for offline testing and as a zero-dependency fallback.
type Heuristic struct{}

// NewHeuristic returns a keyword-overlap scorer.
func NewHeuristic() *Heuristic { return &Heuristic{} }

// Validate always succeeds; the heuristic scorer has no backend.
func (h *Heuristic) Validate(context.Context) error { return nil }

var (
	// function words and prose filler that carry no topical signal
	stop = wordSet(`a about above after again against all am an and any are arent as at be because been
		before being below between both but by cant cannot could couldnt did didnt do does doesnt doing dont
		down during each few for from further had hadnt has hasnt have havent having he hed hell hes her here
		heres hers herself him himself his how hows i id ill im ive if in into is isnt it its itself lets me
		more most mustnt my myself no nor not of off on once only or other ought our ours ourselves out over
		own same shant she shed shell shes should shouldnt so some such than that thats the their theirs them
		themselves then there theres these they theyd theyll theyre theyve this those through to too under
		until up very was wasnt we wed well were werent weve what whats when whens where wheres which while
		who whom whos why whys will with wont would wouldnt you youd youll youre youve your yours yourself
		yourselves new things something ideally anything everything interested score articles profile`)
	pluralKeep = wordSet("news")
	spellingRE = regexp.MustCompile(`^(\p{L}{3,})is(ation|ations|e|ed|er|ers|ing)$`)

	negativeWords = wordSet("not no dont never less least avoid skip dislike hate boring uninteresting exclude ignore")
	weakWords     = wordSet("somewhat moderately moderate secondary sometimes")
	neutralWords  = wordSet("context notes about background")
)

func wordSet(s string) map[string]bool {
	m := make(map[string]bool)
	for _, w := range strings.Fields(s) {
		m[w] = true
	}
	return m
}

type sectionKind int

const (
	sectionPositive sectionKind = iota
	sectionWeak
	sectionNegative
	sectionNeutral
)

// headingKind classifies a heading by its words. A title (level 1) opens the
// introduction, which weighs like a weak section.
func headingKind(text string, level int) sectionKind {
	words := strings.FieldsFunc(strings.ToLower(strings.ReplaceAll(text, "'", "")), func(r rune) bool {
		return r < 'a' || r > 'z'
	})
	for _, w := range words {
		if negativeWords[w] {
			return sectionNegative
		}
	}
	for _, w := range words {
		if weakWords[w] {
			return sectionWeak
		}
	}
	if level == 1 {
		return sectionWeak
	}
	for _, w := range words {
		if neutralWords[w] {
			return sectionNeutral
		}
	}
	return sectionPositive
}

// profileTerms maps each keyword the profile asks for to its weight. Text
// before the first heading and weak sections weigh 1, positive sections 2.
// Negative and neutral sections add nothing, and a term also listed under a
// negative heading stays positive.
func profileTerms(profile string) map[string]int {
	pos := make(map[string]int)
	kind := sectionWeak
	for _, line := range strings.Split(profile, "\n") {
		t := strings.TrimSpace(line)
		if level := len(
			t,
		) - len(
			strings.TrimLeft(t, "#"),
		); level >= 1 && level <= 3 &&
			(len(t) == level || t[level] == ' ') {
			kind = headingKind(t[level:], level)
			continue
		}
		if kind == sectionNeutral || kind == sectionNegative {
			continue
		}
		weight := 2
		if kind == sectionWeak {
			weight = 1
		}
		for _, w := range terms(line, true) {
			if pos[w.norm] < weight {
				pos[w.norm] = weight
			}
		}
	}
	return pos
}

// scoreK is the half-saturation point of the score curve: raw points equal
// to it score 5.
const scoreK = 6

// hit is a profile keyword found in an item.
type hit struct {
	word   string
	points int
	title  bool
}

// Score rates each item by the weighted profile keywords it contains. A term
// counts once, at its best field (title 2, summary 1), times the weight of the
// profile section it came from, and the total is squashed onto 0..10.
func (h *Heuristic) Score(_ context.Context, profile string, items []feeds.Item) ([]ItemScore, error) {
	keywords := profileTerms(profile)
	out := make([]ItemScore, 0, len(items))
	for _, it := range items {
		hits := make(map[string]*hit)
		collect := func(text string, fieldPoints int, title bool) {
			for _, w := range terms(text, false) {
				weight, ok := keywords[w.norm]
				if !ok || hits[w.norm] != nil {
					continue
				}
				hits[w.norm] = &hit{word: w.orig, points: fieldPoints * weight, title: title}
			}
		}
		collect(it.Title, 2, true)
		collect(it.Summary, 1, false)
		matched := make([]hit, 0, len(hits))
		raw := 0
		for _, ht := range hits {
			matched = append(matched, *ht)
			raw += ht.points
		}
		sort.Slice(matched, func(i, j int) bool {
			a, b := matched[i], matched[j]
			if a.points != b.points {
				return a.points > b.points
			}
			if a.title != b.title {
				return a.title
			}
			return a.word < b.word
		})
		reason := "no keyword overlap"
		if len(matched) > 0 {
			if len(matched) > 4 {
				matched = matched[:4]
			}
			words := make([]string, len(matched))
			for i, ht := range matched {
				words[i] = ht.word
			}
			reason = "matched: " + strings.Join(words, ", ")
		}
		score := int(math.Round(10 * float64(raw) / float64(raw+scoreK)))
		out = append(out, ItemScore{ID: it.ID, Score: clamp(score, 0, 10), Reason: reason})
	}
	return out, nil
}

// term is a word as written and its normalized matching form.
type term struct{ orig, norm string }

// terms splits s into keyword candidates. Words of two letters are kept only
// when capitalized, so AI and ML survive while "to" and "go deeper" do not;
// in a profile a capitalized Go counts only away from the start of a sentence.
func terms(s string, profile bool) []term {
	var out []term
	var run []rune
	start := true
	flush := func(first bool) {
		if len(run) == 0 {
			return
		}
		w := string(run)
		run = run[:0]
		if keepWord(w, profile && first) {
			lw := strings.ToLower(w)
			if n := normalize(lw); !stop[lw] && !stop[n] {
				if strings.ToUpper(w) == w {
					lw = w
				}
				out = append(out, term{orig: lw, norm: n})
			}
		}
	}
	first := true
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsMark(r):
			run = append(run, r)
			if len(run) == 1 {
				first = start
			}
			start = false
		case (r == '\'' || r == '\u2019') && len(run) > 0:
		default:
			flush(first)
			if r == '.' || r == '!' || r == '?' || r == ':' || r == '\n' {
				start = true
			}
		}
	}
	flush(first)
	return out
}

// keepWord applies the minimum length rule.
func keepWord(w string, sentenceStart bool) bool {
	switch utf8.RuneCountInString(w) {
	case 0, 1:
		return false
	case 2:
		if w == "Go" {
			return !sentenceStart
		}
		return strings.ToUpper(w) == w && strings.ToLower(w) != w
	}
	return true
}

// normalize folds a lowercase word to a form shared by its plural and by
// British and American spellings.
func normalize(w string) string {
	return spellingRE.ReplaceAllString(singular(w), "${1}iz$2")
}

func singular(w string) string {
	if utf8.RuneCountInString(w) <= 3 || pluralKeep[w] {
		return w
	}
	switch {
	case strings.HasSuffix(w, "ies") && !strings.HasSuffix(w, "eies") && !strings.HasSuffix(w, "aies"):
		return w[:len(w)-3] + "y"
	case strings.HasSuffix(w, "es") && !hasAnySuffix(w, "aes", "ees", "oes"):
		return w[:len(w)-1]
	case strings.HasSuffix(w, "s") && !hasAnySuffix(w, "us", "ss", "is"):
		return w[:len(w)-1]
	}
	return w
}

func hasAnySuffix(w string, suffixes ...string) bool {
	for _, s := range suffixes {
		if strings.HasSuffix(w, s) {
			return true
		}
	}
	return false
}
