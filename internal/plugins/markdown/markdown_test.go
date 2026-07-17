package markdown

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/programmism/brainiac/internal/plugins"
)

func TestFetchWalksSupportedFiles(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.md", "# Alpha\n\nfirst")
	write(t, root, filepath.Join("sub", "b.markdown"), "# Beta\n\nsecond")
	write(t, root, "c.txt", "plain text now ingested") // .txt is supported (#234)
	write(t, root, "page.html", "<h1>Gamma</h1><p>third</p>")
	write(t, root, "ignore.bin", "binary blob") // unsupported → skipped

	c := New(root)
	byURI := map[string]plugins.RawDoc{}
	for d, err := range c.Fetch(context.Background()) {
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		byURI[d.SourceURI] = d
	}

	if len(byURI) != 4 {
		t.Fatalf("got %d docs, want 4 (.bin skipped): %v", len(byURI), keys(byURI))
	}
	if got := byURI["markdown://a.md"].Text; got != "# Alpha\n\nfirst" {
		t.Errorf("md passthrough changed: %q", got)
	}
	if got := byURI["markdown://c.txt"].Text; got != "plain text now ingested" {
		t.Errorf(".txt not ingested as text: %q", got)
	}
	if got := byURI["markdown://page.html"].Text; !contains(got, "Gamma") || !contains(got, "third") {
		t.Errorf("html not converted to text: %q", got)
	}
	if byURI["markdown://sub/b.markdown"].SourceLocator["path"] != "sub/b.markdown" {
		t.Errorf("locator wrong: %v", byURI["markdown://sub/b.markdown"].SourceLocator)
	}
}

func keys(m map[string]plugins.RawDoc) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func contains(s, sub string) bool { return len(s) >= len(sub) && strings.Contains(s, sub) }

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

func TestFetchMissingDirIsNoOp(t *testing.T) {
	c := New(filepath.Join(t.TempDir(), "does-not-exist"))
	n := 0
	for _, err := range c.Fetch(context.Background()) {
		if err != nil {
			t.Fatalf("missing dir should not error: %v", err)
		}
		n++
	}
	if n != 0 {
		t.Fatalf("expected no docs from a missing dir, got %d", n)
	}
}
