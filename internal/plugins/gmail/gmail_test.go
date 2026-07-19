package gmail

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/programmism/brainiac/internal/plugins"
)

func b64url(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

// fakeGmail serves two pages of message ids and three messages: one multipart with
// a text/plain part, one with only a snippet (empty body), one simple text/plain.
func fakeGmail(t *testing.T) *httptest.Server {
	t.Helper()
	msgs := map[string]map[string]any{
		"m1": {
			"id": "m1", "internalDate": "1720000000000",
			"payload": map[string]any{
				"mimeType": "multipart/alternative",
				"headers":  []map[string]any{{"name": "Subject", "value": "Use Kafka"}},
				"parts": []map[string]any{
					{"mimeType": "text/html", "body": map[string]any{"data": b64url("<p>ignored html</p>")}},
					{"mimeType": "text/plain", "body": map[string]any{"data": b64url("we chose Kafka for durability")}},
				},
			},
		},
		"m2": {
			"id": "m2", "internalDate": "1720000100000", "snippet": "short preview only",
			"payload": map[string]any{"mimeType": "text/plain", "headers": []map[string]any{{"name": "Subject", "value": "FYI"}}},
		},
		"m3": {
			"id": "m3", "internalDate": "1720000200000",
			"payload": map[string]any{
				"mimeType": "text/plain",
				"headers":  []map[string]any{{"name": "Subject", "value": "Retry policy"}},
				"body":     map[string]any{"data": b64url("exponential backoff, five attempts")},
			},
		},
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("missing/wrong auth: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/gmail/v1/users/me/messages":
			if r.URL.Query().Get("pageToken") == "" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"messages":      []map[string]any{{"id": "m1"}, {"id": "m2"}},
					"nextPageToken": "pg2",
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"messages": []map[string]any{{"id": "m3"}}})
		case strings.HasPrefix(r.URL.Path, "/gmail/v1/users/me/messages/"):
			id := strings.TrimPrefix(r.URL.Path, "/gmail/v1/users/me/messages/")
			m, ok := msgs[id]
			if !ok {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(m)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
}

func TestFetchMessages(t *testing.T) {
	srv := fakeGmail(t)
	defer srv.Close()

	c := New("tok", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	byURI := map[string]plugins.RawDoc{}
	for d, err := range c.Fetch(context.Background()) {
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		byURI[d.SourceURI] = d
	}

	if len(byURI) != 3 {
		t.Fatalf("got %d messages, want 3 (pagination): %v", len(byURI), keys(byURI))
	}
	m1 := byURI["gmail://m1"]
	if !strings.Contains(m1.Text, "Use Kafka") || !strings.Contains(m1.Text, "durability") {
		t.Errorf("m1 text = %q (want subject + text/plain body, not html)", m1.Text)
	}
	if strings.Contains(m1.Text, "ignored html") {
		t.Errorf("m1 leaked the html part: %q", m1.Text)
	}
	if m1.Metadata["source"] != "gmail" || m1.ModifiedAt == nil {
		t.Errorf("m1 metadata/date: %v / %v", m1.Metadata, m1.ModifiedAt)
	}
	// m2 has no body part → falls back to the snippet.
	if m2 := byURI["gmail://m2"]; !strings.Contains(m2.Text, "short preview only") {
		t.Errorf("m2 should fall back to snippet: %q", m2.Text)
	}
	// m3 is a simple (non-multipart) text/plain message.
	if m3 := byURI["gmail://m3"]; !strings.Contains(m3.Text, "exponential backoff") {
		t.Errorf("m3 body not extracted: %q", m3.Text)
	}
}

func TestFetchErrorSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"invalid_token"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := New("tok", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	var gotErr error
	for _, err := range c.Fetch(context.Background()) {
		if err != nil {
			gotErr = err
			break
		}
	}
	if gotErr == nil || !strings.Contains(gotErr.Error(), "401") {
		t.Fatalf("expected 401, got %v", gotErr)
	}
}

func keys(m map[string]plugins.RawDoc) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
