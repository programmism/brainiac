package chunk

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func longText() string {
	var sb strings.Builder
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&sb, "Sentence number %d: the quick brown fox jumps over the lazy dog near the river bank. ", i)
		if i%5 == 4 {
			sb.WriteString("\n\n")
		}
	}
	return sb.String()
}

func hashes(chunks []string) map[string]bool {
	m := make(map[string]bool, len(chunks))
	for _, c := range chunks {
		h := sha256.Sum256([]byte(c))
		m[fmt.Sprintf("%x", h)] = true
	}
	return m
}

func TestSmallTextIsOneChunk(t *testing.T) {
	got := Split("  hello world  ")
	if len(got) != 1 || got[0] != "hello world" {
		t.Fatalf("got %q", got)
	}
	if Split("   ") != nil {
		t.Fatalf("blank text should yield no chunks")
	}
}

func TestSizeBoundsAndNoMidWord(t *testing.T) {
	chunks := Split(longText())
	if len(chunks) < 5 {
		t.Fatalf("expected several chunks, got %d", len(chunks))
	}
	// A chunk is a core (<= maxSize) plus at most overlapMax of overlap and a
	// one-byte separator (#214).
	bound := maxSize + overlapMax + 1
	for i, c := range chunks {
		if n := len(c); n > bound {
			t.Errorf("chunk %d exceeds core+overlap bound: %d > %d", i, n, bound)
		}
		if !utf8.ValidString(c) {
			t.Errorf("chunk %d is not valid UTF-8", i)
		}
	}
}

// Every chunk after the first prefixes the previous core's sentence-aligned tail,
// so a fact on a boundary survives whole in at least one chunk (#214).
func TestOverlapCarriesPreviousTail(t *testing.T) {
	text := longText()
	cores := splitCores([]byte(text))
	chunks := Split(text)
	if len(cores) != len(chunks) {
		t.Fatalf("core/chunk count mismatch: %d vs %d", len(cores), len(chunks))
	}
	overlaps := 0
	for i := 1; i < len(chunks); i++ {
		ov := overlapTail(cores[i-1])
		var want string
		if ov != "" {
			want = ov + "\n" + cores[i]
			overlaps++
			// The overlap is a genuine substring of the previous core's tail.
			if !strings.Contains(cores[i-1], ov) {
				t.Fatalf("overlap %q is not from the previous core", ov)
			}
		} else {
			want = cores[i]
		}
		if chunks[i] != want {
			t.Fatalf("chunk %d = %q\nwant %q", i, chunks[i], want)
		}
	}
	if overlaps == 0 {
		t.Fatal("expected at least one non-empty overlap across chunks")
	}
}

// overlapTail begins at a clean boundary — never mid-word/mid-rune — and stays
// within the byte budget.
func TestOverlapTailBoundary(t *testing.T) {
	prev := "First sentence here. Second sentence follows. Third and final sentence of the paragraph."
	got := overlapTail(prev)
	if got == "" {
		t.Fatal("expected a non-empty tail")
	}
	if len(got) > overlapMax {
		t.Fatalf("overlap %d bytes exceeds budget %d", len(got), overlapMax)
	}
	// It must start at a word start (no leading partial word) and be a suffix-region.
	if strings.HasPrefix(got, " ") || !strings.Contains(prev, got) {
		t.Fatalf("overlap not cleanly bounded: %q", got)
	}
	if r := []rune(got); len(r) == 0 || !utf8.ValidString(got) {
		t.Fatalf("overlap not valid UTF-8: %q", got)
	}
}

// Split is deterministic — same input, same chunks (relied on by hash reconcile).
func TestSplitDeterministic(t *testing.T) {
	text := longText()
	a, b := Split(text), Split(text)
	if len(a) != len(b) {
		t.Fatalf("nondeterministic length: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("chunk %d differs across runs", i)
		}
	}
}

