package linear

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

func TestFetchPaginatesIssues(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "lin_api_test" {
			t.Errorf("missing/wrong auth: %q", got)
		}
		var req struct {
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		if req.Variables["after"] == nil {
			// Page 1 → hasNextPage, cursor "c1".
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"issues": map[string]any{
				"nodes": []map[string]any{
					{"identifier": "ENG-1", "title": "Use Kafka", "description": "for durability", "updatedAt": "2026-07-01T00:00:00Z", "url": "https://linear.app/x/issue/ENG-1"},
					{"identifier": "ENG-2", "title": "", "description": "", "updatedAt": "2026-07-02T00:00:00Z", "url": "https://linear.app/x/issue/ENG-2"},
				},
				"pageInfo": map[string]any{"hasNextPage": true, "endCursor": "c1"},
			}}})
			return
		}
		if req.Variables["after"] != "c1" {
			t.Errorf("expected cursor c1, got %v", req.Variables["after"])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"issues": map[string]any{
			"nodes":    []map[string]any{{"identifier": "ENG-3", "title": "Second page", "updatedAt": "2026-07-03T00:00:00Z", "url": "https://linear.app/x/issue/ENG-3"}},
			"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
		}}})
	}))
	defer srv.Close()

	c := New("lin_api_test", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	byURI := map[string]plugins.RawDoc{}
	for d, err := range c.Fetch(context.Background()) {
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		byURI[d.SourceURI] = d
	}

	// ENG-2 (empty) skipped; ENG-1 and the page-2 ENG-3 survive.
	if len(byURI) != 2 {
		t.Fatalf("got %d docs, want 2: %v", len(byURI), keys(byURI))
	}
	one := byURI["https://linear.app/x/issue/ENG-1"]
	if !strings.Contains(one.Text, "Use Kafka") || !strings.Contains(one.Text, "durability") {
		t.Errorf("ENG-1 text = %q", one.Text)
	}
	if one.Metadata["source"] != "linear" || one.Metadata["identifier"] != "ENG-1" {
		t.Errorf("ENG-1 metadata = %v", one.Metadata)
	}
	if _, ok := byURI["https://linear.app/x/issue/ENG-3"]; !ok {
		t.Errorf("pagination lost page 2: %v", keys(byURI))
	}
}

func TestFetchGraphQLErrorSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]any{{"message": "authentication failed"}}})
	}))
	defer srv.Close()

	c := New("lin_api_test", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	var gotErr error
	for _, err := range c.Fetch(context.Background()) {
		if err != nil {
			gotErr = err
			break
		}
	}
	if gotErr == nil || !strings.Contains(gotErr.Error(), "authentication failed") {
		t.Fatalf("expected graphql error, got %v", gotErr)
	}
}

func keys(m map[string]plugins.RawDoc) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
