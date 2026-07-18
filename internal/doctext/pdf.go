package doctext

import (
	"bytes"
	"fmt"
	"math"
	"sort"
	"strings"

	"rsc.io/pdf"
)

// pdfToText extracts the embedded text of a PDF, reconstructing reading order
// from each fragment's position (#321). It is the one place doctext takes an
// external dependency: robust PDF parsing is well beyond a hand-rolled tokenizer,
// and the issue scoped a library in. Image-only / scanned PDFs carry no text
// layer, so they yield "" (the caller skips an empty doc); OCR is a follow-up.
//
// rsc.io/pdf panics on some malformed inputs rather than returning an error, so
// the whole parse is wrapped in a recover — a bad PDF becomes a skip-and-count
// error, never a crash of the ingest process.
func pdfToText(data []byte) (text string, err error) {
	defer func() {
		if r := recover(); r != nil {
			text, err = "", fmt.Errorf("pdf: cannot parse (%v)", r)
		}
	}()
	r, rerr := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if rerr != nil {
		return "", fmt.Errorf("pdf: %w", rerr)
	}
	var b strings.Builder
	for i := 1; i <= r.NumPage(); i++ {
		p := r.Page(i)
		if p.V.IsNull() {
			continue
		}
		if pt := pageText(p); pt != "" {
			b.WriteString(pt)
			b.WriteString("\n\n")
		}
	}
	return strings.TrimSpace(collapseBlankLines(b.String())), nil
}

// pageText joins a page's positioned text fragments into reading order: fragments
// are ordered top-to-bottom (Y descending) then left-to-right (X ascending), a
// new line starts when Y drops, and a space separates fragments with an X gap.
func pageText(p pdf.Page) string {
	texts := p.Content().Text
	if len(texts) == 0 {
		return ""
	}
	const yTol = 3.0 // points; fragments within this Y are the same line
	sort.SliceStable(texts, func(i, j int) bool {
		if math.Abs(texts[i].Y-texts[j].Y) > yTol {
			return texts[i].Y > texts[j].Y
		}
		return texts[i].X < texts[j].X
	})
	var b strings.Builder
	var lastY, lastX float64
	first := true
	for _, t := range texts {
		switch {
		case first:
			first = false
		case math.Abs(t.Y-lastY) > yTol:
			b.WriteByte('\n')
		case t.X-lastX > 0.5 && !strings.HasPrefix(t.S, " ") && !strings.HasSuffix(b.String(), " "):
			b.WriteByte(' ')
		}
		b.WriteString(t.S)
		lastY, lastX = t.Y, t.X+t.W
	}
	return b.String()
}
