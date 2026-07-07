package core

import (
	"regexp"
	"strings"
)

var (
	reTrailingWS = regexp.MustCompile(`[ \t]+\n`)
	reBlankRuns  = regexp.MustCompile(`\n{3,}`)

	reCamel   = regexp.MustCompile(`([a-z0-9])([A-Z])`)
	reTypeSep = regexp.MustCompile(`[\s._-]+`)
)

// normalizeType canonicalizes a node/edge type string so that separator and
// case variants of the same intent collapse to one value (#156): `writes-to`,
// `writesTo`, `Writes To` → `writes_to`. Without this, free-text types drift
// across sessions and conflict detection (same from+type → different targets)
// silently misses contradictions written with a different spelling.
//
// It only folds case + separators (camelCase → snake, any run of space/`.`/`-`/`_`
// → a single `_`, lowered, trimmed) — never synonyms, so it can't merge two
// genuinely-distinct types. Pure and idempotent. Empty stays empty (node type is
// optional; an all-separator string like "-" normalizes to "" and is rejected
// upstream where a type is required).
func normalizeType(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = reCamel.ReplaceAllString(s, "${1}_${2}")
	s = strings.ToLower(s)
	s = reTypeSep.ReplaceAllString(s, "_")
	return strings.Trim(s, "_")
}

// normalizeText canonicalizes source whitespace before chunking (#146): CRLF→LF,
// trailing spaces/tabs stripped per line, runs of blank lines collapsed to a
// single blank line, and the whole text trimmed. Poorly-formatted input (e.g. a
// conversion that put a blank line between every line) otherwise gets embedded
// and stored verbatim.
//
// It is a pure, idempotent function of the text, applied once before
// chunk.Split, so boundaries stay content-defined and the self-healing re-ingest
// property is preserved. It touches formatting only — it never drops content
// words, so density scoring and retrieval are unaffected except for the better.
func normalizeText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = reTrailingWS.ReplaceAllString(s, "\n")  // strip trailing spaces/tabs per line…
	s = reBlankRuns.ReplaceAllString(s, "\n\n") // …then collapse blank-line runs
	return strings.TrimSpace(s)
}
