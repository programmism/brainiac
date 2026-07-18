// Package github implements the plugins.SourceConnector seam against the GitHub
// REST API (#238): it reads a repository's issues and pull requests (title +
// body) so a team's decisions and discussions become searchable memory. It is a
// blind connector — unit-tested against a fake GitHub API, not a live token.
//
// Auth is a GitHub token (classic or fine-grained PAT) with `repo` read scope;
// pass it via GITHUB_TOKEN. Read-only. Beyond issues + PRs, it can (opt-in, via
// WithFiles / GITHUB_FILES) also ingest a repo's tracked files matching path globs
// as documents (#354). GitHub Discussions (GraphQL) and Link-header pagination are
// tracked as follow-ups.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strings"
	"time"

	"github.com/programmism/brainiac/internal/plugins"
)

const (
	defaultBaseURL = "https://api.github.com"
	apiVersion     = "2022-11-28"
	perPage        = 100
)

// Connector reads issues + PRs from the given "owner/repo" repositories, and
// (opt-in) tracked files matching a set of path globs.
type Connector struct {
	token   string
	baseURL string
	client  *http.Client
	repos   []string
	files   []string // path globs to also ingest as documents (#354); empty = off
}

// Option customizes a Connector.
type Option func(*Connector)

// WithBaseURL overrides the API base URL (used in tests).
func WithBaseURL(u string) Option {
	return func(c *Connector) { c.baseURL = strings.TrimRight(u, "/") }
}

// WithHTTPClient overrides the HTTP client.
func WithHTTPClient(h *http.Client) Option { return func(c *Connector) { c.client = h } }

// WithFiles opts into ingesting a repo's tracked files whose path matches any of
// the given globs (e.g. "README*", "docs/**", "*.md") as documents, in addition to
// issues/PRs (#354). Empty = off (issues/PRs only, the default). Globs: "dir/**"
// matches a subtree, a glob with "/" is path.Match on the full path, otherwise it
// matches the basename.
func WithFiles(globs []string) Option {
	return func(c *Connector) { c.files = append(c.files, globs...) }
}

// New builds a GitHub connector for the given token and "owner/repo" list.
func New(token string, repos []string, opts ...Option) *Connector {
	c := &Connector{token: token, baseURL: defaultBaseURL, client: &http.Client{Timeout: 30 * time.Second}}
	c.repos = append(c.repos, repos...)
	for _, o := range opts {
		o(c)
	}
	return c
}

// NewForRepos is an alias for a targeted import of specific repos.
func NewForRepos(token string, repos []string, opts ...Option) *Connector {
	return New(token, repos, opts...)
}

var _ plugins.SourceConnector = (*Connector)(nil)

type issue struct {
	Number      int                    `json:"number"`
	Title       string                 `json:"title"`
	Body        string                 `json:"body"`
	HTMLURL     string                 `json:"html_url"`
	UpdatedAt   string                 `json:"updated_at"`
	User        struct{ Login string } `json:"user"`
	PullRequest *struct{}              `json:"pull_request"` // present only on PRs
}

// Fetch yields one RawDoc per issue and pull request across the configured repos.
func (c *Connector) Fetch(ctx context.Context) iter.Seq2[plugins.RawDoc, error] {
	return func(yield func(plugins.RawDoc, error) bool) {
		for _, repo := range c.repos {
			if ctx.Err() != nil {
				return
			}
			for page := 1; ; page++ {
				items, err := c.issuesPage(ctx, repo, page)
				if err != nil {
					if !yield(plugins.RawDoc{}, err) {
						return
					}
					break // move to the next repo on error (non-fatal per #241)
				}
				for _, it := range items {
					doc, ok := issueDoc(repo, it)
					if !ok {
						continue
					}
					if !yield(doc, nil) {
						return
					}
				}
				if len(items) < perPage {
					break
				}
			}
			// Opt-in: also ingest tracked files matching the configured globs (#354).
			if len(c.files) > 0 {
				if !c.fetchFiles(ctx, repo, yield) {
					return
				}
			}
		}
	}
}

// Watch yields an upsert per issue/PR so ingest's content-hash step skips
// unchanged chunks. Persisted per-repo cursors are a later refinement (#323).
func (c *Connector) Watch(ctx context.Context) iter.Seq2[plugins.Change, error] {
	return func(yield func(plugins.Change, error) bool) {
		for doc, err := range c.Fetch(ctx) {
			if err != nil {
				if !yield(plugins.Change{}, err) {
					return
				}
				continue
			}
			if !yield(plugins.Change{SourceURI: doc.SourceURI, Kind: plugins.ChangeUpserted}, nil) {
				return
			}
		}
	}
}

// issueDoc converts an issue/PR to a RawDoc, or ok=false to skip an empty one.
func issueDoc(repo string, it issue) (plugins.RawDoc, bool) {
	title := strings.TrimSpace(it.Title)
	body := strings.TrimSpace(it.Body)
	if title == "" && body == "" {
		return plugins.RawDoc{}, false
	}
	text := title
	if body != "" {
		text = strings.TrimSpace(title + "\n\n" + body)
	}
	kind := "issue"
	if it.PullRequest != nil {
		kind = "pr"
	}
	uri := it.HTMLURL
	if uri == "" {
		uri = fmt.Sprintf("github://%s/%s/%d", repo, kind, it.Number)
	}
	return plugins.RawDoc{
		Text:          text,
		SourceURI:     uri,
		SourceLocator: map[string]any{"repo": repo, "number": it.Number, "kind": kind},
		Metadata:      map[string]any{"source": "github", "kind": kind, "author": it.User.Login, "repo": repo},
		ModifiedAt:    parseTime(it.UpdatedAt),
	}, true
}

func parseTime(s string) *time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	return &t
}

// issuesPage fetches one page of a repo's issues (which the REST API returns
// together with pull requests).
func (c *Connector) issuesPage(ctx context.Context, repo string, page int) ([]issue, error) {
	url := fmt.Sprintf("%s/repos/%s/issues?state=all&per_page=%d&page=%d", c.baseURL, repo, perPage, page)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github issues request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		return nil, fmt.Errorf("github %s issues: status %d: %s", repo, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out []issue
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode github response: %w", err)
	}
	return out, nil
}
