// Package gmail implements the plugins.SourceConnector seam against the Gmail API
// (#245): it reads a mailbox's messages (subject + plain-text body) so email
// threads — decisions, approvals, context — become searchable memory. Blind
// connector, unit-tested against a fake Gmail API, not a live mailbox.
//
// Auth is a Google OAuth access token with a Gmail read scope
// (gmail.readonly), sent as a Bearer token; pass it via GMAIL_TOKEN (minting/
// refreshing it is the per-source OAuth store, #246). Read-only. An optional
// GMAIL_QUERY (Gmail search syntax, e.g. "label:important newer_than:30d") narrows
// what's imported; empty imports the mailbox.
package gmail

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/programmism/brainiac/internal/plugins"
)

const (
	defaultBaseURL = "https://gmail.googleapis.com"
	pageSize       = 100
)

// Connector reads messages the token's mailbox can see, optionally filtered by a
// Gmail search query.
type Connector struct {
	token   string
	query   string
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

// WithQuery sets a Gmail search query to narrow the import (empty = whole mailbox).
func WithQuery(q string) Option { return func(c *Connector) { c.query = q } }

// New builds a Gmail connector for the given OAuth access token.
func New(token string, opts ...Option) *Connector {
	c := &Connector{token: token, baseURL: defaultBaseURL, client: &http.Client{Timeout: 30 * time.Second}}
	for _, o := range opts {
		o(c)
	}
	return c
}

var _ plugins.SourceConnector = (*Connector)(nil)

type messageRef struct {
	ID string `json:"id"`
}

type listResponse struct {
	Messages      []messageRef `json:"messages"`
	NextPageToken string       `json:"nextPageToken"`
}

type message struct {
	ID           string `json:"id"`
	Snippet      string `json:"snippet"`
	InternalDate string `json:"internalDate"` // epoch milliseconds, as a string
	Payload      part   `json:"payload"`
}

type part struct {
	MimeType string   `json:"mimeType"`
	Headers  []header `json:"headers"`
	Body     struct {
		Data string `json:"data"` // base64url
	} `json:"body"`
	Parts []part `json:"parts"`
}

type header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Fetch yields one RawDoc per message: its subject + plain-text body.
func (c *Connector) Fetch(ctx context.Context) iter.Seq2[plugins.RawDoc, error] {
	return func(yield func(plugins.RawDoc, error) bool) {
		pageToken := ""
		for {
			if ctx.Err() != nil {
				return
			}
			list, err := c.listPage(ctx, pageToken)
			if err != nil {
				yield(plugins.RawDoc{}, err)
				return
			}
			for _, ref := range list.Messages {
				msg, err := c.getMessage(ctx, ref.ID)
				if err != nil {
					if !yield(plugins.RawDoc{}, err) {
						return
					}
					continue
				}
				doc, ok := messageDoc(msg)
				if !ok {
					continue
				}
				if !yield(doc, nil) {
					return
				}
			}
			if list.NextPageToken == "" {
				return
			}
			pageToken = list.NextPageToken
		}
	}
}

// Watch yields an upsert per message so ingest's content-hash step skips unchanged
// ones. Persisted history cursors are a later refinement (#415).
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

// messageDoc turns a message into a RawDoc, or ok=false to skip an empty one.
func messageDoc(m message) (plugins.RawDoc, bool) {
	subject := ""
	for _, h := range m.Payload.Headers {
		if strings.EqualFold(h.Name, "Subject") {
			subject = strings.TrimSpace(h.Value)
			break
		}
	}
	body := strings.TrimSpace(plainText(m.Payload))
	if body == "" {
		body = strings.TrimSpace(m.Snippet) // fall back to the API's snippet
	}
	if subject == "" && body == "" {
		return plugins.RawDoc{}, false
	}
	text := subject
	if body != "" {
		text = strings.TrimSpace(subject + "\n\n" + body)
	}
	return plugins.RawDoc{
		Text:          text,
		SourceURI:     "gmail://" + m.ID,
		SourceLocator: map[string]any{"message_id": m.ID},
		Metadata:      map[string]any{"source": "gmail", "subject": subject},
		ModifiedAt:    epochMillis(m.InternalDate),
	}, true
}

// plainText walks a message's MIME tree and returns the first text/plain body it
// finds (recursing into multipart containers), base64url-decoded.
func plainText(p part) string {
	if strings.EqualFold(p.MimeType, "text/plain") {
		if s := decodeBody(p.Body.Data); s != "" {
			return s
		}
	}
	for _, sub := range p.Parts {
		if s := plainText(sub); s != "" {
			return s
		}
	}
	return ""
}

// decodeBody base64url-decodes a Gmail body payload (Gmail uses URL-safe base64,
// usually unpadded).
func decodeBody(data string) string {
	if data == "" {
		return ""
	}
	if b, err := base64.RawURLEncoding.DecodeString(data); err == nil {
		return string(b)
	}
	if b, err := base64.URLEncoding.DecodeString(data); err == nil {
		return string(b)
	}
	return ""
}

func epochMillis(s string) *time.Time {
	ms, err := strconv.ParseInt(s, 10, 64)
	if err != nil || ms <= 0 {
		return nil
	}
	t := time.UnixMilli(ms).UTC()
	return &t
}

func (c *Connector) listPage(ctx context.Context, pageToken string) (listResponse, error) {
	q := url.Values{}
	q.Set("maxResults", strconv.Itoa(pageSize))
	if c.query != "" {
		q.Set("q", c.query)
	}
	if pageToken != "" {
		q.Set("pageToken", pageToken)
	}
	var out listResponse
	err := c.getJSON(ctx, "/gmail/v1/users/me/messages?"+q.Encode(), &out)
	return out, err
}

func (c *Connector) getMessage(ctx context.Context, id string) (message, error) {
	var out message
	err := c.getJSON(ctx, "/gmail/v1/users/me/messages/"+url.PathEscape(id)+"?format=full", &out)
	return out, err
}

func (c *Connector) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("gmail request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		return fmt.Errorf("gmail: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
