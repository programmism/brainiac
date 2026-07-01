package markdown

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/programmism/brainiac/internal/plugins"
)

func TestFetchWalksMarkdownFiles(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.md", "# Alpha\n\nfirst")
	write(t, root, filepath.Join("sub", "b.markdown"), "# Beta\n\nsecond")
	write(t, root, "c.txt", "ignored non-markdown")

	c := New(root)
	var docs []plugins.RawDoc
	for d, err := range c.Fetch(context.Background()) {
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		docs = append(docs, d)
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].SourceURI < docs[j].SourceURI })

	if len(docs) != 2 {
		t.Fatalf("got %d docs, want 2 (.txt ignored): %+v", len(docs), docs)
	}
	if docs[0].SourceURI != "markdown://a.md" || docs[0].Text != "# Alpha\n\nfirst" {
		t.Errorf("doc0 = %+v", docs[0])
	}
	if docs[1].SourceURI != "markdown://sub/b.markdown" {
		t.Errorf("doc1 uri = %q", docs[1].SourceURI)
	}
	if docs[1].SourceLocator["path"] != "sub/b.markdown" {
		t.Errorf("doc1 locator = %v", docs[1].SourceLocator)
	}
}

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
