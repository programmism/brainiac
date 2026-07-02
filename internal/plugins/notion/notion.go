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
	"strconv"
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
// If pages is non-empty, only those specific pages are fetched (by id).
type Connector struct {
	token   string
	baseURL string
	client  *http.Client
	pages   []string
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

// NewForPages builds a connector that fetches only the given pages (by id or
// URL), for targeted imports (e.g. "import this Notion link").
func NewForPages(token string, pages []string, opts ...Option) *Connector {
	c := New(token, opts...)
	for _, p := range pages {
		c.pages = append(c.pages, ParsePageID(p))
	}
	return c
}

var _ plugins.SourceConnector = (*Connector)(nil)

// ParsePageID extracts the 32-hex Notion page id from a page URL (or returns the
// input if it already looks like an id).
func ParsePageID(s string) string {
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	compact := strings.ReplaceAll(s, "-", "")
	if len(compact) >= 32 {
		if tail := compact[len(compact)-32:]; isHex(tail) {
			return tail
		}
	}
	return s
}

func isHex(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// Fetch yields pages as RawDocs (title + block text) with the page URL as
// provenance — either the specific pages set via NewForPages, or every page the
// integration can read.
func (c *Connector) Fetch(ctx context.Context) iter.Seq2[plugins.RawDoc, error] {
	if len(c.pages) > 0 {
		return c.fetchPages(ctx)
	}
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

// fetchPages yields the specific pages requested via NewForPages.
func (c *Connector) fetchPages(ctx context.Context) iter.Seq2[plugins.RawDoc, error] {
	return func(yield func(plugins.RawDoc, error) bool) {
		for _, id := range c.pages {
			var page map[string]any
			if err := c.do(ctx, http.MethodGet, "/v1/pages/"+id, nil, &page); err != nil {
				if !yield(plugins.RawDoc{}, err) {
					return
				}
				continue
			}
			doc, err := c.pageToDoc(ctx, page)
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
	}
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
	edited := stringField(page, "last_edited_time")
	var modified *time.Time
	if t, err := time.Parse(time.RFC3339, edited); err == nil {
		modified = &t
	}
	return plugins.RawDoc{
		Text:      full,
		SourceURI: url,
		SourceLocator: map[string]any{
			"page_id":          id,
			"last_edited_time": edited,
		},
		Metadata:   map[string]any{"source": "notion"},
		ModifiedAt: modified,
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
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}

	backoff := 500 * time.Millisecond
	for attempt := 0; attempt < 4; attempt++ {
		var reader io.Reader
		if payload != nil {
			reader = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Notion-Version", notionVersion)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.client.Do(req)
		if err != nil {
			return err
		}

		// Honor Notion rate limiting (429 + Retry-After).
		if resp.StatusCode == http.StatusTooManyRequests {
			wait := retryAfter(resp.Header.Get("Retry-After"), backoff)
			_ = resp.Body.Close()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			backoff *= 2
			continue
		}
		if resp.StatusCode != http.StatusOK {
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
			_ = resp.Body.Close()
			return fmt.Errorf("notion %s %s: status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(msg)))
		}
		err = json.NewDecoder(resp.Body).Decode(out)
		_ = resp.Body.Close()
		return err
	}
	return fmt.Errorf("notion %s %s: rate-limited after retries", method, path)
}

// retryAfter parses a Retry-After header (seconds), falling back to backoff.
func retryAfter(header string, fallback time.Duration) time.Duration {
	if header != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(header)); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return fallback
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
