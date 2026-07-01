package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

func TestWebUIMounted(t *testing.T) {
	h := New(fakePinger{}, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}
	if !bytesContains(rec.Body.Bytes(), "Brainiac") {
		t.Fatal("root did not serve the WebUI")
	}
}

func bytesContains(b []byte, s string) bool {
	return len(b) > 0 && strings.Contains(string(b), s)
}

func do(t *testing.T, h http.Handler, path string) (int, map[string]string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

func TestHealthz(t *testing.T) {
	h := New(fakePinger{}, nil, nil)
	code, body := do(t, h, "/healthz")
	if code != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("healthz = %d %v", code, body)
	}
}

func TestReadyzDBOK_EmbedderNotConfigured(t *testing.T) {
	h := New(fakePinger{}, nil, nil)
	code, body := do(t, h, "/readyz")
	if code != http.StatusOK {
		t.Fatalf("readyz code = %d", code)
	}
	if body["db"] != "ok" || body["embedder"] != "not-configured" {
		t.Fatalf("readyz body = %v", body)
	}
}

func TestReadyzEmbedderUnreachableStillReady(t *testing.T) {
	down := func(context.Context) error { return errors.New("no ollama") }
	h := New(fakePinger{}, down, nil)
	code, body := do(t, h, "/readyz")
	if code != http.StatusOK { // graceful degradation: embedder down != not ready
		t.Fatalf("readyz code = %d, want 200", code)
	}
	if body["embedder"] != "unreachable" {
		t.Fatalf("embedder = %q", body["embedder"])
	}
}

func TestReadyzDBDownIs503(t *testing.T) {
	h := New(fakePinger{err: errors.New("db gone")}, nil, nil)
	code, body := do(t, h, "/readyz")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("readyz code = %d, want 503", code)
	}
	if body["db"] != "error" {
		t.Fatalf("db = %q", body["db"])
	}
}
