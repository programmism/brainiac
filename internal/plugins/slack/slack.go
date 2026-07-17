// Package slack implements the plugins.SourceConnector seam against the Slack
// Web API (#237). It enumerates the channels the bot token can see, reads each
// channel's message history, and yields one RawDoc per substantive message so the
// ingest pipeline can chunk, select, embed, and store it — with a portable
// `slack://<channel>/<ts>` provenance URI and the message timestamp as the
// modification time.
//
// Auth is a Slack bot token (xoxb-…) with, typically, channels:read +
// channels:history scopes; pass it via SLACK_TOKEN. The connector is read-only.
package slack

import (
	"context"
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

const defaultBaseURL = "https://slack.com/api"

// pageLimit is the per-request page size for list/history calls (Slack caps
// history around 1000; 200 is a friendly default that paginates via cursors).
const pageLimit = 200

// Connector reads messages from the Slack channels a bot token can access. If
// channels is non-empty, only those channel IDs are read (skipping enumeration).
type Connector struct {
	token    string
	baseURL  string
	client   *http.Client
	channels []string
}

// Option customizes a Connector.
type Option func(*Connector)

// WithBaseURL overrides the API base URL (used in tests).
func WithBaseURL(u string) Option {
	return func(c *Connector) { c.baseURL = strings.TrimRight(u, "/") }
}

// WithHTTPClient overrides the HTTP client.
func WithHTTPClient(h *http.Client) Option { return func(c *Connector) { c.client = h } }

// New builds a Slack connector for the given bot token.
func New(token string, opts ...Option) *Connector {
	c := &Connector{token: token, baseURL: defaultBaseURL, client: &http.Client{Timeout: 30 * time.Second}}
	for _, o := range opts {
		o(c)
	}
	return c
}

// NewForChannels builds a connector that reads only the given channel IDs, for a
// targeted import.
func NewForChannels(token string, channels []string, opts ...Option) *Connector {
	c := New(token, opts...)
	c.channels = append(c.channels, channels...)
	return c
}

var _ plugins.SourceConnector = (*Connector)(nil)

type channel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type message struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	User    string `json:"user"`
	Text    string `json:"text"`
	TS      string `json:"ts"`
}

type responseMeta struct {
	NextCursor string `json:"next_cursor"`
}

// Fetch yields one RawDoc per substantive message across the readable channels.
func (c *Connector) Fetch(ctx context.Context) iter.Seq2[plugins.RawDoc, error] {
	return func(yield func(plugins.RawDoc, error) bool) {
		chans, err := c.listChannels(ctx)
		if err != nil {
			yield(plugins.RawDoc{}, err)
			return
		}
		for _, ch := range chans {
			if ctx.Err() != nil {
				return
			}
			cursor := ""
			for {
				msgs, next, err := c.history(ctx, ch.ID, cursor)
				if err != nil {
					if !yield(plugins.RawDoc{}, err) {
						return
					}
					break // move to the next channel on error
				}
				for _, m := range msgs {
					doc, ok := messageDoc(ch, m)
					if !ok {
						continue
					}
					if !yield(doc, nil) {
						return
					}
				}
				if next == "" {
					break
				}
				cursor = next
			}
		}
	}
}

// Watch yields an upsert per message so ingest's content-hash step skips
// unchanged chunks. A persisted per-channel cursor for true incremental sync is a
// later refinement (#323).
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

// messageDoc converts a Slack message to a RawDoc, or ok=false to skip it (empty
// text, or a system message such as a join/leave notice).
func messageDoc(ch channel, m message) (plugins.RawDoc, bool) {
	text := strings.TrimSpace(m.Text)
	if text == "" || m.Subtype != "" {
		return plugins.RawDoc{}, false // system/edited-placeholder/join messages carry no memory value
	}
	uri := "slack://" + ch.ID + "/" + m.TS
	return plugins.RawDoc{
		Text:          text,
		SourceURI:     uri,
		SourceLocator: map[string]any{"channel": ch.ID, "ts": m.TS},
		Metadata:      map[string]any{"source": "slack", "channel_name": ch.Name, "user": m.User},
		ModifiedAt:    tsToTime(m.TS),
	}, true
}

