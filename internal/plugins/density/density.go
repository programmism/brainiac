// Package density implements the v1 Selector — the "water filter" that decides
// what is allowed into the vector index (SYSTEM.md §7.3, §8, PRD §8).
//
// It runs cheapest-first, no LLM: a structural filter (empty/near-empty) then a
// density heuristic (share of content words, lexical diversity, presence of
// entity-like tokens and numbers). Thresholds are reversible — raw text and the
// score are stored, so the cutoff can move later without re-reading sources.
package density

import (
	"strings"
	"unicode"

	"github.com/programmism/brainiac/internal/plugins"
)

// Defaults for the selection thresholds.
const (
	DefaultMinChars     = 20
	DefaultKeepScore    = 0.45
	DefaultQueueScore   = 0.28
	minTokensForContent = 8
)

// Selector scores chunks by information density.
type Selector struct {
	minChars   int
	keepScore  float64
	queueScore float64
}

// Option customizes a Selector.
type Option func(*Selector)

// WithThresholds overrides the keep/queue score cutoffs.
func WithThresholds(keep, queue float64) Option {
	return func(s *Selector) { s.keepScore, s.queueScore = keep, queue }
}

// WithMinChars overrides the minimum chunk length.
func WithMinChars(n int) Option {
	return func(s *Selector) { s.minChars = n }
}

// New builds a density Selector with the given options.
func New(opts ...Option) *Selector {
	s := &Selector{minChars: DefaultMinChars, keepScore: DefaultKeepScore, queueScore: DefaultQueueScore}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Score returns a quality metric and a keep/queue/drop decision for a chunk.
func (s *Selector) Score(chunk string) plugins.Score {
	trimmed := strings.TrimSpace(chunk)
	if len([]rune(trimmed)) < s.minChars {
		return plugins.Score{Quality: 0, Decision: plugins.Drop}
	}

	tokens := tokenize(trimmed)
	if len(tokens) == 0 {
		return plugins.Score{Quality: 0, Decision: plugins.Drop}
	}

	unique := make(map[string]struct{}, len(tokens))
	content := 0
	for _, t := range tokens {
		unique[t] = struct{}{}
		if !stopwords[t] {
			content++
		}
	}
	diversity := float64(len(unique)) / float64(len(tokens))
	contentRatio := float64(content) / float64(len(tokens))

	quality := 0.55*contentRatio + 0.25*diversity
	if hasDigit(trimmed) {
		quality += 0.10
	}
	if hasEntityLike(trimmed) {
		quality += 0.10
	}
	quality = clamp(quality)

	// Structural: mostly stopwords and short → navigation/filler.
	if contentRatio < 0.2 && len(tokens) < minTokensForContent {
		return plugins.Score{Quality: quality, Decision: plugins.Drop}
	}

	switch {
	case quality >= s.keepScore:
		return plugins.Score{Quality: quality, Decision: plugins.Keep}
	case quality >= s.queueScore:
		return plugins.Score{Quality: quality, Decision: plugins.Queue}
	default:
		return plugins.Score{Quality: quality, Decision: plugins.Drop}
	}
}

// tokenize lowercases and splits on non-alphanumeric runes, keeping tokens of
// length ≥ 2.
func tokenize(s string) []string {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	out := fields[:0]
	for _, f := range fields {
		if len([]rune(f)) >= 2 {
			out = append(out, f)
		}
	}
	return out
}

func hasDigit(s string) bool {
	for _, r := range s {
		if unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// hasEntityLike reports whether s contains a capitalized word that is not at the
// very start of the text (a rough proper-noun / identifier signal).
func hasEntityLike(s string) bool {
	fields := strings.Fields(s)
	for i, f := range fields {
		runes := []rune(f)
		if len(runes) < 2 {
			continue
		}
		if i > 0 && unicode.IsUpper(runes[0]) {
			return true
		}
		// CamelCase / has an interior uppercase (e.g. OrderService).
		for _, r := range runes[1:] {
			if unicode.IsUpper(r) {
				return true
			}
		}
	}
	return false
}

func clamp(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 1:
		return 1
	default:
		return v
	}
}

var _ plugins.Selector = (*Selector)(nil)

// stopwords is a compact English stop list for the content-ratio signal.
var stopwords = func() map[string]bool {
	m := make(map[string]bool)
	for _, w := range strings.Fields(`
		the a an and or but if then else of to in on at by for with from into over
		is are was were be been being do does did has have had this that these those
		it its as we you they he she them our your their i me my mine ours yours
		not no yes can could would should will shall may might must here there when
		where what which who whom how why so than too very just about up down out off
		some any all each every more most other such only own same`) {
		m[w] = true
	}
	return m
}()
