package notion

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/programmism/brainiac/internal/plugins"
)

func page(id, title string) map[string]any {
	return map[string]any{
		"id": id, "url": "https://notion.so/" + id, "last_edited_time": "2026-01-01T00:00:00Z",
		"properties": map[string]any{
			"Name": map[string]any{"type": "title", "title": []any{map[string]any{"plain_text": title}}},
		},
	}
}

func blocks(text string) map[string]any {
	return map[string]any{
		"results": []any{
			map[string]any{"type": "paragraph", "has_children": false,
				"paragraph": map[string]any{"rich_text": []any{map[string]any{"plain_text": text}}}},
		},
		"has_more": false, "next_cursor": nil,
	}
}

func mockNotion(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/search":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["start_cursor"] == nil {
				cur := "c2"
				_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{page("p1", "OrderService")}, "has_more": true, "next_cursor": cur})
			} else {
				_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{page("p2", "Postgres")}, "has_more": false, "next_cursor": nil})
			}
		case "/v1/blocks/p1/children":
			_ = json.NewEncoder(w).Encode(blocks("writes orders for durability"))
		case "/v1/blocks/p2/children":
			_ = json.NewEncoder(w).Encode(blocks("stores the data"))
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestFetchPaginatesAndFlattens(t *testing.T) {
	srv := mockNotion(t)
	defer srv.Close()

	c := New("tok", WithBaseURL(srv.URL))
	var docs []plugins.RawDoc
	for d, err := range c.Fetch(context.Background()) {
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		docs = append(docs, d)
	}

	if len(docs) != 2 {
		t.Fatalf("got %d docs, want 2 (pagination)", len(docs))
	}
	if docs[0].Text != "OrderService\n\nwrites orders for durability" {
		t.Errorf("doc0 text = %q", docs[0].Text)
	}
	if docs[0].SourceURI != "https://notion.so/p1" {
		t.Errorf("doc0 uri = %q", docs[0].SourceURI)
	}
	if docs[0].SourceLocator["page_id"] != "p1" {
		t.Errorf("doc0 locator = %v", docs[0].SourceLocator)
	}
	if docs[1].Text != "Postgres\n\nstores the data" {
		t.Errorf("doc1 text = %q", docs[1].Text)
	}
}

func TestFetchRetriesOn429(t *testing.T) {
	var searchCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/search":
			searchCalls++
			if searchCalls == 1 {
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{page("p1", "OrderService")}, "has_more": false, "next_cursor": nil})
		case "/v1/blocks/p1/children":
			_ = json.NewEncoder(w).Encode(blocks("writes orders"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := New("tok", WithBaseURL(srv.URL))
	var n int
	for _, err := range c.Fetch(context.Background()) {
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		n++
	}
	if n != 1 || searchCalls < 2 {
		t.Fatalf("expected 1 doc after a 429 retry (searchCalls=%d, docs=%d)", searchCalls, n)
	}
}

func TestWatchEmitsUpserts(t *testing.T) {
	srv := mockNotion(t)
	defer srv.Close()

	c := New("tok", WithBaseURL(srv.URL))
	var changes []plugins.Change
	for ch, err := range c.Watch(context.Background()) {
		if err != nil {
			t.Fatalf("watch: %v", err)
		}
		changes = append(changes, ch)
	}
	if len(changes) != 2 || changes[0].Kind != plugins.ChangeUpserted {
		t.Fatalf("changes = %+v", changes)
	}
}
