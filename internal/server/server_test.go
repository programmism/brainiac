package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/programmism/brainiac/internal/core"
	"github.com/programmism/brainiac/internal/logbuf"
)

type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

// staticMatcher is a test PrincipalMatcher: it resolves a token to a principal by
// exact match, ignoring the clock (expiry/revocation are covered in config tests).
type staticMatcher map[string]*core.Principal

func (m staticMatcher) Match(token string, _ time.Time) *core.Principal { return m[token] }

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

// Under Layer 2 hard isolation every /api call — reads included — needs a valid
// principal token; the 401 short-circuits in middleware before any handler/DB, so
// this needs no database (#120).
func TestHardIsolationGatesReads(t *testing.T) {
	c := core.New(nil, nil, nil)
	matcher := staticMatcher{"good": {Name: "a", Read: []string{"a"}, Write: "a"}}
	h := New(fakePinger{}, nil, c, Options{Auth: matcher})

	for _, tc := range []struct {
		name, path, auth string
	}{
		{"read no token", "/api/search?q=x", ""},
		{"read wrong token", "/api/search?q=x", "Bearer nope"},
		{"graph no token", "/api/graph", ""},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		if tc.auth != "" {
			req.Header.Set("Authorization", tc.auth)
		}
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: got %d, want 401", tc.name, rec.Code)
		}
	}

	// Capabilities stays public under isolation and advertises that auth is
	// required, so the WebUI can prompt for a token before it has one.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/capabilities", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("capabilities must stay public: got %d", rec.Code)
	}
	var caps map[string]bool
	if err := json.Unmarshal(rec.Body.Bytes(), &caps); err != nil {
		t.Fatalf("caps decode: %v", err)
	}
	if !caps["auth_required"] {
		t.Fatalf("capabilities should report auth_required under isolation: %v", caps)
	}
}

// With no principals configured, reads stay open (Layer 1 unchanged).
func TestLayer1ReadsStayOpen(t *testing.T) {
	c := core.New(nil, nil, nil)
	h := New(fakePinger{}, nil, c, Options{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/capabilities", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("Layer 1 capabilities = %d, want 200 (open)", rec.Code)
	}
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

// The access logger emits one JSON object per request, carrying the chi
// request-id for correlation (#258).
func TestJSONAccessLog(t *testing.T) {
	var buf bytes.Buffer
	f := &jsonLogFormatter{w: &buf}

	req := httptest.NewRequest(http.MethodGet, "/api/search?q=secret", nil)
	req = req.WithContext(context.WithValue(req.Context(), middleware.RequestIDKey, "req-123"))
	entry := f.NewLogEntry(req)
	entry.Write(200, 512, nil, 3*time.Millisecond, nil)

	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("access log line is not valid JSON: %v\n%s", err, buf.String())
	}
	if rec["msg"] != "http_request" || rec["method"] != "GET" || rec["status"] != float64(200) {
		t.Errorf("fields wrong: %+v", rec)
	}
	if rec["request_id"] != "req-123" {
		t.Errorf("missing request_id: %+v", rec)
	}
	// The path is logged but never the query (which can carry secrets).
	if rec["path"] != "/api/search" {
		t.Errorf("path = %v, want /api/search (no query)", rec["path"])
	}
	if strings.Contains(buf.String(), "secret") {
		t.Errorf("query leaked into the access log: %s", buf.String())
	}
}

func TestHealthz(t *testing.T) {
	h := New(fakePinger{}, nil, nil, Options{})
	code, body := do(t, h, "/healthz")
	if code != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("healthz = %d %v", code, body)
	}
}

