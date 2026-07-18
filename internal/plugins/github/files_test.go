package github

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/programmism/brainiac/internal/plugins"
)

func TestFetchRepoFilesOptIn(t *testing.T) {
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/issues"):
			fmt.Fprint(w, `[]`) // no issues — isolate the file path
		case strings.HasSuffix(r.URL.Path, "/git/trees/main"):
			fmt.Fprint(w, `{"tree":[
				{"path":"README.md","type":"blob","sha":"s1"},
				{"path":"docs/guide.md","type":"blob","sha":"s2"},
				{"path":"docs","type":"tree","sha":"t1"},
				{"path":"src/main.go","type":"blob","sha":"s3"},
				{"path":"notes.txt","type":"blob","sha":"s4"}
			],"truncated":false}`)
		case strings.HasSuffix(r.URL.Path, "/git/blobs/s1"):
			fmt.Fprintf(w, `{"encoding":"base64","content":%q}`, b64("# Readme\nthe project overview"))
		case strings.HasSuffix(r.URL.Path, "/git/blobs/s2"):
			fmt.Fprintf(w, `{"encoding":"base64","content":%q}`, b64("architecture guide details"))
		case strings.HasSuffix(r.URL.Path, "/git/blobs/s4"):
			fmt.Fprintf(w, `{"encoding":"base64","content":%q}`, b64("misc notes"))
		case strings.HasSuffix(r.URL.Path, "/repos/octo/repo"):
			fmt.Fprint(w, `{"default_branch":"main"}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	// Opt-in: README* and docs/** → README.md + docs/guide.md; src/main.go excluded
	// (.go unsupported by doctext), notes.txt excluded (matches no glob).
	c := New("tok", []string{"octo/repo"},
		WithFiles([]string{"README*", "docs/**"}), WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	byURI := map[string]plugins.RawDoc{}
	for d, err := range c.Fetch(context.Background()) {
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		byURI[d.SourceURI] = d
	}
	if len(byURI) != 2 {
		t.Fatalf("got %d file docs, want 2 (README.md + docs/guide.md): %v", len(byURI), keysOf(byURI))
	}
	readme := byURI["github://octo/repo/README.md"]
	if !strings.Contains(readme.Text, "the project overview") || readme.Metadata["kind"] != "file" {
		t.Fatalf("README doc = %+v", readme)
	}
	if _, ok := byURI["github://octo/repo/docs/guide.md"]; !ok {
		t.Fatalf("docs/guide.md not ingested: %v", keysOf(byURI))
	}
	if _, ok := byURI["github://octo/repo/src/main.go"]; ok {
		t.Fatal("src/main.go should be excluded (unsupported by doctext)")
	}

	// Default (no WithFiles): file endpoints are never hit — only issues/PRs.
	c2 := New("tok", []string{"octo/repo"}, WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	n := 0
	for _, err := range c2.Fetch(context.Background()) {
		if err != nil {
			t.Fatalf("fetch default: %v", err)
		}
		n++
	}
	if n != 0 {
		t.Fatalf("default connector yielded %d docs, want 0 (no files, no issues)", n)
	}
}

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		glob, path string
		want       bool
	}{
		{"README*", "README.md", true},
		{"README*", "docs/README.md", true}, // basename match
		{"*.md", "docs/guide.md", true},
		{"docs/**", "docs/a/b.md", true},
		{"docs/**", "src/x.md", false},
		{"docs/*.md", "docs/guide.md", true},
		{"docs/*.md", "docs/sub/guide.md", false},
		{"ADR*", "src/main.go", false},
	}
	for _, tc := range cases {
		if got := matchGlob(tc.glob, tc.path); got != tc.want {
			t.Errorf("matchGlob(%q,%q)=%v want %v", tc.glob, tc.path, got, tc.want)
		}
	}
}

func keysOf(m map[string]plugins.RawDoc) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
