// Package server builds the HTTP surface for the app: health endpoints and the
// read-only REST API over the core (ADR 0001). It is a thin adapter — handlers
// forward to internal/core and render JSON; no business logic lives here.
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/programmism/brainiac/internal/core"
	"github.com/programmism/brainiac/internal/webui"
)

// Pinger is the minimal storage dependency readiness needs.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Checker reports whether an optional backend (e.g. the embedder) is reachable.
// A nil Checker means "not configured".
type Checker func(ctx context.Context) error

// New builds the HTTP handler:
//   - GET /healthz — liveness.
//   - GET /readyz  — readiness (DB-gated; embedder reported, not fatal — §11).
//   - GET /api/health, /api/search, /api/recall — read-only REST over core
//     (mounted only when c is non-nil).
func New(db Pinger, embedder Checker, c *core.Core) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	r.Get("/readyz", readyz(db, embedder))

	if c != nil {
		r.Route("/api", func(r chi.Router) {
			r.Get("/health", func(w http.ResponseWriter, req *http.Request) {
				m, err := c.Health(req.Context())
				if err != nil {
					writeError(w, http.StatusInternalServerError, err)
					return
				}
				writeJSON(w, http.StatusOK, m)
			})
			r.Get("/search", func(w http.ResponseWriter, req *http.Request) {
				q := req.URL.Query().Get("q")
				if q == "" {
					writeError(w, http.StatusBadRequest, errMissingQ)
					return
				}
				k, _ := strconv.Atoi(req.URL.Query().Get("k"))
				hits, err := c.Search(req.Context(), q, k)
				if err != nil {
					writeError(w, http.StatusInternalServerError, err)
					return
				}
				writeJSON(w, http.StatusOK, hits)
			})
			r.Get("/recall", func(w http.ResponseWriter, req *http.Request) {
				q := req.URL.Query().Get("q")
				if q == "" {
					writeError(w, http.StatusBadRequest, errMissingQ)
					return
				}
				res, err := c.Recall(req.Context(), q)
				if err != nil {
					writeError(w, http.StatusInternalServerError, err)
					return
				}
				writeJSON(w, http.StatusOK, res)
			})
		})
	}

	// Read-only WebUI as a catch-all (specific routes above win).
	r.Handle("/*", webui.Handler())

	return r
}

func readyz(db Pinger, embedder Checker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
	}
}

type stringError string

func (e stringError) Error() string { return string(e) }

const errMissingQ = stringError("missing required query parameter 'q'")

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
