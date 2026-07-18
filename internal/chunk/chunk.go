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
//
// Chunking is also structure-aware (#242): a boundary is never placed inside a
// fenced code block or a Markdown table when the block fits within maxSize, so
// code and tables aren't halved mid-structure (which mangles both the rendering
// and the embedding). A block larger than maxSize instead begins its own chunk; a
// block that both starts a chunk and still exceeds maxSize is split at a
// *structured* boundary — a blank line, else a top-level line start (a symbol
// boundary, and every row start of a pipe table) — rather than an arbitrary
// rolling-hash cut, so a big code file or table breaks into coherent pieces
// (#350). Atomic regions are a pure function of content, so self-healing holds.
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

// Params tunes the chunker's size bounds in bytes (#401), so a source can pick a
// strategy — e.g. larger chunks for prose, smaller for code — without changing the
// algorithm or its structure-awareness. Zero fields fall back to the package
// defaults, and any set that violates 0 < MinSize < TargetLen <= MaxSize (or a
// non-positive OverlapMax) is rejected wholesale back to the defaults, so a
// misconfiguration can never break the size invariant. The default reproduces the
// original constants exactly, so SplitWithProvenance is byte-for-byte unchanged.
type Params struct {
	MinSize    int
	TargetLen  int
	MaxSize    int
	OverlapMax int
}

// DefaultParams is the built-in tuning (the original constants).
func DefaultParams() Params {
	return Params{MinSize: minSize, TargetLen: targetLen, MaxSize: maxSize, OverlapMax: overlapMax}
}

// Preset maps a named chunking strategy to its Params (#401): "prose" for larger
// chunks over narrative docs, "code" for tighter chunks over source/config files.
// An empty or unknown name returns the default tuning, so a misconfigured preset is
// harmless.
func Preset(name string) Params {
	switch name {
	case "prose":
		return Params{MinSize: 600, TargetLen: 1600, MaxSize: 2600, OverlapMax: 320}
	case "code":
		return Params{MinSize: 300, TargetLen: 768, MaxSize: 1200, OverlapMax: 200}
	default:
		return DefaultParams()
	}
}

// withDefaults fills zero fields from the defaults and, if the result is not a
// valid ordering, falls back to the defaults entirely — the invariant safety net.
func (p Params) withDefaults() Params {
	if p.MinSize <= 0 {
		p.MinSize = minSize
	}
	if p.TargetLen <= 0 {
		p.TargetLen = targetLen
	}
	if p.MaxSize <= 0 {
		p.MaxSize = maxSize
	}
	if p.OverlapMax <= 0 {
		p.OverlapMax = overlapMax
	}
	if p.MinSize >= p.TargetLen || p.TargetLen > p.MaxSize {
		return DefaultParams()
	}
	return p
}

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
	p := DefaultParams()
	cores := splitCores([]byte(text), p)
	if len(cores) <= 1 {
		return cores
	}
	out := make([]string, 0, len(cores))
	out = append(out, cores[0])
	for i := 1; i < len(cores); i++ {
		if ov := overlapTail(cores[i-1], p); ov != "" {
			out = append(out, ov+"\n"+cores[i])
		} else {
			out = append(out, cores[i])
		}
	}
	return out
}

// splitCores produces the non-overlapping content-defined pieces (trimmed,
// non-empty) that the overlap is then layered onto.
func splitCores(b []byte, p Params) []string {
	cores, _ := splitCoresWithOffsets(b, p)
	return cores
}