// TestSelfHealingOnEarlyEdit is the whole point: inserting a word near the top
// must change only a few chunks — the rest re-synchronize and keep their hashes.
func TestSelfHealingOnEarlyEdit(t *testing.T) {
	text := longText()
	c1 := Split(text)
	// Insert a word ~30 bytes in (well inside the first chunk).
	edited := text[:30] + "INSERTEDWORD " + text[30:]
	c2 := Split(edited)

	h1 := hashes(c1)
	changed := 0
	for _, c := range c2 {
		sum := fmt.Sprintf("%x", sha256.Sum256([]byte(c)))
		if !h1[sum] {
			changed++
		}
	}
	if changed > 3 {
		t.Fatalf("early edit changed %d/%d chunks — boundaries did not self-heal", changed, len(c2))
	}
	if changed == 0 {
		t.Fatalf("expected the edited chunk to change, got 0")
	}
}

func TestUnicodeBoundaries(t *testing.T) {
	// A long run of multibyte runes with no ASCII spaces in places.
	text := strings.Repeat("привет мир “ёжик” 日本語テキスト ", 200)
	for _, c := range Split(text) {
		if !utf8.ValidString(c) {
			t.Fatalf("chunk is not valid UTF-8: %q", c)
		}
	}
}

func TestSplitWithProvenance(t *testing.T) {
	// Provenance Text matches Split 1:1 (same overlap).
	text := longText()
	pieces := SplitWithProvenance(text)
	split := Split(text)
	if len(pieces) != len(split) {
		t.Fatalf("provenance/split count mismatch: %d vs %d", len(pieces), len(split))
	}
	for i := range pieces {
		if pieces[i].Text != split[i] {
			t.Fatalf("piece %d text differs from Split", i)
		}
		if i > 0 && pieces[i].Offset <= pieces[i-1].Offset {
			t.Errorf("offsets not increasing at %d: %d <= %d", i, pieces[i].Offset, pieces[i-1].Offset)
		}
	}

	// Headings: a chunk after a heading carries the nearest preceding one.
	var sb strings.Builder
	sb.WriteString("# Architecture\n\n")
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&sb, "OrderService writes orders to Kafka for durability during peak load, decision %d. ", i)
	}
	sb.WriteString("\n\n## Retention policy\n\n")
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&sb, "Events are retained for ninety days then archived to cold storage, note %d. ", i)
	}
	ps := SplitWithProvenance(sb.String())
	seen := map[string]bool{}
	for _, p := range ps {
		if p.Heading != "" {
			seen[p.Heading] = true
		}
	}
	if !seen["Architecture"] && !seen["Retention policy"] {
		t.Errorf("expected a heading anchor among chunks, got %v", seen)
	}
}

// prose returns n paragraphs of filler so a fenced block placed between them
// straddles what would otherwise be a natural chunk boundary.
func prose(n int) string {
	var sb strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&sb, "Paragraph %d: the quick brown fox jumps over the lazy dog by the river. ", i)
		if i%4 == 3 {
			sb.WriteString("\n\n")
		}
	}
	return sb.String()
}

func TestAtomicRegionsDetection(t *testing.T) {
	text := "before\n```go\nfunc f() {}\nx := 1\n```\nafter\n"
	regs := atomicRegions([]byte(text))
	if len(regs) != 1 {
		t.Fatalf("want 1 region, got %d: %+v", len(regs), regs)
	}
	got := text[regs[0].lo:regs[0].hi]
	want := "```go\nfunc f() {}\nx := 1\n```\n"
	if got != want {
		t.Fatalf("region = %q, want %q", got, want)
	}
	// A lone pipe line is not a table; two consecutive rows are.
	if r := atomicRegions([]byte("a | b is prose\nmore\n")); len(r) != 0 {
		t.Fatalf("lone pipe line should not be atomic: %+v", r)
	}
	tbl := "| a | b |\n|---|---|\n| 1 | 2 |\n"
	if r := atomicRegions([]byte(tbl)); len(r) != 1 || tbl[r[0].lo:r[0].hi] != tbl {
		t.Fatalf("table not detected as one region: %+v", r)
	}
	// An unterminated fence runs to EOF so a half-open block still isn't split.
	if r := atomicRegions([]byte("x\n```\nunclosed\n")); len(r) != 1 || r[0].hi != len("x\n```\nunclosed\n") {
		t.Fatalf("unterminated fence should extend to EOF: %+v", r)
	}
}

