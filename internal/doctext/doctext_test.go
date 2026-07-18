package doctext

import (
	"archive/zip"
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestSupported(t *testing.T) {
	for _, ok := range []string{"a.txt", "A.MD", "notes.markdown", "page.HTML", "p.htm", "doc.docx", "report.PDF"} {
		if !Supported(ok) {
			t.Errorf("Supported(%q) = false, want true", ok)
		}
	}
	for _, no := range []string{"b.png", "c", "d.xlsx"} {
		if Supported(no) {
			t.Errorf("Supported(%q) = true, want false", no)
		}
	}
}

func TestPlainAndMarkdownPassthrough(t *testing.T) {
	in := "# Title\n\nBody with **bold**.\n"
	for _, name := range []string{"a.md", "a.txt", "a.markdown"} {
		got, err := ToText(name, []byte(in))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != in {
			t.Errorf("%s: passthrough changed content: %q", name, got)
		}
	}
}

func TestHTMLToText(t *testing.T) {
	html := `<html><head><title>t</title><style>.x{color:red}</style></head>
<body>
<h1>Heading</h1>
<p>First&nbsp;paragraph with <b>bold</b> &amp; an entity.</p>
<script>var x = 1 < 2;</script>
<ul><li>one</li><li>two</li></ul>
</body></html>`
	got, err := ToText("page.html", []byte(html))
	if err != nil {
		t.Fatalf("ToText: %v", err)
	}
	// Markup and script/style bodies are gone; text and entities survive.
	for _, want := range []string{"Heading", "First paragraph with bold & an entity.", "one", "two"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	for _, bad := range []string{"color:red", "var x", "<h1>", "<p>", "&amp;", "&nbsp;"} {
		if strings.Contains(got, bad) {
			t.Errorf("leaked markup/entity %q in:\n%s", bad, got)
		}
	}
}

func TestHTMLUnterminatedTagIsSafe(t *testing.T) {
	// A stray '<' with no '>' must not panic or loop forever.
	if _, err := ToText("x.htm", []byte("hello <world")); err != nil {
		t.Fatalf("unterminated tag: %v", err)
	}
}

// buildDocx assembles a minimal but valid .docx (a zip with word/document.xml).
func buildDocx(t *testing.T, bodyXML string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	doc := `<?xml version="1.0"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
<w:body>` + bodyXML + `</w:body></w:document>`
	f, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(doc)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestDocxToText(t *testing.T) {
	body := `<w:p><w:r><w:t>Hello</w:t></w:r><w:r><w:t xml:space="preserve"> world</w:t></w:r></w:p>` +
		`<w:p><w:r><w:t>Second</w:t><w:tab/><w:t>line</w:t></w:r></w:p>`
	data := buildDocx(t, body)
	got, err := ToText("doc.docx", data)
	if err != nil {
		t.Fatalf("docx: %v", err)
	}
	if !strings.Contains(got, "Hello world") {
		t.Errorf("runs not joined: %q", got)
	}
	if !strings.Contains(got, "Second\tline") {
		t.Errorf("tab separator lost: %q", got)
	}
	// Two paragraphs → a line break between them.
	if !strings.Contains(got, "Hello world\nSecond") {
		t.Errorf("paragraph break lost: %q", got)
	}
}

func TestDocxInvalidZip(t *testing.T) {
	if _, err := ToText("bad.docx", []byte("not a zip")); err == nil {
		t.Fatal("expected error for non-zip .docx")
	}
}

func TestUnsupported(t *testing.T) {
	_, err := ToText("sheet.xlsx", []byte("PK\x03\x04"))
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("xlsx should be ErrUnsupported, got %v", err)
	}
}
