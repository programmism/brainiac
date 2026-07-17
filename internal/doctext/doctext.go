// Package doctext is the multi-format extraction layer (#234): it turns a source
// document's bytes into plain text the ingest pipeline can chunk, embed, and
// store. It is deliberately dependency-free (SYSTEM.md §3) — plain text and
// Markdown pass through, HTML is stripped with a small hand-rolled tokenizer, and
// DOCX is unzipped and its runs pulled from the WordprocessingML with the
// standard library. Formats that need a heavier parser (e.g. PDF) return
// ErrUnsupported so the caller can skip and count them rather than ingest binary.
package doctext

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

// ErrUnsupported is returned by ToText for a file extension with no converter.
var ErrUnsupported = fmt.Errorf("doctext: unsupported format")

// Supported reports whether ToText has a converter for name's extension.
func Supported(name string) bool {
	switch ext(name) {
	case ".txt", ".text", ".md", ".markdown", ".html", ".htm", ".docx":
		return true
	default:
		return false
	}
}

// ToText extracts plain text from a document's bytes, dispatching on the file
// extension. Markdown and plain text pass through unchanged (the chunker and
// normalizer handle their structure). Returns ErrUnsupported for formats without
// a converter.
func ToText(name string, data []byte) (string, error) {
	switch ext(name) {
	case ".txt", ".text", ".md", ".markdown":
		return string(data), nil
	case ".html", ".htm":
		return htmlToText(data), nil
	case ".docx":
		return docxToText(data)
	default:
		return "", fmt.Errorf("%w: %q", ErrUnsupported, filepath.Ext(name))
	}
}

func ext(name string) string { return strings.ToLower(filepath.Ext(name)) }

// htmlToText strips markup to readable text: <script>/<style> bodies are dropped
// wholesale, block-level tags become line breaks, remaining tags are removed, and
// common entities are decoded. Not a full HTML parser — good enough to feed
// retrieval, and dependency-free.
func htmlToText(data []byte) string {
	s := string(data)
	var b strings.Builder
	for i := 0; i < len(s); {
		c := s[i]
		if c == '<' {
			// Drop the entire body of script/style/head elements.
			if skip := skipRawElement(s, i); skip > i {
				i = skip
				continue
			}
			end := strings.IndexByte(s[i:], '>')
			if end < 0 {
				break // unterminated tag — stop, the rest is not markup we trust
			}
			tag := s[i+1 : i+end]
			if isBlockTag(tag) {
				b.WriteByte('\n')
			}
			i += end + 1
			continue
		}
		b.WriteByte(c)
		i++
	}
	return strings.TrimSpace(collapseBlankLines(decodeEntities(b.String())))
}

// skipRawElement, when s[i:] opens a <script>/<style>/<head>, returns the index
// just past its closing tag; otherwise it returns i unchanged.
func skipRawElement(s string, i int) int {
	for _, name := range []string{"script", "style", "head"} {
		open := "<" + name
		if len(s) >= i+len(open) && strings.EqualFold(s[i:i+len(open)], open) {
			closeTag := "</" + name
			if end := indexFold(s[i:], closeTag); end >= 0 {
				rest := s[i+end:]
				if gt := strings.IndexByte(rest, '>'); gt >= 0 {
					return i + end + gt + 1
				}
			}
			return len(s) // unterminated — consume the rest
		}
	}
	return i
}

func isBlockTag(tag string) bool {
	tag = strings.TrimSpace(tag)
	tag = strings.TrimPrefix(tag, "/")
	if j := strings.IndexAny(tag, " \t\r\n>"); j >= 0 {
		tag = tag[:j]
	}
	switch strings.ToLower(tag) {
	case "p", "br", "div", "li", "tr", "h1", "h2", "h3", "h4", "h5", "h6",
		"ul", "ol", "table", "section", "article", "header", "footer", "hr", "blockquote", "pre":
		return true
	}
	return false
}

var entityReplacer = strings.NewReplacer(
	"&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", "\"",
	"&#39;", "'", "&apos;", "'", "&nbsp;", " ", "&mdash;", "—", "&ndash;", "–",
)

func decodeEntities(s string) string { return entityReplacer.Replace(s) }

// collapseBlankLines squeezes runs of blank lines down to one, trimming trailing
// spaces per line, so stripped markup doesn't leave a ragged wall of whitespace.
func collapseBlankLines(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := false
	for _, ln := range lines {
		ln = strings.TrimRight(ln, " \t\r")
		if strings.TrimSpace(ln) == "" {
			if blank {
				continue
			}
			blank = true
			out = append(out, "")
			continue
		}
		blank = false
		out = append(out, ln)
	}
	return strings.Join(out, "\n")
}

// indexFold is a case-insensitive strings.Index.
func indexFold(s, substr string) int {
	n := len(substr)
	for i := 0; i+n <= len(s); i++ {
		if strings.EqualFold(s[i:i+n], substr) {
			return i
		}
	}
	return -1
}

// docxToText unzips a .docx and concatenates the text runs (<w:t>) from
// word/document.xml, inserting a newline per paragraph (<w:p>) and tab/break
// separators. Namespace prefixes are ignored (match on local element name) so it
// is robust to the various producers' prefixes.
func docxToText(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("docx: not a valid zip: %w", err)
	}
	var doc *zip.File
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			doc = f
			break
		}
	}
	if doc == nil {
		return "", fmt.Errorf("docx: missing word/document.xml")
	}
	rc, err := doc.Open()
	if err != nil {
		return "", fmt.Errorf("docx: open document.xml: %w", err)
	}
	defer func() { _ = rc.Close() }()
	raw, err := io.ReadAll(rc)
	if err != nil {
		return "", fmt.Errorf("docx: read document.xml: %w", err)
	}

	var b strings.Builder
	dec := xml.NewDecoder(bytes.NewReader(raw))
	inText := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("docx: parse document.xml: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "t":
				inText = true
			case "tab":
				b.WriteByte('\t')
			case "br":
				b.WriteByte('\n')
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "p":
				b.WriteByte('\n')
			}
		case xml.CharData:
			if inText {
				b.Write(t)
			}
		}
	}
	return strings.TrimSpace(collapseBlankLines(b.String())), nil
}
