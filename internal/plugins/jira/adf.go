package jira

import "strings"

// adfToText renders an Atlassian Document Format tree (decoded JSON) to plain
// text: text nodes are concatenated, hard breaks become newlines, and each
// block-level node (paragraph, heading, list item, quote, code, rule) ends with a
// blank line. It is intentionally lenient — an unknown node just has its content
// walked — so a new ADF node type degrades to "keep its text" rather than losing
// it. Runs of blank lines are collapsed so the result feeds cleanly into chunking.
func adfToText(v any) string {
	var b strings.Builder
	walkADF(&b, v)
	return collapseBlankLines(strings.TrimSpace(b.String()))
}

func walkADF(b *strings.Builder, v any) {
	switch n := v.(type) {
	case []any:
		for _, ch := range n {
			walkADF(b, ch)
		}
	case map[string]any:
		typ, _ := n["type"].(string)
		switch typ {
		case "text":
			if s, ok := n["text"].(string); ok {
				b.WriteString(s)
			}
		case "hardBreak":
			b.WriteByte('\n')
		}
		if content, ok := n["content"].([]any); ok {
			for _, ch := range content {
				walkADF(b, ch)
			}
		}
		switch typ {
		case "paragraph", "heading", "blockquote", "codeBlock", "rule", "mediaSingle":
			b.WriteString("\n\n")
		case "listItem":
			b.WriteByte('\n')
		}
	}
}

// collapseBlankLines trims trailing spaces and reduces any run of 3+ newlines to
// a paragraph break (two).
func collapseBlankLines(s string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	joined := strings.Join(lines, "\n")
	for strings.Contains(joined, "\n\n\n") {
		joined = strings.ReplaceAll(joined, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(joined)
}
