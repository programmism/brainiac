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

// fakeGitHub serves one repo's issues across two pages, driving pagination via the
// Link: rel="next" header (#381): page 1 advertises page 2, page 2 has no next.
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
		// The first request carries no page param; a full page then advertises the
		// next via Link (a final page exactly per_page long must NOT stop early).
		if r.URL.Query().Get("page") == "" {
			next := "http://" + r.Host + "/repos/octo/mem/issues?state=all&per_page=100&page=2"
			w.Header().Set("Link", "<"+next+`>; rel="next", <`+next+`>; rel="last"`)
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"number": 1, "title": "Use Kafka", "body": "for durability", "html_url": "https://github.com/octo/mem/issues/1", "updated_at": "2026-07-01T00:00:00Z", "user": map[string]any{"login": "alice"}},
				{"number": 2, "title": "Add retries", "body": "idempotency keys", "html_url": "https://github.com/octo/mem/pull/2", "updated_at": "2026-07-02T00:00:00Z", "user": map[string]any{"login": "bob"}, "pull_request": map[string]any{"url": "x"}},
				{"number": 3, "title": "", "body": "", "html_url": "https://github.com/octo/mem/issues/3", "updated_at": "2026-07-03T00:00:00Z"},
			})
			return
		}
		// Page 2: no Link header → connector stops after it.
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

func TestNextLink(t *testing.T) {
	cases := map[string]string{
		"":                                   "",
		`<https://api/x?page=2>; rel="next"`: "https://api/x?page=2",
		`<https://api/x?page=9>; rel="last"`: "",
		`<https://a?p=2>; rel="next", <https://a?p=9>; rel="last"`: "https://a?p=2",
		`<https://a?p=1>; rel="prev", <https://a?p=3>; rel="next"`: "https://a?p=3",
	}
	for header, want := range cases {
		if got := nextLink(header); got != want {
			t.Errorf("nextLink(%q) = %q, want %q", header, got, want)
		}
	}
}

// TestFetchDiscussions drives the opt-in GraphQL Discussions path (#381) across two
// cursor pages against a fake GraphQL endpoint.
func TestFetchDiscussions(t *testing.T) {
	var gotQueryVars []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/octo/mem/issues" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]map[string]any{}) // no issues
			return
		}
		if r.URL.Path != "/graphql" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			return
		}
		var body struct {
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotQueryVars = append(gotQueryVars, body.Variables)
		w.Header().Set("Content-Type", "application/json")
		// First call (no cursor) → one node + hasNextPage; second → last node.
		if _, ok := body.Variables["after"]; !ok {
			_, _ = w.Write([]byte(`{"data":{"repository":{"discussions":{"pageInfo":{"hasNextPage":true,"endCursor":"CUR1"},"nodes":[{"number":10,"title":"RFC: caching","body":"proposal","url":"https://github.com/octo/mem/discussions/10","updatedAt":"2026-07-01T00:00:00Z","author":{"login":"carol"}}]}}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"repository":{"discussions":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[{"number":11,"title":"Q: sharding","body":"how?","url":"https://github.com/octo/mem/discussions/11","updatedAt":"2026-07-02T00:00:00Z","author":{"login":"dave"}}]}}}}`))
	}))
	defer srv.Close()

	c := New("ghp-test", []string{"octo/mem"}, WithBaseURL(srv.URL), WithHTTPClient(srv.Client()), WithDiscussions())
	byURI := map[string]plugins.RawDoc{}
	for d, err := range c.Fetch(context.Background()) {
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		byURI[d.SourceURI] = d
	}

	d10 := byURI["https://github.com/octo/mem/discussions/10"]
	if !strings.Contains(d10.Text, "RFC: caching") || !strings.Contains(d10.Text, "proposal") {
		t.Errorf("discussion 10 text = %q", d10.Text)
	}
	if d10.Metadata["kind"] != "discussion" || d10.Metadata["author"] != "carol" {
		t.Errorf("discussion 10 metadata = %v", d10.Metadata)
	}
	if _, ok := byURI["https://github.com/octo/mem/discussions/11"]; !ok {
		t.Errorf("cursor pagination lost page 2: %v", keys(byURI))
	}
	if len(gotQueryVars) != 2 {
		t.Fatalf("expected 2 graphql calls (cursor paging), got %d", len(gotQueryVars))
	}
	if gotQueryVars[1]["after"] != "CUR1" {
		t.Errorf("second page cursor = %v, want CUR1", gotQueryVars[1]["after"])
	}
}

// TestDiscussionsOffByDefault confirms Discussions are not fetched unless opted in.
func TestDiscussionsOffByDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/graphql" {
			t.Error("GraphQL called without WithDiscussions")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	}))
	defer srv.Close()

	c := New("ghp-test", []string{"octo/mem"}, WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	for _, err := range c.Fetch(context.Background()) {
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
	}
}
