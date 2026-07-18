// Package gitlab implements the plugins.SourceConnector seam against the GitLab
// API (#340): it reads a project's issues and merge requests (title + Markdown
// description) so a team's decisions and discussions become searchable memory.
// Blind connector — unit-tested against a fake GitLab API, not a live token.
//
// Auth is a GitLab token (personal/project/group access token) with `read_api`
// scope, sent in the PRIVATE-TOKEN header; pass it via GITLAB_TOKEN. The host is
// configurable (GITLAB_BASE_URL) for self-managed instances, default gitlab.com.
// Read-only. Descriptions are already Markdown (like Linear), so no format
// conversion is needed. Code/file ingestion, Discussions, and Link-header
// pagination are tracked as follow-ups; this slice is issues + MRs.
package gitlab

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/programmism/brainiac/internal/plugins"
)

const (
	defaultBaseURL = "https://gitlab.com"
	perPage        = 100
)

// Connector reads issues + merge requests from the given "group/project" paths.
type Connector struct {
	token    string
	baseURL  string
	client   *http.Client
	projects []string
}

// Option customizes a Connector.
type Option func(*Connector)

// WithBaseURL overrides the API host (self-managed GitLab, and tests).
func WithBaseURL(u string) Option {
	return func(c *Connector) { c.baseURL = strings.TrimRight(u, "/") }
}

// WithHTTPClient overrides the HTTP client.
func WithHTTPClient(h *http.Client) Option { return func(c *Connector) { c.client = h } }

// New builds a GitLab connector for the given token and "group/project" list.
// An empty baseURL defaults to gitlab.com.
func New(token, baseURL string, projects []string, opts ...Option) *Connector {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	c := &Connector{token: token, baseURL: strings.TrimRight(baseURL, "/"), client: &http.Client{Timeout: 30 * time.Second}}
	c.projects = append(c.projects, projects...)
	for _, o := range opts {
		o(c)
	}
	return c
}

var _ plugins.SourceConnector = (*Connector)(nil)

type item struct {
	IID         int                       `json:"iid"`
	Title       string                    `json:"title"`
	Description string                    `json:"description"`
	WebURL      string                    `json:"web_url"`
	UpdatedAt   string                    `json:"updated_at"`
	Author      struct{ Username string } `json:"author"`
}

// Fetch yields one RawDoc per issue and merge request across the configured
// projects. A failure on one project surfaces the error and moves to the next.
func (c *Connector) Fetch(ctx context.Context) iter.Seq2[plugins.RawDoc, error] {
	return func(yield func(plugins.RawDoc, error) bool) {
		for _, project := range c.projects {
			if ctx.Err() != nil {
				return
			}
			for _, kind := range []struct{ endpoint, label string }{
				{"issues", "issue"}, {"merge_requests", "mr"},
			} {
				if !c.fetchKind(ctx, project, kind.endpoint, kind.label, yield) {
					return
				}
			}
		}
	}
}

// fetchKind pages one endpoint (issues or merge_requests) for a project, yielding
// a doc per item. It returns false if the consumer asked to stop.
func (c *Connector) fetchKind(ctx context.Context, project, endpoint, label string, yield func(plugins.RawDoc, error) bool) bool {
	for page := 1; ; page++ {
		if ctx.Err() != nil {
			return false
		}
		items, err := c.page(ctx, project, endpoint, page)
		if err != nil {
			if !yield(plugins.RawDoc{}, err) {
				return false
			}
			return true // non-fatal: move on to the next endpoint/project
		}
		for _, it := range items {
			doc, ok := itemDoc(project, label, it)
			if !ok {
				continue
			}
			if !yield(doc, nil) {
				return false
			}
		}
		if len(items) < perPage {
			return true
		}
	}
}

// Watch yields an upsert per item so ingest's content-hash step skips unchanged
// chunks. Persisted per-project cursors are a later refinement (#323).
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

func itemDoc(project, kind string, it item) (plugins.RawDoc, bool) {
	title := strings.TrimSpace(it.Title)
	body := strings.TrimSpace(it.Description)
	if title == "" && body == "" {
		return plugins.RawDoc{}, false
	}
	text := title
	if body != "" {
		text = strings.TrimSpace(title + "\n\n" + body)
	}
	uri := it.WebURL
	if uri == "" {
		uri = fmt.Sprintf("gitlab://%s/%s/%d", project, kind, it.IID)
	}
	return plugins.RawDoc{
		Text:          text,
		SourceURI:     uri,
		SourceLocator: map[string]any{"project": project, "iid": it.IID, "kind": kind},
		Metadata:      map[string]any{"source": "gitlab", "kind": kind, "author": it.Author.Username, "project": project},
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

// page fetches one page of a project's issues or merge requests. The project path
// is URL-encoded into the :id position per the GitLab API.
func (c *Connector) page(ctx context.Context, project, endpoint string, page int) ([]item, error) {
	u := fmt.Sprintf("%s/api/v4/projects/%s/%s?scope=all&per_page=%d&page=%d",
		c.baseURL, url.PathEscape(project), endpoint, perPage, page)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("PRIVATE-TOKEN", c.token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gitlab %s request: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		return nil, fmt.Errorf("gitlab %s %s: status %d: %s", project, endpoint, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out []item
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode gitlab response: %w", err)
	}
	return out, nil
}
