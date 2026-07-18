// Package gdrive implements the plugins.SourceConnector seam against the Google
// Drive API v3 (#239): it lists the files an access token can see and pulls their
// text — Google Docs are exported to text/plain, plain-text/markdown files are
// downloaded — so a team's Drive becomes searchable memory. Blind connector,
// unit-tested against a fake Drive API.
//
// Auth is an OAuth2 **access token** (pass it via GDRIVE_TOKEN). Minting/refreshing
// that token (service account or OAuth flow) is out of scope here — the per-source
// credential/OAuth store is #246; this connector just uses the bearer token.
package gdrive

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
	defaultBaseURL = "https://www.googleapis.com"
	pageSize       = 100
	// googleDocMIME is exported to text/plain; text/* files are downloaded as-is.
	googleDocMIME = "application/vnd.google-apps.document"
)

// Connector reads text files (Google Docs + text/*) the access token can see.
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

// New builds a Google Drive connector for the given OAuth access token.
func New(token string, opts ...Option) *Connector {
	c := &Connector{token: token, baseURL: defaultBaseURL, client: &http.Client{Timeout: 30 * time.Second}}
	for _, o := range opts {
		o(c)
	}
	return c
}

var _ plugins.SourceConnector = (*Connector)(nil)

type file struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	MimeType     string `json:"mimeType"`
	ModifiedTime string `json:"modifiedTime"`
	WebViewLink  string `json:"webViewLink"`
}

type fileList struct {
	Files         []file `json:"files"`
	NextPageToken string `json:"nextPageToken"`
}

// Fetch yields one RawDoc per readable Google Doc / text file, paginated.
func (c *Connector) Fetch(ctx context.Context) iter.Seq2[plugins.RawDoc, error] {
	return func(yield func(plugins.RawDoc, error) bool) {
		pageToken := ""
		for {
			if ctx.Err() != nil {
				return
			}
			list, err := c.listFiles(ctx, pageToken)
			if err != nil {
				yield(plugins.RawDoc{}, err)
				return
			}
			for _, f := range list.Files {
				text, ok, err := c.fileText(ctx, f)
				if err != nil {
					if !yield(plugins.RawDoc{}, err) {
						return
					}
					continue // non-fatal per file (#241)
				}
				if !ok {
					continue // unsupported type, or empty
				}
				if !yield(doc(f, text), nil) {
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

// Watch yields an upsert per file so ingest's content-hash step skips unchanged
// chunks. Persisted change-token sync is a later refinement (#323).
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

func doc(f file, text string) plugins.RawDoc {
	uri := f.WebViewLink
	if uri == "" {
		uri = "gdrive://" + f.ID
	}
	return plugins.RawDoc{
		Text:          text,
		SourceURI:     uri,
		SourceLocator: map[string]any{"file_id": f.ID, "name": f.Name},
		Metadata:      map[string]any{"source": "gdrive", "mime_type": f.MimeType, "name": f.Name},
		ModifiedAt:    parseTime(f.ModifiedTime),
	}
}

func parseTime(s string) *time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	return &t
}

// fileText returns the extractable text for a file: Google Docs are exported to
// text/plain, text/* files are downloaded. ok=false for unsupported types (folders,
// binaries) or empty content, so they're skipped.
func (c *Connector) fileText(ctx context.Context, f file) (string, bool, error) {
	var raw string
	var err error
	switch {
	case f.MimeType == googleDocMIME:
		raw, err = c.get(ctx, fmt.Sprintf("/drive/v3/files/%s/export?mimeType=text/plain", f.ID))
	case strings.HasPrefix(f.MimeType, "text/"):
		raw, err = c.get(ctx, fmt.Sprintf("/drive/v3/files/%s?alt=media", f.ID))
	default:
		return "", false, nil // folders, sheets, slides, binaries — skipped for now
	}
	if err != nil {
		return "", false, err
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false, nil
	}
	return raw, true, nil
}

// listFiles fetches one page of the file listing (name/mime/modified/link).
func (c *Connector) listFiles(ctx context.Context, pageToken string) (*fileList, error) {
	params := url.Values{}
	params.Set("fields", "nextPageToken,files(id,name,mimeType,modifiedTime,webViewLink)")
	params.Set("pageSize", fmt.Sprintf("%d", pageSize))
	// Only files (not trashed); the token's scope already bounds visibility.
	params.Set("q", "trashed = false")
	if pageToken != "" {
		params.Set("pageToken", pageToken)
	}
	body, err := c.get(ctx, "/drive/v3/files?"+params.Encode())
	if err != nil {
		return nil, err
	}
	var out fileList
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		return nil, fmt.Errorf("decode drive file list: %w", err)
	}
	return &out, nil
}

// get performs an authenticated GET and returns the response body as a string.
func (c *Connector) get(ctx context.Context, path string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("drive request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<24))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("drive GET %s: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(data[:min(len(data), 1<<12)])))
	}
	return string(data), nil
}
