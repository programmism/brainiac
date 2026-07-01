// Package notion implements the plugins.SourceConnector seam against the Notion
// API (ADR 0002). It enumerates pages, flattens their block text, and reports
// changes via last_edited_time for actualization.
package notion

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

// notionVersion pins a known-good API version (sent as the Notion-Version header).
const notionVersion = "2022-06-28"

const defaultBaseURL = "https://api.notion.com"

// maxBlockDepth bounds recursion into nested child blocks.
const maxBlockDepth = 3

// Connector reads pages from a Notion workspace the integration can access.
type Connector struct {
	token   string
	baseURL string
	client  *http.Client
}

// Option customizes a Connector.
type Option func(*Connector)

// WithBaseURL overrides the API base URL (used in tests).
func WithBaseURL(u string) Option {
	return func(c *Connector) { c.baseURL = strings.TrimRight(u, "/") }
}

// WithHTTPClient overrides the HTTP client.
func WithHTTPClient(h *http.Client) Option { return func(c *Connector) { c.client = h } }

// New builds a Notion connector for the given integration token.
func New(token string, opts ...Option) *Connector {
	c := &Connector{token: token, baseURL: defaultBaseURL, client: &http.Client{Timeout: 30 * time.Second}}
	for _, o := range opts {
		o(c)
	}
	return c
}

var _ plugins.SourceConnector = (*Connector)(nil)

// Fetch yields every page the integration can read, as a RawDoc (title + block
// text) with the page URL as provenance.
func (c *Connector) Fetch(ctx context.Context) iter.Seq2[plugins.RawDoc, error] {
	return func(yield func(plugins.RawDoc, error) bool) {
		var cursor string
		for {
			page, err := c.searchPages(ctx, cursor)
			if err != nil {
				yield(plugins.RawDoc{}, err)
				return
			}
			for _, p := range page.Results {
				doc, err := c.pageToDoc(ctx, p)
				if err != nil {
					if !yield(plugins.RawDoc{}, err) {
						return
					}
					continue
				}
				if !yield(doc, nil) {
					return
				}
			}
			if !page.HasMore || page.NextCursor == nil {
				return
			}
			cursor = *page.NextCursor
		}
	}
}

// Watch yields a change for every page, letting the ingest content-hash step
// skip unchanged chunks. (A persisted cursor for true incremental sync is a
// later refinement — see ADR 0002.)
func (c *Connector) Watch(ctx context.Context) iter.Seq2[plugins.Change, error] {
	return func(yield func(plugins.Change, error) bool) {
		var cursor string
		for {
			page, err := c.searchPages(ctx, cursor)
			if err != nil {
				yield(plugins.Change{}, err)
				return
			}
			for _, p := range page.Results {
				if !yield(plugins.Change{SourceURI: stringField(p, "url"), Kind: plugins.ChangeUpserted}, nil) {
					return
				}
			}
			if !page.HasMore || page.NextCursor == nil {
				return
			}
			cursor = *page.NextCursor
		}
	}
}

// --- Notion API ---

type listResponse struct {
	Results    []map[string]any `json:"results"`
	HasMore    bool             `json:"has_more"`
	NextCursor *string          `json:"next_cursor"`
}

func (c *Connector) searchPages(ctx context.Context, cursor string) (*listResponse, error) {
	body := map[string]any{
		"filter":    map[string]any{"property": "object", "value": "page"},
		"page_size": 100,
	}
	if cursor != "" {
		body["start_cursor"] = cursor
	}
	var out listResponse
	if err := c.do(ctx, http.MethodPost, "/v1/search", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Connector) pageToDoc(ctx context.Context, page map[string]any) (plugins.RawDoc, error) {
	id := stringField(page, "id")
	url := stringField(page, "url")
	title := pageTitle(page)

	text, err := c.blockText(ctx, id, 0)
	if err != nil {
		return plugins.RawDoc{}, err
	}
	full := title
	if text != "" {
		full = strings.TrimSpace(title + "\n\n" + text)
	}
	return plugins.RawDoc{
		Text:      full,
		SourceURI: url,
		SourceLocator: map[string]any{
			"page_id":          id,
			"last_edited_time": stringField(page, "last_edited_time"),
		},
		Metadata: map[string]any{"source": "notion"},
	}, nil
}

// blockText fetches and flattens a block's children (recursively, bounded).
func (c *Connector) blockText(ctx context.Context, blockID string, depth int) (string, error) {
	if depth >= maxBlockDepth {
		return "", nil
	}
	var lines []string
	cursor := ""
	for {
		path := "/v1/blocks/" + blockID + "/children?page_size=100"
		if cursor != "" {
			path += "&start_cursor=" + cursor
		}
		var out listResponse
		if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
			return "", err
		}
		for _, b := range out.Results {
			if line := richText(b); line != "" {
				lines = append(lines, line)
			}
			if hasChildren(b) {
				child, err := c.blockText(ctx, stringField(b, "id"), depth+1)
				if err != nil {
					return "", err
				}
				if child != "" {
					lines = append(lines, child)
				}
			}
		}
		if !out.HasMore || out.NextCursor == nil {
			break
		}
		cursor = *out.NextCursor
	}
	return strings.Join(lines, "\n"), nil
}

func (c *Connector) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Notion-Version", notionVersion)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		return fmt.Errorf("notion %s %s: status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// --- JSON helpers (Notion's shapes are dynamic) ---

func stringField(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func hasChildren(block map[string]any) bool {
	b, _ := block["has_children"].(bool)
	return b
}

// richText extracts the plain text of a block from its type-specific object.
func richText(block map[string]any) string {
	typ := stringField(block, "type")
	obj, ok := block[typ].(map[string]any)
	if !ok {
		return ""
	}
	arr, ok := obj["rich_text"].([]any)
	if !ok {
		return ""
	}
	var b strings.Builder
	for _, item := range arr {
		if m, ok := item.(map[string]any); ok {
			b.WriteString(stringField(m, "plain_text"))
		}
	}
	return b.String()
}

// pageTitle finds the page's title property and concatenates its plain text.
func pageTitle(page map[string]any) string {
	props, ok := page["properties"].(map[string]any)
	if !ok {
		return ""
	}
	for _, v := range props {
		prop, ok := v.(map[string]any)
		if !ok || stringField(prop, "type") != "title" {
			continue
		}
		arr, ok := prop["title"].([]any)
		if !ok {
			continue
		}
		var b strings.Builder
		for _, item := range arr {
			if m, ok := item.(map[string]any); ok {
				b.WriteString(stringField(m, "plain_text"))
			}
		}
		return b.String()
	}
	return ""
}
