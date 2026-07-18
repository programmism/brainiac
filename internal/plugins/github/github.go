// Package github implements the plugins.SourceConnector seam against the GitHub
// REST API (#238): it reads a repository's issues and pull requests (title +
// body) so a team's decisions and discussions become searchable memory. It is a
// blind connector — unit-tested against a fake GitHub API, not a live token.
//
// Auth is a GitHub token (classic or fine-grained PAT) with `repo` read scope;
// pass it via GITHUB_TOKEN. Read-only. Issue/PR pages are followed via the
// Link: rel="next" header (#381). Beyond issues + PRs it can (opt-in) also ingest a
// repo's tracked files matching path globs (WithFiles / GITHUB_FILES, #354) and its
// GitHub Discussions via GraphQL (WithDiscussions / GITHUB_DISCUSSIONS, #381).
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
	token       string
	baseURL     string
	client      *http.Client
	repos       []string
	files       []string // path globs to also ingest as documents (#354); empty = off
	discussions bool     // opt-in: also ingest GitHub Discussions via GraphQL (#381)
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

// WithDiscussions opts into ingesting a repo's GitHub Discussions (title + body)
// as documents, in addition to issues/PRs (#381). Discussions are only exposed via
// the GraphQL API, so this issues a GraphQL query. Off by default.
func WithDiscussions() Option {
	return func(c *Connector) { c.discussions = true }
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
			// Follow GitHub's Link: rel="next" header across pages rather than
			// guessing from page size (#381) — robust to a final page that happens
			// to be exactly per_page long.
			url := fmt.Sprintf("%s/repos/%s/issues?state=all&per_page=%d", c.baseURL, repo, perPage)
			for url != "" {
				items, next, err := c.issuesPage(ctx, repo, url)
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
				url = next
			}
			// Opt-in: also ingest a repo's Discussions via GraphQL (#381).
			if c.discussions {
				if !c.fetchDiscussions(ctx, repo, yield) {
					return
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
// together with pull requests) from url, and returns the next page's URL parsed
// from the Link header ("" when there is no next page).
func (c *Connector) issuesPage(ctx context.Context, repo, url string) ([]issue, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("github issues request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		return nil, "", fmt.Errorf("github %s issues: status %d: %s", repo, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out []issue
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, "", fmt.Errorf("decode github response: %w", err)
	}
	return out, nextLink(resp.Header.Get("Link")), nil
}

// nextLink extracts the rel="next" URL from a GitHub Link header (#381), or "" if
// there is none. The header looks like:
//
//	<https://api.github.com/…&page=2>; rel="next", <…&page=9>; rel="last"
func nextLink(header string) string {
	if header == "" {
		return ""
	}
	for _, part := range strings.Split(header, ",") {
		segs := strings.Split(part, ";")
		if len(segs) < 2 {
			continue
		}
		isNext := false
		for _, s := range segs[1:] {
			if strings.TrimSpace(s) == `rel="next"` {
				isNext = true
				break
			}
		}
		if !isNext {
			continue
		}
		u := strings.TrimSpace(segs[0])
		u = strings.TrimPrefix(u, "<")
		u = strings.TrimSuffix(u, ">")
		return u
	}
	return ""
}
