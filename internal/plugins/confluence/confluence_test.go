package confluence

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchPaginatesAndStripsXHTML(t *testing.T) {
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("me@x.com:tok"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != wantAuth {
			t.Errorf("auth = %q, want %q", got, wantAuth)
		}
		start := r.URL.Query().Get("start")
		w.Header().Set("Content-Type", "application/json")
		if start == "0" {
			body := map[string]any{
				"results": []any{
					page1("100", "Runbook", "<p>Restart the <strong>app</strong> pod.</p><p>Then check logs.</p>"),
					page1("101", "", ""), // empty → skipped
				},
				"size": 2,
			}
			body["_links"] = map[string]any{"next": "/rest/api/content?start=2"}
			_ = json.NewEncoder(w).Encode(body)
			return
		}
		if start != "2" {
			t.Errorf("expected start=2, got %q", start)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []any{page1("102", "Second page", "<p>more</p>")},
			"size":    1,
			"_links":  map[string]any{}, // no next → stop
		})
	}))
	defer srv.Close()

	c := New("https://site.atlassian.net/wiki", "me@x.com", "tok", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	byTitle := map[string]string{}
	for d, err := range c.Fetch(context.Background()) {
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		byTitle[d.Metadata["id"].(string)] = d.Text
	}

	if len(byTitle) != 2 {
		t.Fatalf("got %d docs, want 2: %v", len(byTitle), byTitle)
	}
	runbook := byTitle["100"]
	if !strings.Contains(runbook, "Runbook") || !strings.Contains(runbook, "Restart the app pod") {
		t.Errorf("XHTML not stripped to text: %q", runbook)
	}
	if strings.Contains(runbook, "<p>") || strings.Contains(runbook, "<strong>") {
		t.Errorf("markup leaked into text: %q", runbook)
	}
	if _, ok := byTitle["102"]; !ok {
		t.Errorf("pagination lost page 2: %v", byTitle)
	}
}

func TestFetchErrorSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()

	c := New("x", "e", "t", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	var gotErr error
	for _, err := range c.Fetch(context.Background()) {
		if err != nil {
			gotErr = err
			break
		}
	}
	if gotErr == nil || !strings.Contains(gotErr.Error(), "403") {
		t.Fatalf("expected 403 error, got %v", gotErr)
	}
}

// page1 builds a Confluence content result with a storage-format body.
func page1(id, title, xhtml string) map[string]any {
	return map[string]any{
		"id":      id,
		"title":   title,
		"body":    map[string]any{"storage": map[string]any{"value": xhtml}},
		"version": map[string]any{"when": "2026-07-01T00:00:00.000Z"},
		"_links":  map[string]any{"webui": "/spaces/X/pages/" + id},
	}
}
