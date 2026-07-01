// Package server builds the HTTP surface for the app: health endpoints and the
// read-only REST API over the core (ADR 0001). It is a thin adapter — handlers
// forward to internal/core and render JSON; no business logic lives here.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/programmism/brainiac/internal/core"
	"github.com/programmism/brainiac/internal/metrics"
	"github.com/programmism/brainiac/internal/webui"
)

// Pinger is the minimal storage dependency readiness needs.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Checker reports whether an optional backend (e.g. the embedder) is reachable.
// A nil Checker means "not configured".
type Checker func(ctx context.Context) error

// Options controls the writable surface. Secure by default: write endpoints are
// mounted only when Writable is true AND an AuthToken is set, and then they
// require `Authorization: Bearer <AuthToken>`. Read endpoints stay open (put the
// whole surface behind the reverse proxy for read protection — §16).
type Options struct {
	Writable  bool
	AuthToken string
}

// New builds the HTTP handler:
//   - GET /healthz — liveness.
//   - GET /readyz  — readiness (DB-gated; embedder reported, not fatal — §11).
//   - GET /api/health, /api/search, /api/recall, /api/graph, /api/consolidate — read REST.
//   - POST /api/merge, /api/edges/{id}/confirm|flag-stale — writes, only when
//     opts.Writable && opts.AuthToken != "", behind bearer auth.
func New(db Pinger, embedder Checker, c *core.Core, opts Options) http.Handler {
	reg := metrics.New()

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(reg.Middleware)
	r.Use(middleware.Timeout(30 * time.Second))

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": core.Version})
	})
	r.Get("/readyz", readyz(db, embedder))
	r.Handle("/metrics", reg.Handler())

	if c != nil {
		reg.SetGauge("brainiac_vector_index_bytes", "hot-tier HNSW vector index size in bytes", func() float64 {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			n, _ := c.IndexSizeBytes(ctx)
			return float64(n)
		})
	}

	if c != nil {
		r.Route("/api", func(r chi.Router) {
			r.Get("/health", func(w http.ResponseWriter, req *http.Request) {
				m, err := c.Health(req.Context())
				if err != nil {
					writeError(w, http.StatusInternalServerError, err)
					return
				}
				idx, _ := c.IndexSizeBytes(req.Context())
				writeJSON(w, http.StatusOK, healthResponse{
					HealthMetrics:   m,
					Version:         core.Version,
					VectorIndexByte: idx,
					LatencyP50ms:    reg.Quantile(0.50) * 1000,
					LatencyP95ms:    reg.Quantile(0.95) * 1000,
				})
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
					handleCoreErr(w, err)
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
					handleCoreErr(w, err)
					return
				}
				writeJSON(w, http.StatusOK, res)
			})

			r.Get("/graph", func(w http.ResponseWriter, req *http.Request) {
				limit, _ := strconv.Atoi(req.URL.Query().Get("limit"))
				g, err := c.Graph(req.Context(), limit)
				if err != nil {
					writeError(w, http.StatusInternalServerError, err)
					return
				}
				writeJSON(w, http.StatusOK, g)
			})

			// Consolidation queue (interactive).
			r.Get("/consolidate", func(w http.ResponseWriter, req *http.Request) {
				rep, err := c.Consolidate(req.Context())
				if err != nil {
					writeError(w, http.StatusInternalServerError, err)
					return
				}
				writeJSON(w, http.StatusOK, rep)
			})
			// Write endpoints: mounted only when explicitly writable + a token is
			// set, and gated by bearer auth. Secure by default.
			if opts.Writable && opts.AuthToken != "" {
				r.Group(func(r chi.Router) {
					r.Use(bearerAuth(opts.AuthToken))
					r.Post("/merge", func(w http.ResponseWriter, req *http.Request) {
						var body struct{ Keep, Drop string }
						if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 64<<10)).Decode(&body); err != nil {
							writeError(w, http.StatusBadRequest, err)
							return
						}
						if err := c.ApplyMerge(req.Context(), body.Keep, body.Drop); err != nil {
							writeError(w, http.StatusInternalServerError, err)
							return
						}
						writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
					})
					r.Post("/edges/{id}/confirm", func(w http.ResponseWriter, req *http.Request) {
						if err := c.Confirm(req.Context(), chi.URLParam(req, "id")); err != nil {
							writeError(w, http.StatusInternalServerError, err)
							return
						}
						writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
					})
					r.Post("/edges/{id}/flag-stale", func(w http.ResponseWriter, req *http.Request) {
						if err := c.FlagStale(req.Context(), chi.URLParam(req, "id")); err != nil {
							writeError(w, http.StatusInternalServerError, err)
							return
						}
						writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
					})
				})
			}
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

const (
	errMissingQ     = stringError("missing required query parameter 'q'")
	errUnauthorized = stringError("unauthorized")
)

// bearerAuth requires a matching `Authorization: Bearer <token>` header.
func bearerAuth(token string) func(http.Handler) http.Handler {
	want := []byte("Bearer " + token)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got := []byte(r.Header.Get("Authorization"))
			if subtle.ConstantTimeCompare(got, want) != 1 {
				writeError(w, http.StatusUnauthorized, errUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// healthResponse enriches the core metrics with operational fields.
type healthResponse struct {
	core.HealthMetrics
	Version         string  `json:"version"`
	VectorIndexByte int64   `json:"vector_index_bytes"`
	LatencyP50ms    float64 `json:"latency_p50_ms"`
	LatencyP95ms    float64 `json:"latency_p95_ms"`
}

// handleCoreErr maps an embedder outage to 503 and everything else to 500.
func handleCoreErr(w http.ResponseWriter, err error) {
	if errors.Is(err, core.ErrEmbed) {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	writeError(w, http.StatusInternalServerError, err)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError logs server-side (≥500) with the real error and returns a generic
// message to the client, so internal details never leak (#77).
func writeError(w http.ResponseWriter, code int, err error) {
	if code >= http.StatusInternalServerError {
		log.Printf("http %d: %v", code, err)
		writeJSON(w, code, map[string]string{"error": http.StatusText(code)})
		return
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
