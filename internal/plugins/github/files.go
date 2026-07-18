package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"

	"github.com/programmism/brainiac/internal/doctext"
	"github.com/programmism/brainiac/internal/plugins"
)

// Repo-file ingestion (#354): opt-in via WithFiles. Lists the repo's default-branch
// tree, filters to blobs matching the configured globs that doctext can convert,
// fetches each blob, and yields a RawDoc. Best-effort per file; a repo-level error
// (default branch / tree) is surfaced then the repo's file pass is skipped.

type repoInfo struct {
	DefaultBranch string `json:"default_branch"`
}

type treeResponse struct {
	Tree []struct {
		Path string `json:"path"`
		Type string `json:"type"` // "blob" | "tree"
		SHA  string `json:"sha"`
	} `json:"tree"`
	Truncated bool `json:"truncated"`
}

type blobResponse struct {
	Content  string `json:"content"`
	Encoding string `json:"encoding"` // "base64"
}

func (c *Connector) fetchFiles(ctx context.Context, repo string, yield func(plugins.RawDoc, error) bool) bool {
	var info repoInfo
	if err := c.getJSON(ctx, fmt.Sprintf("%s/repos/%s", c.baseURL, repo), &info); err != nil {
		return yield(plugins.RawDoc{}, fmt.Errorf("github %s: default branch: %w", repo, err))
	}
	branch := info.DefaultBranch
	if branch == "" {
		branch = "main"
	}
	var tree treeResponse
	if err := c.getJSON(ctx, fmt.Sprintf("%s/repos/%s/git/trees/%s?recursive=1", c.baseURL, repo, branch), &tree); err != nil {
		return yield(plugins.RawDoc{}, fmt.Errorf("github %s: tree: %w", repo, err))
	}
	for _, e := range tree.Tree {
		if ctx.Err() != nil {
			return false
		}
		if e.Type != "blob" || !c.fileMatches(e.Path) || !doctext.Supported(e.Path) {
			continue
		}
		var blob blobResponse
		if err := c.getJSON(ctx, fmt.Sprintf("%s/repos/%s/git/blobs/%s", c.baseURL, repo, e.SHA), &blob); err != nil {
			if !yield(plugins.RawDoc{}, err) {
				return false
			}
			continue // one bad blob doesn't abort the repo (#241)
		}
		data, err := decodeBlob(blob)
		if err != nil {
			if !yield(plugins.RawDoc{}, err) {
				return false
			}
			continue
		}
		text, err := doctext.ToText(path.Base(e.Path), data)
		if err != nil || strings.TrimSpace(text) == "" {
			continue // unconvertible/empty file — skip
		}
		doc := plugins.RawDoc{
			Text:          text,
			SourceURI:     fmt.Sprintf("github://%s/%s", repo, e.Path),
			SourceLocator: map[string]any{"repo": repo, "path": e.Path, "kind": "file"},
			Metadata:      map[string]any{"source": "github", "kind": "file", "repo": repo, "path": e.Path},
		}
		if !yield(doc, nil) {
			return false
		}
	}
	return true
}

func decodeBlob(b blobResponse) ([]byte, error) {
	if b.Encoding != "base64" {
		return []byte(b.Content), nil
	}
	// GitHub wraps base64 blob content at 76 cols; strip whitespace before decoding.
	clean := strings.NewReplacer("\n", "", "\r", "", " ", "").Replace(b.Content)
	return base64.StdEncoding.DecodeString(clean)
}

// fileMatches reports whether path p matches any configured glob.
func (c *Connector) fileMatches(p string) bool {
	for _, g := range c.files {
		if matchGlob(strings.TrimSpace(g), p) {
			return true
		}
	}
	return false
}

// matchGlob supports three forms: "dir/**" (subtree prefix), a glob containing "/"
// (path.Match on the full path), and a bare glob (matched against the basename).
func matchGlob(glob, p string) bool {
	switch {
	case glob == "":
		return false
	case strings.HasSuffix(glob, "/**"):
		return strings.HasPrefix(p, strings.TrimSuffix(glob, "**")) // keeps trailing "/"
	case strings.Contains(glob, "/"):
		ok, _ := path.Match(glob, p)
		return ok
	default:
		ok, _ := path.Match(glob, path.Base(p))
		return ok
	}
}

// getJSON performs an authenticated GET and decodes the JSON body into out.
func (c *Connector) getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("github request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		return fmt.Errorf("github: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