// tsToTime parses a Slack "seconds.micros" timestamp into a time, or nil.
func tsToTime(ts string) *time.Time {
	f, err := strconv.ParseFloat(ts, 64)
	if err != nil {
		return nil
	}
	sec := int64(f)
	nsec := int64((f - float64(sec)) * 1e9)
	t := time.Unix(sec, nsec).UTC()
	return &t
}

// --- Slack API ---

// listChannels returns the channels to read: the explicit set if configured, else
// every public channel the token can see (paginated).
func (c *Connector) listChannels(ctx context.Context) ([]channel, error) {
	if len(c.channels) > 0 {
		out := make([]channel, len(c.channels))
		for i, id := range c.channels {
			out[i] = channel{ID: id}
		}
		return out, nil
	}
	var all []channel
	cursor := ""
	for {
		params := url.Values{}
		params.Set("types", "public_channel")
		params.Set("limit", strconv.Itoa(pageLimit))
		if cursor != "" {
			params.Set("cursor", cursor)
		}
		var resp struct {
			apiStatus
			Channels []channel    `json:"channels"`
			Meta     responseMeta `json:"response_metadata"`
		}
		if err := c.get(ctx, "conversations.list", params, &resp); err != nil {
			return nil, err
		}
		all = append(all, resp.Channels...)
		if resp.Meta.NextCursor == "" {
			return all, nil
		}
		cursor = resp.Meta.NextCursor
	}
}

// history returns one page of a channel's messages and the next cursor ("" when
// there are no more).
func (c *Connector) history(ctx context.Context, channelID, cursor string) ([]message, string, error) {
	params := url.Values{}
	params.Set("channel", channelID)
	params.Set("limit", strconv.Itoa(pageLimit))
	if cursor != "" {
		params.Set("cursor", cursor)
	}
	var resp struct {
		apiStatus
		Messages []message    `json:"messages"`
		Meta     responseMeta `json:"response_metadata"`
	}
	if err := c.get(ctx, "conversations.history", params, &resp); err != nil {
		return nil, "", err
	}
	return resp.Messages, resp.Meta.NextCursor, nil
}

// apiStatus is the {ok, error} envelope every Slack response carries.
type apiStatus struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

// get performs a Slack Web API GET, honoring 429 rate limiting, and decodes the
// JSON body into out. A transport 200 with {"ok":false} is surfaced as an error.
func (c *Connector) get(ctx context.Context, endpoint string, params url.Values, out any) error {
	backoff := 500 * time.Millisecond
	for attempt := 0; attempt < 4; attempt++ {
		u := c.baseURL + "/" + endpoint
		if enc := params.Encode(); enc != "" {
			u += "?" + enc
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)

		resp, err := c.client.Do(req)
		if err != nil {
			return err
		}
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
			return fmt.Errorf("slack %s: status %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(msg)))
		}
		// Decode into the caller's struct AND check the ok/error envelope.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<24))
		_ = resp.Body.Close()
		var status apiStatus
		if err := json.Unmarshal(body, &status); err != nil {
			return fmt.Errorf("slack %s: decode: %w", endpoint, err)
		}
		if !status.OK {
			return fmt.Errorf("slack %s: %s", endpoint, statusError(status.Error))
		}
		return json.Unmarshal(body, out)
	}
	return fmt.Errorf("slack %s: rate-limited after retries", endpoint)
}

func statusError(e string) string {
	if e == "" {
		return "not ok"
	}
	return e
}

// retryAfter parses a Retry-After header (seconds), falling back to def.
func retryAfter(h string, def time.Duration) time.Duration {
	if h == "" {
		return def
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(h)); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return def
}
