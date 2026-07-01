// Package chunk splits text with content-defined chunking (a Gear/FastCDC-style
// rolling hash) so chunk boundaries are determined by local content, not
// absolute position. An edit therefore only changes the chunk(s) it touches:
// downstream boundaries re-synchronize on the same content, keeping their text
// (and content hash) identical — so re-ingest re-embeds only the edited region,
// not the whole tail (SYSTEM.md §8, issue #99). Boundaries are snapped to the
// nearest line/word break so chunks never split mid-word or mid-rune.
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

// Split breaks text into content-defined chunks (trimmed, non-empty).
func Split(text string) []string {
	b := []byte(text)
	var chunks []string
	for start := 0; start < len(b); {
		end := nextCut(b, start)
		if piece := strings.TrimSpace(string(b[start:end])); piece != "" {
			chunks = append(chunks, piece)
		}
		start = end
	}
	return chunks
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
