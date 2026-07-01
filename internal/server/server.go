// Package server builds the HTTP surface for the app: the WebUI/REST handler.
// For now it exposes the health endpoints that make the deployment
// self-verifiable; the core operations land in #19/#21. See SYSTEM.md §4.
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// Pinger is the minimal storage dependency readiness needs.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Checker reports whether an optional backend (e.g. the embedder) is reachable.
// A nil Checker means "not configured".
type Checker func(ctx context.Context) error

// New builds the HTTP handler exposing:
//
//   - GET /healthz — liveness: 200 as long as the process serves.
//   - GET /readyz  — readiness: gates on the database only. The embedder being
//     unreachable is reported but does NOT fail readiness, because capture and
//     existing search keep working without it (graceful degradation, §11).
func New(db Pinger, embedder Checker) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		resp := map[string]string{"db": "ok", "embedder": "ok"}
		code := http.StatusOK

		if err := db.Ping(ctx); err != nil {
			resp["db"] = "error"
			code = http.StatusServiceUnavailable
		}
		if embedder == nil {
			resp["embedder"] = "not-configured"
		} else if err := embedder(ctx); err != nil {
			resp["embedder"] = "unreachable" // reported, not fatal
		}
		writeJSON(w, code, resp)
	})

	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
