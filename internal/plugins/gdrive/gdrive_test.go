package gdrive

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

func fakeDrive(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer ya29-test" {
			t.Errorf("missing/wrong auth: %q", got)
		}
		switch r.URL.Path {
		case "/drive/v3/files":
			// One page: a Google Doc, a text file, a folder (skipped), a PDF (skipped).
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{
				{"id": "doc1", "name": "Decision", "mimeType": googleDocMIME, "modifiedTime": "2026-07-01T00:00:00Z", "webViewLink": "https://docs.google.com/document/d/doc1"},
				{"id": "txt1", "name": "notes.txt", "mimeType": "text/plain", "modifiedTime": "2026-07-02T00:00:00Z", "webViewLink": "https://drive.google.com/file/d/txt1"},
				{"id": "fold", "name": "Folder", "mimeType": "application/vnd.google-apps.folder"},
				{"id": "pdf1", "name": "scan.pdf", "mimeType": "application/pdf"},
			}})
		case "/drive/v3/files/doc1/export":
			if r.URL.Query().Get("mimeType") != "text/plain" {
				t.Errorf("doc export not text/plain: %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte("Exported Google Doc body about Kafka."))
		case "/drive/v3/files/txt1":
			if r.URL.Query().Get("alt") != "media" {
				t.Errorf("text download not alt=media: %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte("plain text note"))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
}

func TestFetchExportsDocsAndDownloadsText(t *testing.T) {
	srv := fakeDrive(t)
	defer srv.Close()

	c := New("ya29-test", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	byURI := map[string]plugins.RawDoc{}
	for d, err := range c.Fetch(context.Background()) {
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		byURI[d.SourceURI] = d
	}

	// Only the Doc and the text file survive (folder + PDF skipped).
	if len(byURI) != 2 {
		t.Fatalf("got %d docs, want 2 (folder+pdf skipped): %v", len(byURI), keys(byURI))
	}
	gdoc := byURI["https://docs.google.com/document/d/doc1"]
	if !strings.Contains(gdoc.Text, "Kafka") || gdoc.Metadata["source"] != "gdrive" {
		t.Errorf("doc1 = %+v", gdoc)
	}
	if gdoc.ModifiedAt == nil {
		t.Error("doc1 missing ModifiedAt")
	}
	if txt := byURI["https://drive.google.com/file/d/txt1"]; txt.Text != "plain text note" {
		t.Errorf("txt1 text = %q", txt.Text)
	}
}

func TestFetchErrorSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"invalid_token"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := New("ya29-test", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
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
