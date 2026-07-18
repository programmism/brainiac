// Package chunk splits text with content-defined chunking (a Gear/FastCDC-style
// rolling hash) so chunk boundaries are determined by local content, not
// absolute position. An edit therefore only changes the chunk(s) it touches:
// downstream boundaries re-synchronize on the same content, keeping their text
// (and content hash) identical — so re-ingest re-embeds only the edited region,
// not the whole tail (SYSTEM.md §8, issue #99). Boundaries are snapped to the
// nearest line/word break so chunks never split mid-word or mid-rune.
//
// Each chunk after the first also carries a bounded, sentence-aligned overlap of
// the preceding chunk's tail (#214), so a fact spanning a boundary lands wholly
// in at least one chunk instead of being halved with neither side winning
// retrieval. The overlap is itself a function of local content (the previous
// core's tail), so the self-healing property holds — an edit's blast radius just
// grows by one: the chunk whose overlap it changed. Near-duplicate results the
// overlap can produce are collapsed at retrieval time (#217).
package chunk

import (
	"strings"
	"unicode/utf8"
)

// Tuning (in bytes; ~chars for ASCII).
const (
	minSize    = 400  // no cut before this — bounds tiny chunks
	targetLen  = 1024 // switch masks here to center sizes
	maxSize    = 1600 // forced cut — bounds huge chunks
	snapWindow = 64   // look-back window to align a cut to a word/line break
	overlapMax = 256  // max bytes of the previous chunk's tail carried into the next
)

// Two masks (normalized chunking): a stricter one below the target length makes
// cuts rare (chunks grow toward the target); a looser one above makes them
// likely (chunks cut soon after). This centers the size distribution.
const (
	maskStrict uint64 = (1 << 12) - 1
	maskLoose  uint64 = (1 << 8) - 1
)

// gear is a fixed, deterministic byte→hash table (splitmix64 of the index).
var gear [256]uint64

func init() {
	x := uint64(0)
	for i := range gear {
		x += 0x9E3779B97F4A7C15
		z := x
		z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
		z = (z ^ (z >> 27)) * 0x94D049BB133111EB
		gear[i] = z ^ (z >> 31)
	}
}

// Split breaks text into content-defined chunks (trimmed, non-empty). Every chunk
// after the first is prefixed with a bounded, sentence-aligned overlap of the
// previous chunk's tail (#214).
func Split(text string) []string {
	cores := splitCores([]byte(text))
	if len(cores) <= 1 {
		return cores
	}
	out := make([]string, 0, len(cores))
	out = append(out, cores[0])
	for i := 1; i < len(cores); i++ {
		if ov := overlapTail(cores[i-1]); ov != "" {
			out = append(out, ov+"\n"+cores[i])
		} else {
			out = append(out, cores[i])
		}
	}
	return out
}

// splitCores produces the non-overlapping content-defined pieces (trimmed,
// non-empty) that the overlap is then layered onto.
func splitCores(b []byte) []string {
	cores, _ := splitCoresWithOffsets(b)
	return cores
}

// splitCoresWithOffsets is splitCores that also returns each core's byte offset in
// b (the untrimmed start), for passage-level provenance (#243).
func splitCoresWithOffsets(b []byte) (cores []string, offsets []int) {
	for start := 0; start < len(b); {
		end := nextCut(b, start)
		if piece := strings.TrimSpace(string(b[start:end])); piece != "" {
			cores = append(cores, piece)
			offsets = append(offsets, start)
		}
		start = end
	}
	return cores, offsets
}

// Piece is a chunk plus its passage-level provenance (#243): the byte Offset of
// its content-defined core in the source text, and the nearest preceding Markdown
// Heading (empty if none), so a citation can point at a passage/section, not just
// the whole document.
type Piece struct {
	Text    string
	Offset  int
	Heading string
}

