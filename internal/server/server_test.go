package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/programmism/brainiac/internal/core"
	"github.com/programmism/brainiac/internal/logbuf"
)

type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

func TestMetricsAndVersion(t *testing.T) {
	h := New(fakePinger{}, nil, nil, Options{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "brainiac_http_request_duration_seconds_count") {
		t.Fatalf("/metrics = %d, body=%q", rec.Code, rec.Body.String())
	}

	code, body := do(t, h, "/healthz")
	if code != http.StatusOK || body["version"] == "" {
		t.Fatalf("healthz version missing: %d %v", code, body)
	}
}

func TestWebUIMounted(t *testing.T) {
	h := New(fakePinger{}, nil, nil, Options{})
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
	h := New(fakePinger{}, nil, nil, Options{})
	code, body := do(t, h, "/healthz")
	if code != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("healthz = %d %v", code, body)
	}
}

func TestReadyzDBOK_EmbedderNotConfigured(t *testing.T) {
	h := New(fakePinger{}, nil, nil, Options{})
	code, body := do(t, h, "/readyz")
	if code != http.StatusOK {
		t.Fatalf("readyz code = %d", code)
	}
	if body["db"] != "ok" || body["embedder"] != "not-configured" {
		t.Fatalf("readyz body = %v", body)
	}
}

func TestLogsEndpoint(t *testing.T) {
	buf := logbuf.New(10)
	_, _ = buf.Write([]byte("hello log\n"))
	_, _ = buf.Write([]byte("auth attempt token=SUPERSECRETVALUE123\n"))
	// A non-nil core mounts the /api group; the logs handler never touches the DB.
	h := New(fakePinger{}, nil, core.New(nil, nil, nil), Options{Logs: buf})

	req := httptest.NewRequest(http.MethodGet, "/api/logs", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/logs = %d, want 200", rec.Code)
	}
	var body struct {
		Lines []string `json:"lines"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Lines) != 2 || body.Lines[0] != "hello log" {
		t.Fatalf("lines = %v, want [hello log, …redacted…]", body.Lines)
	}
	if strings.Contains(rec.Body.String(), "SUPERSECRETVALUE123") {
		t.Fatalf("secret leaked through /api/logs: %s", rec.Body.String())
	}
}

func TestLogsEndpointAbsentWithoutSink(t *testing.T) {
	h := New(fakePinger{}, nil, core.New(nil, nil, nil), Options{}) // no Logs sink
	req := httptest.NewRequest(http.MethodGet, "/api/logs", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("/api/logs should not be mounted without a log sink, got %d", rec.Code)
	}
}

func TestReadyzEmbedderUnreachableStillReady(t *testing.T) {
	down := func(context.Context) error { return errors.New("no ollama") }
	h := New(fakePinger{}, down, nil, Options{})
	code, body := do(t, h, "/readyz")
	if code != http.StatusOK { // graceful degradation: embedder down != not ready
		t.Fatalf("readyz code = %d, want 200", code)
	}
	if body["embedder"] != "unreachable" {
		t.Fatalf("embedder = %q", body["embedder"])
	}
}

func TestReadyzDBDownIs503(t *testing.T) {
	h := New(fakePinger{err: errors.New("db gone")}, nil, nil, Options{})
	code, body := do(t, h, "/readyz")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("readyz code = %d, want 503", code)
	}
	if body["db"] != "error" {
		t.Fatalf("db = %q", body["db"])
	}
}
