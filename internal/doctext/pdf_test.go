package doctext

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// makePDF builds a minimal single-page PDF whose content stream draws each line
// with the standard Helvetica font, computing xref offsets as it writes so the
// file is valid for rsc.io/pdf. Lines are drawn top-to-bottom.
func makePDF(lines []string) []byte {
	// Draw each word as its own positioned Tj. The standard-14 Helvetica has no
	// /Widths array, so rsc.io/pdf can't advance X within a Tj; placing each word
	// at an explicit, well-separated X makes the fragment gaps real — exactly what
	// a real PDF's width metrics would produce — so reading-order reconstruction
	// (and the space between words) is exercised honestly.
	var content strings.Builder
	content.WriteString("BT /F0 12 Tf\n")
	esc := strings.NewReplacer("\\", "\\\\", "(", "\\(", ")", "\\)")
	y := 720
	for _, ln := range lines {
		x := 72
		for _, word := range strings.Fields(ln) {
			fmt.Fprintf(&content, "1 0 0 1 %d %d Tm (%s) Tj\n", x, y, esc.Replace(word))
			x += len(word)*7 + 10 // advance past the word plus a space gap
		}
		y -= 16
	}
	content.WriteString("ET")
	stream := content.String()

	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F0 4 0 R >> >> /Contents 5 0 R >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream),
	}

	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs)+1)
	for i, body := range objs {
		offsets[i+1] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i+1, body)
	}
	xrefStart := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n", len(objs)+1)
	buf.WriteString("0000000000 65535 f \n")
	for i := 1; i <= len(objs); i++ {
		fmt.Fprintf(&buf, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF", len(objs)+1, xrefStart)
	return buf.Bytes()
}

func TestPDFToText(t *testing.T) {
	pdf := makePDF([]string{"OrderService writes to Postgres", "chosen for one join and one backup"})

	if !Supported("doc.pdf") {
		t.Fatal("Supported should report .pdf")
	}
	got, err := ToText("doc.pdf", pdf)
	if err != nil {
		t.Fatalf("ToText pdf: %v", err)
	}
	for _, want := range []string{"OrderService writes to Postgres", "one join and one backup"} {
		if !strings.Contains(got, want) {
			t.Errorf("extracted text missing %q; got:\n%s", want, got)
		}
	}
}

// makeBlankPDF builds a valid single-page PDF whose content stream draws no text,
// so pdfToText returns "" — a stand-in for a scanned / image-only PDF.
func makeBlankPDF() []byte {
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R >>",
		"<< /Length 0 >>\nstream\n\nendstream",
	}
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs)+1)
	for i, body := range objs {
		offsets[i+1] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i+1, body)
	}
	xrefStart := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n", len(objs)+1)
	buf.WriteString("0000000000 65535 f \n")
	for i := 1; i <= len(objs); i++ {
		fmt.Fprintf(&buf, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF", len(objs)+1, xrefStart)
	return buf.Bytes()
}

// TestPDFOCRHook: an image-only PDF yields "" without OCR, the configured OCR
// fallback supplies text, and a PDF that already has a text layer never invokes
// OCR (#356).
func TestPDFOCRHook(t *testing.T) {
	blank := makeBlankPDF()

	// No text layer, no OCR → empty (skipped).
	if got, err := ToText("scan.pdf", blank); err != nil || got != "" {
		t.Fatalf("image-only PDF without OCR = (%q, %v), want (\"\", nil)", got, err)
	}
	// With an OCR fallback → its output.
	ocr := func([]byte) (string, error) { return "recognized scan text", nil }
	if got, err := ToTextOCR("scan.pdf", blank, ocr); err != nil || got != "recognized scan text" {
		t.Fatalf("image-only PDF with OCR = (%q, %v), want the OCR text", got, err)
	}
	// A PDF that has a text layer is never sent to OCR, even when one is set.
	textPDF := makePDF([]string{"OrderService writes to Postgres"})
	called := false
	got, err := ToTextOCR("doc.pdf", textPDF, func([]byte) (string, error) { called = true; return "SHOULD NOT RUN", nil })
	if err != nil {
		t.Fatalf("text PDF: %v", err)
	}
	if called {
		t.Fatal("OCR was invoked for a PDF that already has a text layer")
	}
	if !strings.Contains(got, "OrderService writes to Postgres") {
		t.Fatalf("text PDF lost its text: %q", got)
	}
	// OCR errors surface (so the connector can count/skip the doc).
	if _, err := ToTextOCR("scan.pdf", blank, func([]byte) (string, error) { return "", fmt.Errorf("ocr boom") }); err == nil {
		t.Fatal("OCR error should surface")
	}
}

func TestPDFMalformedIsErrorNotPanic(t *testing.T) {
	// Garbage that is not a PDF must return an error, never crash the process.
	got, err := ToText("bad.pdf", []byte("%PDF-1.4\nthis is not a real pdf\n%%EOF"))
	if err == nil {
		t.Fatalf("expected an error for malformed PDF, got text %q", got)
	}
}