// SplitWithProvenance is Split, but each chunk carries its core's byte offset and
// the nearest preceding Markdown heading. Text matches Split's output 1:1 (same
// overlap), so the content hash / reconcile behavior is unchanged.
func SplitWithProvenance(text string) []Piece {
	b := []byte(text)
	cores, offsets := splitCoresWithOffsets(b)
	if len(cores) == 0 {
		return nil
	}
	pieces := make([]Piece, 0, len(cores))
	for i, core := range cores {
		txt := core
		if i > 0 {
			if ov := overlapTail(cores[i-1]); ov != "" {
				txt = ov + "\n" + core
			}
		}
		pieces = append(pieces, Piece{Text: txt, Offset: offsets[i], Heading: precedingHeading(b, offsets[i])})
	}
	return pieces
}

// precedingHeading returns the text of the last Markdown ATX heading (`#`..`######`
// followed by a space) that starts at or before offset, or "" if none.
func precedingHeading(b []byte, offset int) string {
	if offset > len(b) {
		offset = len(b)
	}
	heading := ""
	lineStart := 0
	for i := 0; i <= offset; i++ {
		if i == offset || (i < len(b) && b[i] == '\n') {
			if h, ok := atxHeading(b[lineStart:i]); ok {
				heading = h
			}
			lineStart = i + 1
		}
	}
	return heading
}

// atxHeading returns the heading text if line is a Markdown ATX heading.
func atxHeading(line []byte) (string, bool) {
	s := strings.TrimLeft(string(line), " ")
	n := 0
	for n < len(s) && s[n] == '#' {
		n++
	}
	if n == 0 || n > 6 || n >= len(s) || s[n] != ' ' {
		return "", false
	}
	// Strip the leading #s and any trailing closing #s / spaces.
	return strings.TrimSpace(strings.Trim(strings.TrimSpace(s[n:]), "#")), true
}

// overlapTail returns the trailing context of prev to carry into the next chunk:
// the last whole sentence(s) within the final overlapMax bytes, falling back to a
// word boundary so it never begins mid-word or mid-rune. Empty if prev has no
// usable tail.
func overlapTail(prev string) string {
	b := []byte(prev)
	from := len(b) - overlapMax
	if from < 0 {
		from = 0
	}
	for from < len(b) && !utf8.RuneStart(b[from]) {
		from++
	}
	w := b[from:]
	return strings.TrimSpace(string(w[overlapStart(w):]))
}

// overlapStart finds where the tail window should begin so it starts at a clean
// boundary: just after the first sentence terminator (. ! ? or newline) if one
// exists, else after the first word break, else the window start.
func overlapStart(w []byte) int {
	for i := 0; i < len(w); i++ {
		switch {
		case w[i] == '\n':
			return i + 1
		case (w[i] == '.' || w[i] == '!' || w[i] == '?') && i+1 < len(w):
			j := i + 1
			for j < len(w) && (w[j] == ' ' || w[j] == '\t' || w[j] == '\n') {
				j++
			}
			if j < len(w) {
				return j
			}
		}
	}
	for i := 0; i < len(w); i++ {
		if w[i] == ' ' || w[i] == '\t' || w[i] == '\n' {
			return i + 1
		}
	}
	return 0
}

// nextCut returns the byte offset where the chunk beginning at start should end.
func nextCut(b []byte, start int) int {
	n := len(b)
	if n-start <= minSize {
		return n
	}
	limit := start + maxSize
	if limit > n {
		limit = n
	}
	target := start + targetLen

	var hash uint64
	i := start
	for ; i < limit; i++ {
		hash = (hash << 1) + gear[b[i]]
		if i-start < minSize {
			continue
		}
		mask := maskStrict
		if i >= target {
			mask = maskLoose
		}
		if hash&mask == 0 {
			i++ // cut after the boundary byte
			break
		}
	}
	return snap(b, start+minSize, i)
}

// snap moves a cut back to the nearest line break (preferred) or whitespace
// within the look-back window, and guarantees a rune boundary. It is purely a
// function of local content, so it preserves the self-healing property.
func snap(b []byte, lo, cut int) int {
	if cut >= len(b) {
		return len(b)
	}
	from := cut - snapWindow
	if from < lo {
		from = lo
	}
	space := -1
	for j := cut; j >= from; j-- {
		if b[j] == '\n' {
			return j
		}
		if space < 0 && (b[j] == ' ' || b[j] == '\t') {
			space = j
		}
	}
	if space >= 0 {
		return space
	}
	for cut > lo && !utf8.RuneStart(b[cut]) {
		cut--
	}
	return cut
}
