package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/programmism/brainiac/internal/plugins"
)

// fakeSlack serves canned conversations.list / conversations.history responses,
// with one page of cursor pagination on history.
func fakeSlack(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer xoxb-test" {
			t.Errorf("missing/wrong auth header: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/conversations.list"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":       true,
				"channels": []map[string]any{{"id": "C1", "name": "general"}},
			})
		case strings.HasSuffix(r.URL.Path, "/conversations.history"):
			cursor := r.URL.Query().Get("cursor")
			if cursor == "" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"ok": true,
					"messages": []map[string]any{
						{"type": "message", "user": "U1", "text": "first message", "ts": "1700000000.000100"},
						{"type": "message", "subtype": "channel_join", "user": "U2", "text": "joined", "ts": "1700000001.000000"},
						{"type": "message", "user": "U3", "text": "  ", "ts": "1700000002.000000"}, // blank
					},
					"response_metadata": map[string]any{"next_cursor": "page2"},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"messages": []map[string]any{
					{"type": "message", "user": "U4", "text": "second page message", "ts": "1700000100.000000"},
				},
			})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
}

func TestFetchYieldsMessages(t *testing.T) {
	srv := fakeSlack(t)
	defer srv.Close()

	c := New("xoxb-test", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	var docs []plugins.RawDoc
	for d, err := range c.Fetch(context.Background()) {
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		docs = append(docs, d)
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].SourceURI < docs[j].SourceURI })

	// Only the two real messages survive (join + blank are skipped), across 2 pages.
	if len(docs) != 2 {
		t.Fatalf("got %d docs, want 2: %+v", len(docs), docs)
	}
	if docs[0].SourceURI != "slack://C1/1700000000.000100" || docs[0].Text != "first message" {
		t.Errorf("doc0 = %+v", docs[0])
	}
	if docs[0].Metadata["channel_name"] != "general" || docs[0].Metadata["source"] != "slack" {
		t.Errorf("doc0 metadata = %v", docs[0].Metadata)
	}
	if docs[0].ModifiedAt == nil || docs[0].ModifiedAt.Unix() != 1700000000 {
		t.Errorf("doc0 modifiedAt = %v", docs[0].ModifiedAt)
	}
	if docs[1].Text != "second page message" {
		t.Errorf("pagination lost the second page: %+v", docs[1])
	}
}

func TestFetchForSpecificChannelsSkipsList(t *testing.T) {
	var listCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/conversations.list") {
			listCalled = true
		}
		if got := r.URL.Query().Get("channel"); got != "" && got != "C9" {
			t.Errorf("history for unexpected channel %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":       true,
			"messages": []map[string]any{{"type": "message", "user": "U", "text": "hi", "ts": "1700000000.000000"}},
		})
	}))
	defer srv.Close()

	c := NewForChannels("xoxb-test", []string{"C9"}, WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	n := 0
	for _, err := range c.Fetch(context.Background()) {
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		n++
	}
	if listCalled {
		t.Error("explicit channel set must not call conversations.list")
	}
	if n != 1 {
		t.Fatalf("got %d docs, want 1", n)
	}
}

func TestAPIErrorSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "invalid_auth"})
	}))
	defer srv.Close()

	c := New("xoxb-test", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	var gotErr error
	for _, err := range c.Fetch(context.Background()) {
		if err != nil {
			gotErr = err
			break
		}
	}
	if gotErr == nil || !strings.Contains(gotErr.Error(), "invalid_auth") {
		t.Fatalf("expected invalid_auth error, got %v", gotErr)
	}
}

func TestTSToTime(t *testing.T) {
	got := tsToTime("1700000000.000100")
	if got == nil || got.Unix() != 1700000000 {
		t.Fatalf("tsToTime = %v", got)
	}
	if tsToTime("not-a-ts") != nil {
		t.Error("bad ts should yield nil")
	}
}
