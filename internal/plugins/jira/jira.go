// Package jira implements the plugins.SourceConnector seam against the Jira
// Cloud REST API (#343): it reads a site's issues (summary + description) so a
// team's tickets and the decisions in them become searchable memory. Blind
// connector, unit-tested against a fake Jira endpoint.
//
// Auth is Basic email:token (a Jira Cloud API token); pass the site base URL, the
// account email, and the token. Read-only. Issue descriptions are ADF (Atlassian
// Document Format — nested JSON), so they are walked to plain text here; a
// pre-v3 string description is used as-is.
package jira

import (
	"context"
	"encoding/base64"
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

const pageSize = 50

// Connector reads issues from the Jira site the credentials can see.
type Connector struct {
	baseURL string
	email   string
	token   string
	client  *http.Client
}

// Option customizes a Connector.
type Option func(*Connector)

// WithBaseURL overrides the site base URL (used in tests).
func WithBaseURL(u string) Option {
	return func(c *Connector) { c.baseURL = strings.TrimRight(u, "/") }
}

// WithHTTPClient overrides the HTTP client.
func WithHTTPClient(h *http.Client) Option { return func(c *Connector) { c.client = h } }

// New builds a Jira connector for the given site base URL (e.g.
// https://your-site.atlassian.net), account email, and API token.
func New(baseURL, email, token string, opts ...Option) *Connector {
	c := &Connector{
		baseURL: strings.TrimRight(baseURL, "/"),
		email:   email,
		token:   token,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

var _ plugins.SourceConnector = (*Connector)(nil)

type issue struct {
	Key    string `json:"key"`
	Fields struct {
		Summary     string          `json:"summary"`
		Description json.RawMessage `json:"description"`
		Updated     string          `json:"updated"`
	} `json:"fields"`
}

type searchResponse struct {
	Issues     []issue `json:"issues"`
	StartAt    int     `json:"startAt"`
	MaxResults int     `json:"maxResults"`
	Total      int     `json:"total"`
}

// Fetch yields one RawDoc per issue across the site, offset-paginated.
func (c *Connector) Fetch(ctx context.Context) iter.Seq2[plugins.RawDoc, error] {
	return func(yield func(plugins.RawDoc, error) bool) {
		startAt := 0
		for {
			if ctx.Err() != nil {
				return
			}
			resp, err := c.search(ctx, startAt)
			if err != nil {
				yield(plugins.RawDoc{}, err)
				return
			}
			for _, it := range resp.Issues {
				doc, ok := c.issueDoc(it)
				if !ok {
					continue
				}
				if !yield(doc, nil) {
					return
				}
			}
			startAt += len(resp.Issues)
			if len(resp.Issues) == 0 || startAt >= resp.Total {
				return
			}
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

func (c *Connector) issueDoc(it issue) (plugins.RawDoc, bool) {
	title := strings.TrimSpace(it.Fields.Summary)
	body := strings.TrimSpace(descriptionText(it.Fields.Description))
	if title == "" && body == "" {
		return plugins.RawDoc{}, false
	}
	text := title
	if body != "" {
		text = strings.TrimSpace(title + "\n\n" + body)
	}
	return plugins.RawDoc{
		Text:          text,
		SourceURI:     c.baseURL + "/browse/" + it.Key,
		SourceLocator: map[string]any{"key": it.Key},
		Metadata:      map[string]any{"source": "jira", "key": it.Key},
		ModifiedAt:    parseTime(it.Fields.Updated),
	}, true
}

// descriptionText renders a Jira description to plain text. The v3 API returns
// ADF (a JSON object); older setups return a plain string.
func descriptionText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	return adfToText(v)
}

// parseTime accepts both RFC3339 and Jira's default "2006-01-02T15:04:05.000-0700".
func parseTime(s string) *time.Time {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05.000-0700"} {
		if t, err := time.Parse(layout, s); err == nil {
			return &t
		}
	}
	return nil
}

// search runs one page of the issue search (JQL empty = all visible issues).
func (c *Connector) search(ctx context.Context, startAt int) (*searchResponse, error) {
	u := fmt.Sprintf("%s/rest/api/3/search?%s", c.baseURL, url.Values{
		"startAt":    {fmt.Sprint(startAt)},
		"maxResults": {fmt.Sprint(pageSize)},
		"fields":     {"summary,description,updated"},
	}.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(c.email+":"+c.token)))

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jira request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		return nil, fmt.Errorf("jira: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out searchResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode jira response: %w", err)
	}
	return &out, nil
}
