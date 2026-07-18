package jira

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// adfDoc builds a minimal ADF description with one paragraph.
func adfDoc(text string) map[string]any {
	return map[string]any{
		"type": "doc", "version": 1,
		"content": []any{map[string]any{
			"type":    "paragraph",
			"content": []any{map[string]any{"type": "text", "text": text}},
		}},
	}
}

func TestFetchPaginatesAndParsesADF(t *testing.T) {
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("me@x.com:tok"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != wantAuth {
			t.Errorf("auth = %q, want %q", got, wantAuth)
		}
		startAt := r.URL.Query().Get("startAt")
		w.Header().Set("Content-Type", "application/json")
		if startAt == "0" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"startAt": 0, "maxResults": 50, "total": 3,
				"issues": []any{
					map[string]any{"key": "ENG-1", "fields": map[string]any{
						"summary": "Use Kafka", "description": adfDoc("for durability"), "updated": "2026-07-01T00:00:00.000-0700"}},
					map[string]any{"key": "ENG-2", "fields": map[string]any{
						"summary": "", "description": nil, "updated": "2026-07-02T00:00:00.000-0700"}},
				},
			})
			return
		}
		if startAt != "2" {
			t.Errorf("expected startAt=2, got %q", startAt)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"startAt": 2, "maxResults": 50, "total": 3,
			"issues": []any{
				map[string]any{"key": "ENG-3", "fields": map[string]any{
					"summary": "Second page", "description": "plain string desc", "updated": "2026-07-03T00:00:00.000-0700"}},
			},
		})
	}))
	defer srv.Close()

	c := New("https://site.atlassian.net", "me@x.com", "tok", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	byURI := map[string]string{}
	for d, err := range c.Fetch(context.Background()) {
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		byURI[d.SourceURI] = d.Text
	}

	// ENG-2 (empty) skipped; ENG-1 (ADF) + ENG-3 (string, page 2) survive.
	if len(byURI) != 2 {
		t.Fatalf("got %d docs, want 2: %v", len(byURI), byURI)
	}
	one := byURI[srv.URL+"/browse/ENG-1"]
	if !strings.Contains(one, "Use Kafka") || !strings.Contains(one, "for durability") {
		t.Errorf("ENG-1 text = %q (ADF not rendered?)", one)
	}
	three := byURI[srv.URL+"/browse/ENG-3"]
	if !strings.Contains(three, "plain string desc") {
		t.Errorf("ENG-3 (string description) = %q", three)
	}
}

func TestFetchErrorSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"errorMessages":["Unauthorized"]}`, http.StatusUnauthorized)
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
	if gotErr == nil || !strings.Contains(gotErr.Error(), "401") {
		t.Fatalf("expected 401 error, got %v", gotErr)
	}
}

func TestADFToText(t *testing.T) {
	adf := map[string]any{
		"type": "doc",
		"content": []any{
			map[string]any{"type": "heading", "content": []any{map[string]any{"type": "text", "text": "Decision"}}},
			map[string]any{"type": "paragraph", "content": []any{
				map[string]any{"type": "text", "text": "We chose "},
				map[string]any{"type": "text", "text": "Postgres"},
			}},
			map[string]any{"type": "bulletList", "content": []any{
				map[string]any{"type": "listItem", "content": []any{
					map[string]any{"type": "paragraph", "content": []any{map[string]any{"type": "text", "text": "one join"}}}}},
				map[string]any{"type": "listItem", "content": []any{
					map[string]any{"type": "paragraph", "content": []any{map[string]any{"type": "text", "text": "one backup"}}}}},
			}},
		},
	}
	got := adfToText(adf)
	for _, want := range []string{"Decision", "We chose Postgres", "one join", "one backup"} {
		if !strings.Contains(got, want) {
			t.Errorf("adfToText missing %q in:\n%s", want, got)
		}
	}
	// No run of 3+ newlines survives the collapse.
	if strings.Contains(got, "\n\n\n") {
		t.Errorf("blank lines not collapsed:\n%q", got)
	}
	// An unknown node type still yields its text (lenient walk).
	weird := map[string]any{"type": "doc", "content": []any{
		map[string]any{"type": "someFutureNode", "content": []any{map[string]any{"type": "text", "text": "kept"}}}}}
	if !strings.Contains(adfToText(weird), "kept") {
		t.Errorf("unknown node dropped its text: %q", adfToText(weird))
	}
}
