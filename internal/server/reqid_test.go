package server

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// TestRequestIDInErrorLog: a 5xx app log carries the request's request_id (#348),
// a <500 response logs nothing, and loggerFrom falls back outside a request.
func TestRequestIDInErrorLog(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	var gotID string
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(requestLogger)
	r.Get("/boom", func(w http.ResponseWriter, req *http.Request) {
		gotID = middleware.GetReqID(req.Context())
		writeError(req.Context(), w, http.StatusInternalServerError, errors.New("kaboom"))
	})
	r.Get("/bad", func(w http.ResponseWriter, req *http.Request) {
		writeError(req.Context(), w, http.StatusBadRequest, errors.New("nope"))
	})

	srv := httptest.NewServer(r)
	defer srv.Close()

	// 5xx: the app log line carries the same request_id the handler saw + the error.
	resp, err := http.Get(srv.URL + "/boom")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	if gotID == "" {
		t.Fatal("chi did not generate a request id")
	}
	logLine := buf.String()
	if !strings.Contains(logLine, gotID) {
		t.Fatalf("5xx log missing request_id %q:\n%s", gotID, logLine)
	}
	if !strings.Contains(logLine, "kaboom") || !strings.Contains(logLine, `"request_id"`) {
		t.Fatalf("5xx log missing error/request_id field:\n%s", logLine)
	}

	// <500: writeError does not log server-side, so nothing new is emitted.
	buf.Reset()
	resp2, err := http.Get(srv.URL + "/bad")
	if err != nil {
		t.Fatalf("get bad: %v", err)
	}
	_ = resp2.Body.Close()
	if buf.Len() != 0 {
		t.Fatalf("a 4xx should not emit an app log line, got:\n%s", buf.String())
	}
}

func TestLoggerFromFallback(t *testing.T) {
	// No bound logger → the default logger, never nil.
	if loggerFrom(context.Background()) == nil {
		t.Fatal("loggerFrom returned nil")
	}
}
