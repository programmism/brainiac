package core

import (
	"regexp"
	"strings"
)

var (
	reTrailingWS = regexp.MustCompile(`[ \t]+\n`)
	reBlankRuns  = regexp.MustCompile(`\n{3,}`)
)

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
