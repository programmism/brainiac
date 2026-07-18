package gitlab

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/programmism/brainiac/internal/plugins"
)

func TestFetchIssuesAndMRs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("PRIVATE-TOKEN"); got != "glpat-x" {
			t.Errorf("auth = %q, want glpat-x", got)
		}
		// Project path must be URL-encoded into the :id slot.
		if !strings.Contains(r.URL.Path, "/api/v4/projects/group%2Fproj/") &&
			!strings.Contains(r.URL.EscapedPath(), "group%2Fproj") {
			t.Errorf("project not url-encoded: %s", r.URL.EscapedPath())
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/issues"):
			fmt.Fprint(w, `[{"iid":1,"title":"Use Kafka","description":"for durability","web_url":"https://gitlab.com/group/proj/-/issues/1","updated_at":"2026-07-01T00:00:00Z","author":{"username":"alice"}},
			                {"iid":2,"title":"","description":"","web_url":"","updated_at":"2026-07-02T00:00:00Z","author":{"username":"bob"}}]`)
		case strings.Contains(r.URL.Path, "/merge_requests"):
			fmt.Fprint(w, `[{"iid":7,"title":"Add index","description":"speeds recall","web_url":"https://gitlab.com/group/proj/-/merge_requests/7","updated_at":"2026-07-03T00:00:00Z","author":{"username":"carol"}}]`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := New("glpat-x", srv.URL, []string{"group/proj"}, WithHTTPClient(srv.Client()))
	byURI := map[string]plugins.RawDoc{}
	for d, err := range c.Fetch(context.Background()) {
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		byURI[d.SourceURI] = d
	}

	// Empty issue #2 skipped; issue #1 and MR #7 survive.
	if len(byURI) != 2 {
		t.Fatalf("got %d docs, want 2: %v", len(byURI), keys(byURI))
	}
	iss := byURI["https://gitlab.com/group/proj/-/issues/1"]
	if !strings.Contains(iss.Text, "Use Kafka") || !strings.Contains(iss.Text, "durability") {
		t.Errorf("issue text = %q", iss.Text)
	}
	if iss.Metadata["source"] != "gitlab" || iss.Metadata["kind"] != "issue" || iss.Metadata["author"] != "alice" {
		t.Errorf("issue metadata = %v", iss.Metadata)
	}
	mr := byURI["https://gitlab.com/group/proj/-/merge_requests/7"]
	if mr.Metadata["kind"] != "mr" || !strings.Contains(mr.Text, "Add index") {
		t.Errorf("mr doc = %+v", mr)
	}
}

func TestFetchErrorIsNonFatalPerProject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The first project 404s; the second returns one issue.
		if strings.Contains(r.URL.EscapedPath(), "missing%2Fproj") {
			http.Error(w, `{"message":"404 Project Not Found"}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/issues") {
			fmt.Fprint(w, `[{"iid":1,"title":"Kept","description":"","web_url":"https://gitlab.com/ok/proj/-/issues/1","updated_at":"2026-07-01T00:00:00Z","author":{"username":"a"}}]`)
			return
		}
		fmt.Fprint(w, `[]`)
	}))
	defer srv.Close()

	c := New("t", srv.URL, []string{"missing/proj", "ok/proj"}, WithHTTPClient(srv.Client()))
	var docs, errs int
	for d, err := range c.Fetch(context.Background()) {
		if err != nil {
			errs++
			continue
		}
		if d.SourceURI != "" {
			docs++
		}
	}
	if errs == 0 {
		t.Fatalf("expected an error from the missing project")
	}
	if docs != 1 {
		t.Fatalf("expected the good project's 1 doc despite the other's error, got %d", docs)
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