// Per-route metrics label the matched chi pattern and count status codes, so
// /healthz never pollutes the /api p95 and errors are visible (#259).
func TestPerRouteMetrics(t *testing.T) {
	c := core.New(nil, nil, nil)
	h := New(fakePinger{}, nil, c, Options{})

	// A healthy route and a guaranteed 404 (unmatched /api path is JSON 404).
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/does-not-exist", nil))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()

	if !strings.Contains(body, `brainiac_http_route_duration_seconds_count{route="/healthz"}`) {
		t.Errorf("healthz route latency not recorded:\n%s", body)
	}
	if !strings.Contains(body, `code="404"`) {
		t.Errorf("404 error not counted:\n%s", body)
	}
}

// The token bucket allows up to burst immediately, then refills at rps.
func TestRateLimiterAllow(t *testing.T) {
	l := newRateLimiter(2, 3) // 2 req/s, burst 3
	base := time.Unix(1700000000, 0)
	for i := 0; i < 3; i++ {
		if ok, _ := l.allow("k", base); !ok {
			t.Fatalf("request %d within burst should be allowed", i)
		}
	}
	ok, wait := l.allow("k", base)
	if ok {
		t.Fatal("4th immediate request should be denied (burst exhausted)")
	}
	if wait <= 0 || wait > time.Second {
		t.Fatalf("retry-after = %v, want ~0.5s", wait)
	}
	// After 1s, 2 tokens have refilled → two more allowed, third denied.
	later := base.Add(time.Second)
	if ok, _ := l.allow("k", later); !ok {
		t.Fatal("token should have refilled after 1s")
	}
	if ok, _ := l.allow("k", later); !ok {
		t.Fatal("second refilled token should be allowed")
	}
	if ok, _ := l.allow("k", later); ok {
		t.Fatal("only 2 tokens refill per second")
	}
	// A different client has its own independent bucket.
	if ok, _ := l.allow("other", later); !ok {
		t.Fatal("independent client should not share a bucket")
	}
}

// The middleware returns 429 + Retry-After once a client's burst is spent.
func TestRateLimitMiddleware429(t *testing.T) {
	c := core.New(nil, nil, nil)
	h := New(fakePinger{}, nil, c, Options{RateLimitRPS: 1, RateLimitBurst: 1})

	// capabilities is cheap and always mounted; the first call consumes the single
	// burst token, the immediate second is throttled.
	first := httptest.NewRecorder()
	h.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/api/capabilities", nil))
	if first.Code != http.StatusOK {
		t.Fatalf("first request = %d, want 200", first.Code)
	}
	second := httptest.NewRecorder()
	h.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/api/capabilities", nil))
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second request = %d, want 429", second.Code)
	}
	if second.Header().Get("Retry-After") == "" {
		t.Fatal("429 should carry a Retry-After header")
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

func TestCapabilities(t *testing.T) {
	// Default: read-only.
	assertWritable(t, New(fakePinger{}, nil, core.New(nil, nil, nil), Options{}), false)
	// Interactive + token: writable.
	assertWritable(t, New(fakePinger{}, nil, core.New(nil, nil, nil), Options{Writable: true, AuthToken: "secret"}), true)
	// Writable without a token stays read-only (secure by default).
	assertWritable(t, New(fakePinger{}, nil, core.New(nil, nil, nil), Options{Writable: true}), false)
}

func assertWritable(t *testing.T, h http.Handler, want bool) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/capabilities", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/capabilities = %d", rec.Code)
	}
	var body struct {
		Writable bool `json:"writable"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Writable != want {
		t.Fatalf("writable = %v, want %v", body.Writable, want)
	}
}

func TestUnmountedWriteRouteReturnsJSON(t *testing.T) {
	// Read-only deploy: the retire route is not mounted. It must still answer with
	// a JSON error, not a plain-text 404 the WebUI would fail to parse (#168).
	h := New(fakePinger{}, nil, core.New(nil, nil, nil), Options{})
	req := httptest.NewRequest(http.MethodPost, "/api/edges/abc/retire", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("unmounted write route returned 200")
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response was not JSON (%q): %v", rec.Body.String(), err)
	}
	if body.Error == "" {
		t.Fatalf("expected an error message, got %q", rec.Body.String())
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