// A fenced code block that fits within maxSize must land whole inside a single
// chunk even when surrounded by enough prose to force several chunks (#242).
func TestFencedBlockNotSplit(t *testing.T) {
	fence := "```go\n"
	for i := 0; i < 12; i++ {
		fence += fmt.Sprintf("line%02d := compute(%d) // keep this whole\n", i, i)
	}
	fence += "```"
	text := prose(30) + "\n\n" + fence + "\n\n" + prose(30)

	chunks := Split(text)
	if len(chunks) < 3 {
		t.Fatalf("expected several chunks, got %d", len(chunks))
	}
	whole := false
	for _, c := range chunks {
		if strings.Contains(c, fence) {
			whole = true
		}
		// No chunk should contain an opening fence without its close (a mid-split).
		if n := strings.Count(c, "```"); n%2 != 0 {
			t.Errorf("chunk splits a code fence (odd ``` count): %q", c)
		}
	}
	if !whole {
		t.Fatalf("fenced block was not kept whole in any chunk")
	}
}

// A Markdown table that fits within maxSize is likewise kept intact.
func TestTableNotSplit(t *testing.T) {
	var tb strings.Builder
	tb.WriteString("| id | name | note |\n|----|------|------|\n")
	for i := 0; i < 12; i++ {
		fmt.Fprintf(&tb, "| %d | row-%02d | keep this row intact |\n", i, i)
	}
	table := strings.TrimRight(tb.String(), "\n")
	text := prose(30) + "\n\n" + table + "\n\n" + prose(30)

	for _, c := range Split(text) {
		if strings.Contains(c, "| id | name | note |") && !strings.Contains(c, "row-11") {
			t.Fatalf("table header separated from its last row — split mid-table:\n%q", c)
		}
	}
}

func TestFenceCharAndClosing(t *testing.T) {
	if _, ok := fenceChar([]byte("```go")); !ok {
		t.Error("```go should open a fence")
	}
	if _, ok := fenceChar([]byte("  ~~~")); !ok {
		t.Error("indented ~~~ should open a fence")
	}
	if _, ok := fenceChar([]byte("``x")); ok {
		t.Error("two backticks is not a fence")
	}
	if !isClosingFence([]byte("```\n"), '`') {
		t.Error("``` should close a backtick fence")
	}
	if isClosingFence([]byte("```go\n"), '`') {
		t.Error("a fence with an info string does not close")
	}
	if isClosingFence([]byte("~~~\n"), '`') {
		t.Error("~~~ must not close a backtick fence")
	}
}

func TestStructureAwareStillDeterministic(t *testing.T) {
	text := prose(20) + "\n\n```\ncode block here\nsecond line\n```\n\n" + prose(20)
	a, b := Split(text), Split(text)
	if len(a) != len(b) {
		t.Fatalf("non-deterministic length: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("chunk %d differs across runs", i)
		}
	}
}

func TestATXHeading(t *testing.T) {
	cases := map[string]string{
		"# Title":          "Title",
		"###  Deep  ":      "Deep",
		"## Section ##":    "Section",
		"not a heading":    "",
		"#nospace":         "",
		"####### too many": "",
		"  ## indented":    "indented",
	}
	for in, want := range cases {
		got, ok := atxHeading([]byte(in))
		if (want == "" && ok) || (want != "" && got != want) {
			t.Errorf("atxHeading(%q) = (%q,%v), want %q", in, got, ok, want)
		}
	}
}
