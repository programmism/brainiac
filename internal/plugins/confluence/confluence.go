// Package confluence implements the plugins.SourceConnector seam against the
// Confluence Cloud REST API (#343): it reads a space's pages (title + body) so a
// team's wiki becomes searchable memory. Blind connector, unit-tested against a
// fake Confluence endpoint.
//
// Auth is Basic email:token (a Confluence Cloud API token); pass the site base
// URL (e.g. https://your-site.atlassian.net/wiki), the account email, and the
// token. Read-only. Page bodies are XHTML "storage format", stripped to text by
// reusing doctext's HTML path so the same tokenizer serves both.
package confluence

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

	"github.com/programmism/brainiac/internal/doctext"
	"github.com/programmism/brainiac/internal/plugins"
)

const pageSize = 50

// Connector reads pages from the Confluence site the credentials can see.
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

// New builds a Confluence connector for the given site base URL (…/wiki),
// account email, and API token.
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

type page struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Body  struct {
		Storage struct {
			Value string `json:"value"`
		} `json:"storage"`
	} `json:"body"`
	Version struct {
		When string `json:"when"`
	} `json:"version"`
	Links struct {
		WebUI string `json:"webui"`
	} `json:"_links"`
}

type contentResponse struct {
	Results []page `json:"results"`
	Size    int    `json:"size"`
	Links   struct {
		Next string `json:"next"`
	} `json:"_links"`
}

// Fetch yields one RawDoc per page across the site, offset-paginated.
func (c *Connector) Fetch(ctx context.Context) iter.Seq2[plugins.RawDoc, error] {
	return func(yield func(plugins.RawDoc, error) bool) {
		start := 0
		for {
			if ctx.Err() != nil {
				return
			}
			resp, err := c.list(ctx, start)
			if err != nil {
				yield(plugins.RawDoc{}, err)
				return
			}
			for _, p := range resp.Results {
				doc, ok := c.pageDoc(p)
				if !ok {
					continue
				}
				if !yield(doc, nil) {
					return
				}
			}
			start += len(resp.Results)
			// Confluence signals more pages via _links.next; also stop on a short page.
			if len(resp.Results) == 0 || resp.Links.Next == "" {
				return
			}
		}
	}
}

// Watch yields an upsert per page so ingest's content-hash step skips unchanged
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

func (c *Connector) pageDoc(p page) (plugins.RawDoc, bool) {
	title := strings.TrimSpace(p.Title)
	// Reuse doctext's HTML tokenizer for the XHTML storage body.
	body := ""
	if v := strings.TrimSpace(p.Body.Storage.Value); v != "" {
		if txt, err := doctext.ToText("page.html", []byte(v)); err == nil {
			body = strings.TrimSpace(txt)
		}
	}
	if title == "" && body == "" {
		return plugins.RawDoc{}, false
	}
	text := title
	if body != "" {
		text = strings.TrimSpace(title + "\n\n" + body)
	}
	uri := c.baseURL + "/pages/" + p.ID
	if p.Links.WebUI != "" {
		uri = c.baseURL + p.Links.WebUI
	}
	return plugins.RawDoc{
		Text:          text,
		SourceURI:     uri,
		SourceLocator: map[string]any{"id": p.ID},
		Metadata:      map[string]any{"source": "confluence", "id": p.ID},
		ModifiedAt:    parseTime(p.Version.When),
	}, true
}

func parseTime(s string) *time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	return &t
}

// list fetches one page of content (type=page), expanding the storage body and
// version so each result carries text + a modification time.
func (c *Connector) list(ctx context.Context, start int) (*contentResponse, error) {
	u := fmt.Sprintf("%s/rest/api/content?%s", c.baseURL, url.Values{
		"type":   {"page"},
		"start":  {fmt.Sprint(start)},
		"limit":  {fmt.Sprint(pageSize)},
		"expand": {"body.storage,version"},
	}.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(c.email+":"+c.token)))

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("confluence request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		return nil, fmt.Errorf("confluence: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out contentResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode confluence response: %w", err)
	}
	return &out, nil
}