// splitCoresWithOffsets is splitCores that also returns each core's byte offset in
// b (the untrimmed start), for passage-level provenance (#243).
func splitCoresWithOffsets(b []byte, p Params) (cores []string, offsets []int) {
	regions := atomicRegions(b) // structure that must not be split mid-block (#242)
	for start := 0; start < len(b); {
		end := nextCut(b, start, regions, p)
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
// overlap), so the content hash / reconcile behavior is unchanged. Uses the default
// tuning; SplitWithProvenanceParams takes a per-source override (#401).
func SplitWithProvenance(text string) []Piece {
	return SplitWithProvenanceParams(text, DefaultParams())
}

// SplitWithProvenanceParams is SplitWithProvenance with an explicit size tuning
// (#401). Invalid params fall back to the defaults (see Params.withDefaults), so
// the size invariant always holds.
func SplitWithProvenanceParams(text string, p Params) []Piece {
	p = p.withDefaults()
	b := []byte(text)
	cores, offsets := splitCoresWithOffsets(b, p)
	if len(cores) == 0 {
		return nil
	}
	regions := atomicRegions(b)
	pieces := make([]Piece, 0, len(cores))
	for i, core := range cores {
		txt := core
		if i > 0 {
			// When a chunk begins inside a table body (a big table split across
			// chunks), carry the table's header + separator rows as the overlap so
			// each fragment is self-describing for embedding/retrieval (#369) —
			// instead of the generic previous-tail overlap. Capped to OverlapMax
			// inside tableHeaderOverlap, so the size invariant is unchanged.
			if hdr := tableHeaderOverlap(b, offsets[i], regions, p); hdr != "" {
				txt = hdr + "\n" + core
			} else if ov := overlapTail(cores[i-1], p); ov != "" {
				txt = ov + "\n" + core
			}
		}
		pieces = append(pieces, Piece{Text: txt, Offset: offsets[i], Heading: precedingHeading(b, offsets[i])})
	}
	return pieces
}

// tableHeaderOverlap returns a Markdown table's header + separator rows when
// offset falls inside that table's *body* (i.e. a chunk boundary split the table
// and this chunk starts past the header) — so the continuation fragment repeats the
// header and reads as a table on its own (#369). Returns "" when offset isn't in a
// table body, or when the header wouldn't fit the overlap budget (falls back to the
// generic overlap, preserving the size bound).
func tableHeaderOverlap(b []byte, offset int, regions []region, p Params) string {
	for _, r := range regions {
		if offset <= r.lo || offset >= r.hi {
			continue
		}
		if !isTableRow(b[r.lo:min(r.lo+lineLen(b, r.lo), r.hi)]) {
			continue // a fenced-code region, not a table
		}
		// The header is the first two rows (header + separator); find where the
		// separator row ends. If this chunk already starts at/before the header,
		// there is nothing to repeat.
		sepEnd := secondRowEnd(b, r.lo, r.hi)
		if sepEnd == 0 || offset < sepEnd {
			return ""
		}
		hdr := strings.TrimSpace(string(b[r.lo:sepEnd]))
		if len(hdr) == 0 || len(hdr) > p.OverlapMax {
			return ""
		}
		return hdr
	}
	return ""
}

// lineLen returns the length of the line beginning at start (through its newline).
func lineLen(b []byte, start int) int {
	for i := start; i < len(b); i++ {
		if b[i] == '\n' {
			return i - start + 1
		}
	}
	return len(b) - start
}

// secondRowEnd returns the offset just past the second line (the table separator
// row) within [lo, hi), or 0 if there is no second line.
func secondRowEnd(b []byte, lo, hi int) int {
	firstEnd := lo + lineLen(b, lo)
	if firstEnd >= hi {
		return 0
	}
	return min(firstEnd+lineLen(b, firstEnd), hi)
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
func overlapTail(prev string, p Params) string {
	b := []byte(prev)
	from := len(b) - p.OverlapMax
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
func nextCut(b []byte, start int, regions []region, p Params) int {
	n := len(b)
	if n-start <= p.MinSize {
		return n
	}
	limit := start + p.MaxSize
	if limit > n {
		limit = n
	}
	target := start + p.TargetLen

	var hash uint64
	i := start
	for ; i < limit; i++ {
		hash = (hash << 1) + gear[b[i]]
		if i-start < p.MinSize {
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
	return avoidSplit(b, start, snap(b, start+p.MinSize, i), regions, p)
}

// region is a byte range [lo,hi) covering an atomic structure — a fenced code
// block or a Markdown table — that should not be split mid-block (#242). lo and
// hi are line-aligned (the start of the opening line and the end of the closing
// line), so cutting at either is clean.
type region struct{ lo, hi int }

// avoidSplit moves a proposed cut that lands *inside* an atomic region to a clean
// boundary, so code fences and tables aren't halved. Preference: keep the whole
// block in this chunk when it still fits within maxSize; otherwise cut just
// before the block so it starts the next chunk (where it gets its own shot at
// staying whole). A block that already spans from the chunk's start and exceeds
// maxSize *must* be split — but at a structured boundary (a blank line or a
// top-level line start / table row) rather than an arbitrary rolling-hash cut, so
// a big code file or table breaks into coherent pieces (#350).
func avoidSplit(b []byte, start, cut int, regions []region, p Params) int {
	for _, r := range regions {
		if r.lo >= cut {
			break // regions are sorted; none can contain cut past here
		}
		if cut > r.lo && cut < r.hi { // cut strictly inside this region
			if r.hi-start <= p.MaxSize {
				return r.hi // keep the block whole
			}
			if r.lo > start {
				return r.lo // cut before the block; it begins the next chunk
			}
			return structuredCut(b, start+p.MinSize, cut) // oversized from chunk start
		}
	}
	return cut
}

// structuredCut picks the best split point in (lo, cut] for an oversized code/
// table block (#350): the position just after the last blank line (a natural
// break between functions/paragraphs), else the last top-level line start (a
// newline followed by a non-indented, non-empty line — a symbol boundary, and for
// a pipe table every row start), else cut unchanged. Result is always in [lo, cut]
// so the maxSize bound holds and no line is split mid-way.
func structuredCut(b []byte, lo, cut int) int {
	if cut > len(b) {
		cut = len(b)
	}
	blank, topLevel := -1, -1
	for i := cut - 1; i > lo; i-- {
		if b[i] != '\n' {
			continue
		}
		next := i + 1
		if next >= cut { // the newline is the last byte before the cut — no content after
			continue
		}
		switch {
		case b[next] == '\n':
			// b[i] then an empty line → a blank-line break; split after the empty line.
			if blank < 0 && next+1 <= cut {
				blank = next + 1
			}
		case b[next] != ' ' && b[next] != '\t':
			// a new line starting at column 0 — a likely top-level symbol / table row.
			if topLevel < 0 {
				topLevel = next
			}
		}
	}
	if blank > lo && blank <= cut {
		return blank
	}
	if topLevel > lo && topLevel <= cut {
		return topLevel
	}
	return cut
}

// atomicRegions finds the fenced code blocks and Markdown tables in b, as
// line-aligned [lo,hi) ranges sorted by lo and non-overlapping. A table needs at
// least two consecutive pipe rows (header + separator) to count, so a lone
// pipe-bearing prose line isn't treated as structure.
func atomicRegions(b []byte) []region {
	lines := lineSpans(b)
	var regs []region
	for i := 0; i < len(lines); {
		ls, le := lines[i][0], lines[i][1]
		if fc, ok := fenceChar(b[ls:le]); ok {
			j, hi := i+1, len(b)
			for ; j < len(lines); j++ {
				if isClosingFence(b[lines[j][0]:lines[j][1]], fc) {
					hi = lines[j][1]
					j++ // consume the closing line
					break
				}
			}
			regs = append(regs, region{ls, hi})
			i = j
			continue
		}
		if isTableRow(b[ls:le]) {
			j := i + 1
			for j < len(lines) && isTableRow(b[lines[j][0]:lines[j][1]]) {
				j++
			}
			if j-i >= 2 {
				regs = append(regs, region{ls, lines[j-1][1]})
			}
			i = j
			continue
		}
		i++
	}
	return regs
}

// lineSpans returns each line's [start, end) where end is just past its trailing
// newline (or len(b) for the last line).
func lineSpans(b []byte) [][2]int {
	var spans [][2]int
	start := 0
	for i := 0; i < len(b); i++ {
		if b[i] == '\n' {
			spans = append(spans, [2]int{start, i + 1})
			start = i + 1
		}
	}
	if start < len(b) {
		spans = append(spans, [2]int{start, len(b)})
	}
	return spans
}

// fenceChar reports whether line opens a code fence (>=3 leading '`' or '~',
// after optional indentation) and which char it uses. An info string (```go) is
// allowed after the run.
func fenceChar(line []byte) (byte, bool) {
	s := strings.TrimLeft(string(line), " ")
	for _, fc := range []byte{'`', '~'} {
		n := 0
		for n < len(s) && s[n] == fc {
			n++
		}
		if n >= 3 {
			return fc, true
		}
	}
	return 0, false
}

// isClosingFence reports whether line is a closing fence for char fc: only fc
// repeated >=3 times, with no info string (per CommonMark).
func isClosingFence(line []byte, fc byte) bool {
	s := strings.TrimSpace(string(line))
	if len(s) < 3 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] != fc {
			return false
		}
	}
	return true
}

// isTableRow reports whether line looks like a Markdown table row: after
// optional indentation it starts with a pipe.
func isTableRow(line []byte) bool {
	s := strings.TrimLeft(string(line), " \t")
	return len(s) > 0 && s[0] == '|'
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
