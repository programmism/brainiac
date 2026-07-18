// Package linear implements the plugins.SourceConnector seam against the Linear
// GraphQL API (#240): it reads a workspace's issues (title + markdown
// description) so a team's decisions and specs become searchable memory. Blind
// connector, unit-tested against a fake Linear GraphQL endpoint.
//
// Auth is a Linear API key (personal key or OAuth token), sent in the
// Authorization header; pass it via LINEAR_TOKEN. Read-only. Linear descriptions
// are already Markdown, so no format conversion is needed. Confluence (XHTML) and
// Jira (ADF) are tracked as follow-ups.
package linear

import (
	"bytes"
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
	defaultBaseURL = "https://api.linear.app/graphql"
	pageSize       = 100
)

// Connector reads issues from the Linear workspace the API key can see.
type Connector struct {
	token   string
	baseURL string
	client  *http.Client
}

// Option customizes a Connector.
type Option func(*Connector)

// WithBaseURL overrides the GraphQL endpoint (used in tests).
func WithBaseURL(u string) Option {
	return func(c *Connector) { c.baseURL = strings.TrimRight(u, "/") }
}

// WithHTTPClient overrides the HTTP client.
func WithHTTPClient(h *http.Client) Option { return func(c *Connector) { c.client = h } }

// New builds a Linear connector for the given API key.
func New(token string, opts ...Option) *Connector {
	c := &Connector{token: token, baseURL: defaultBaseURL, client: &http.Client{Timeout: 30 * time.Second}}
	for _, o := range opts {
		o(c)
	}
	return c
}

var _ plugins.SourceConnector = (*Connector)(nil)

const issuesQuery = `query Issues($after: String) {
  issues(first: %d, after: $after) {
    nodes { identifier title description updatedAt url }
    pageInfo { hasNextPage endCursor }
  }
}`

type issue struct {
	Identifier  string `json:"identifier"`
	Title       string `json:"title"`
	Description string `json:"description"`
	UpdatedAt   string `json:"updatedAt"`
	URL         string `json:"url"`
}

type graphResponse struct {
	Data struct {
		Issues struct {
			Nodes    []issue `json:"nodes"`
			PageInfo struct {
				HasNextPage bool   `json:"hasNextPage"`
				EndCursor   string `json:"endCursor"`
			} `json:"pageInfo"`
		} `json:"issues"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// Fetch yields one RawDoc per issue across the workspace, cursor-paginated.
func (c *Connector) Fetch(ctx context.Context) iter.Seq2[plugins.RawDoc, error] {
	return func(yield func(plugins.RawDoc, error) bool) {
		after := ""
		for {
			if ctx.Err() != nil {
				return
			}
			resp, err := c.query(ctx, after)
			if err != nil {
				yield(plugins.RawDoc{}, err)
				return
			}
			for _, it := range resp.Data.Issues.Nodes {
				doc, ok := issueDoc(it)
				if !ok {
					continue
				}
				if !yield(doc, nil) {
					return
				}
			}
			if !resp.Data.Issues.PageInfo.HasNextPage || resp.Data.Issues.PageInfo.EndCursor == "" {
				return
			}
			after = resp.Data.Issues.PageInfo.EndCursor
		}
	}
}

// Watch yields an upsert per issue so ingest's content-hash step skips unchanged
// chunks. Persisted cursors for true incremental sync are a later refinement (#323).
func (c *Connector) Watch(ctx context.Context) iter.Seq2[plugins.Change, error] {
	return func(yield func(plugins.Change, error) bool) {
		for d, err := range c.Fetch(ctx) {
			if err != nil {
				if !yield(plugins.Change{}, err) {
					return
				}
				continue
			}
			if !yield(plugins.Change{SourceURI: d.SourceURI, Kind: plugins.ChangeUpserted}, nil) {
				return
			}
		}
	}
}

func issueDoc(it issue) (plugins.RawDoc, bool) {
	title := strings.TrimSpace(it.Title)
	body := strings.TrimSpace(it.Description)
	if title == "" && body == "" {
		return plugins.RawDoc{}, false
	}
	text := title
	if body != "" {
		text = strings.TrimSpace(title + "\n\n" + body)
	}
	uri := it.URL
	if uri == "" {
		uri = "linear://" + it.Identifier
	}
	return plugins.RawDoc{
		Text:          text,
		SourceURI:     uri,
		SourceLocator: map[string]any{"identifier": it.Identifier},
		Metadata:      map[string]any{"source": "linear", "identifier": it.Identifier},
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

// query runs one page of the issues query.
func (c *Connector) query(ctx context.Context, after string) (*graphResponse, error) {
	vars := map[string]any{}
	if after != "" {
		vars["after"] = after
	}
	payload, err := json.Marshal(map[string]any{
		"query":     fmt.Sprintf(issuesQuery, pageSize),
		"variables": vars,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", c.token) // Linear API keys go raw in Authorization

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("linear request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		return nil, fmt.Errorf("linear: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out graphResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode linear response: %w", err)
	}
	if len(out.Errors) > 0 {
		return nil, fmt.Errorf("linear graphql error: %s", out.Errors[0].Message)
	}
	return &out, nil
}
