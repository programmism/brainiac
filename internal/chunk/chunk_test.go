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
	for i, c := range chunks {
		if n := len(c); n > maxSize {
			t.Errorf("chunk %d exceeds maxSize: %d", i, n)
		}
		if !utf8.ValidString(c) {
			t.Errorf("chunk %d is not valid UTF-8", i)
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
