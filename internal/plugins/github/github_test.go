package github

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

// fakeGitHub serves one repo's issues across two pages (page 2 is short → stop).
func fakeGitHub(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer ghp-test" {
			t.Errorf("missing/wrong auth: %q", got)
		}
		if r.URL.Path != "/repos/octo/mem/issues" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "1" {
			// A full first page: mix of an issue, a PR, and an empty one (skipped).
			page := make([]map[string]any, 0, perPage)
			page = append(page,
				map[string]any{"number": 1, "title": "Use Kafka", "body": "for durability", "html_url": "https://github.com/octo/mem/issues/1", "updated_at": "2026-07-01T00:00:00Z", "user": map[string]any{"login": "alice"}},
				map[string]any{"number": 2, "title": "Add retries", "body": "idempotency keys", "html_url": "https://github.com/octo/mem/pull/2", "updated_at": "2026-07-02T00:00:00Z", "user": map[string]any{"login": "bob"}, "pull_request": map[string]any{"url": "x"}},
				map[string]any{"number": 3, "title": "", "body": "", "html_url": "https://github.com/octo/mem/issues/3", "updated_at": "2026-07-03T00:00:00Z"},
			)
			// Pad to perPage so the connector requests page 2.
			for i := len(page); i < perPage; i++ {
				page = append(page, map[string]any{"number": 100 + i, "title": "pad", "html_url": "u", "updated_at": "2026-07-01T00:00:00Z"})
			}
			_ = json.NewEncoder(w).Encode(page)
			return
		}
		// Page 2: one item → short page → connector stops.
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"number": 200, "title": "Second page", "html_url": "https://github.com/octo/mem/issues/200", "updated_at": "2026-07-05T00:00:00Z"},
		})
	}))
}

func TestFetchIssuesAndPRs(t *testing.T) {
	srv := fakeGitHub(t)
	defer srv.Close()

	c := New("ghp-test", []string{"octo/mem"}, WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	byURI := map[string]plugins.RawDoc{}
	for d, err := range c.Fetch(context.Background()) {
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		byURI[d.SourceURI] = d
	}

	issue1 := byURI["https://github.com/octo/mem/issues/1"]
	if !strings.Contains(issue1.Text, "Use Kafka") || !strings.Contains(issue1.Text, "durability") {
		t.Errorf("issue1 text = %q", issue1.Text)
	}
	if issue1.Metadata["kind"] != "issue" || issue1.Metadata["source"] != "github" {
		t.Errorf("issue1 metadata = %v", issue1.Metadata)
	}
	if issue1.ModifiedAt == nil {
		t.Error("issue1 missing ModifiedAt")
	}
	if pr := byURI["https://github.com/octo/mem/pull/2"]; pr.Metadata["kind"] != "pr" {
		t.Errorf("PR not tagged kind=pr: %v", pr.Metadata)
	}
	// Empty issue #3 is skipped; the second-page item is fetched (pagination).
	if _, ok := byURI["https://github.com/octo/mem/issues/3"]; ok {
		t.Error("empty issue should be skipped")
	}
	if _, ok := byURI["https://github.com/octo/mem/issues/200"]; !ok {
		t.Errorf("pagination lost page 2: %v", keys(byURI))
	}
}

func TestFetchErrorSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := New("ghp-test", []string{"octo/mem"}, WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	var gotErr error
	for _, err := range c.Fetch(context.Background()) {
		if err != nil {
			gotErr = err
			break
		}
	}
	if gotErr == nil || !strings.Contains(gotErr.Error(), "401") {
		t.Fatalf("expected 401 error, got %v", gotErr)
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
