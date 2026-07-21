// Package server builds the HTTP surface for the app: health endpoints and the
// read-only REST API over the core (ADR 0001). It is a thin adapter — handlers
// forward to internal/core and render JSON; no business logic lives here.
package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/programmism/brainiac/internal/core"
	"github.com/programmism/brainiac/internal/logbuf"
	"github.com/programmism/brainiac/internal/metrics"
	"github.com/programmism/brainiac/internal/sysstat"
	"github.com/programmism/brainiac/internal/webui"
)

// Pinger is the minimal storage dependency readiness needs.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Checker reports whether an optional backend (e.g. the embedder) is reachable.
// A nil Checker means "not configured".
type Checker func(ctx context.Context) error

// ErrEmbedderModelMissing is returned by a Checker when the embedder is reachable
// but the required model hasn't been pulled yet — a distinct, actionable state
// from "unreachable" (the model pull is still running or failed, #250).
var ErrEmbedderModelMissing = errors.New("embedder model not pulled")

// Options controls the writable surface. Secure by default: write endpoints are
// mounted only when Writable is true AND an AuthToken is set, and then they
// require `Authorization: Bearer <AuthToken>`. Read endpoints stay open (put the
// whole surface behind the reverse proxy for read protection — §16).
type Options struct {
	Writable  bool
	AuthToken string
	// Auth, when non-nil, turns on Layer 2 hard isolation (#120): every /api call
	// must present a bearer token the matcher recognizes, and the core walls reads
	// / pins writes to that token's principal. The matcher resolves tokens against
	// the wall clock so expiry/revocation apply live (#269); a reloadable matcher
	// swaps the roster on SIGHUP. Nil = Layer 1 (open reads, single AuthToken gates
	// writes) — unchanged.
	Auth PrincipalMatcher
	// Logs, when set, is the in-memory log sink the access logger tees into and
	// GET /api/logs reads from (the WebUI Logs tab, #166). Nil keeps the default
	// access logger and mounts no logs endpoint.
	Logs *logbuf.Buffer
	// RateLimitRPS > 0 turns on per-client /api rate limiting (#270): each client
	// (principal, else bearer token, else source IP) gets a token bucket refilling
	// at this many requests/sec. RateLimitBurst is the bucket depth (defaulted to
	// ceil(rps), min 1, when <= 0). 0 RPS = no limiting.
	RateLimitRPS   float64
	RateLimitBurst int
	// MCP, when non-nil, is the streamable-HTTP MCP handler mounted at /mcp behind
	// the AuthToken bearer gate, so clients can register brainiac as an HTTP MCP
	// transport and auto-reconnect across app restarts (#440). Nil = not served
	// (stdio remains the default transport).
	MCP http.Handler
}

// New builds the HTTP handler:
//   - GET /healthz — liveness.
//   - GET /readyz  — readiness (DB-gated; embedder reported, not fatal — §11).
//   - GET /api/capabilities — {writable, auth_required}, so the WebUI can gate
//     its controls and learn whether a token is required (public even under
//     hard isolation).
//   - GET /api/health, /api/system, /api/search, /api/recall, /api/graph,
//     /api/consolidate, /api/proposals — read REST.
//   - GET /api/logs — recent app + access logs, only when a log sink is set (#166).
//   - POST /api/merge, /api/split, /api/edges/{id}/confirm|flag-stale|retire,
//     /api/proposals/{nodes|edges}/{id}/{approve|reject} — writes, only when
//     opts.Writable && opts.AuthToken != "", behind bearer auth.
//
// Unmatched /api routes and methods answer with a JSON error, never plain text,
// so a client never has to parse a non-JSON body (#168).
func New(db Pinger, embedder Checker, c *core.Core, opts Options) http.Handler {
	reg := metrics.New()

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(requestLogger) // bind a request_id-tagged logger to the ctx (#348)
	r.Use(accessLogger(opts.Logs))
	r.Use(middleware.Recoverer)
	r.Use(reg.Middleware)
	r.Use(routeMetrics(reg))
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
		registerHealthGauges(reg, c)
	}

	// Writes are live only when explicitly enabled AND a token is set (secure by
	// default). The WebUI reads this via /api/capabilities to gate its action
	// buttons instead of firing them at unmounted routes.
	//
	// Under Layer 2 hard isolation (principals configured), the whole /api surface
	// requires a principal token — reads included — and the operator-only curation
	// write group is not mounted (id-based curation crosses namespaces; deferred to
	// #188). Writes into an isolated namespace flow through MCP, pinned per-token.
	hardIso := opts.Auth != nil
	writeEnabled := opts.Writable && opts.AuthToken != "" && !hardIso

	if c != nil {
		r.Route("/api", func(r chi.Router) {
			if hardIso {
				r.Use(principalAuth(opts.Auth))
			}
			// Rate limiting sits after auth so it keys on the resolved principal
			// (falling back to token/IP). Each /api/search costs an Ollama embed, so
			// this is the front-line cap against one caller exhausting the box (#270).
			if opts.RateLimitRPS > 0 {
				burst := opts.RateLimitBurst
				if burst < 1 {
					burst = int(math.Ceil(opts.RateLimitRPS))
					if burst < 1 {
						burst = 1
					}
				}
				r.Use(rateLimit(newRateLimiter(opts.RateLimitRPS, burst)))
			}
			// API errors are always JSON, even for unmatched routes/methods, so a
			// client (the WebUI) never gets a plain-text body it then fails to parse
			// as JSON (#168). Without this, a write button in read-only mode POSTs to
			// an unmounted route and chi's default plain-text 404 surfaces as a
			// cryptic "invalid JSON" error in the browser.
			r.NotFound(func(w http.ResponseWriter, req *http.Request) {
				writeError(req.Context(), w, http.StatusNotFound, errNotFound)
			})
			r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
				writeError(req.Context(), w, http.StatusMethodNotAllowed, errMethodNotAllowed)
			})

			// Capabilities: what the current deployment allows, so the UI can gate
			// its controls. No DB; kept PUBLIC even under hard isolation (booleans
			// only, no memory data) so the WebUI can learn a token is required and
			// prompt for one — principalAuth allow-lists this path.
			r.Get("/capabilities", func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, map[string]bool{"writable": writeEnabled, "auth_required": hardIso})
			})

			r.Get("/health", func(w http.ResponseWriter, req *http.Request) {
				m, err := c.Health(req.Context())
				if err != nil {
					writeError(req.Context(), w, http.StatusInternalServerError, err)
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
					writeError(req.Context(), w, http.StatusBadRequest, errMissingQ)
					return
				}
				k, _ := strconv.Atoi(req.URL.Query().Get("k"))
				// Optional ?project= scopes the lens to that project + global;
				// omitting it spans all scopes (#119). ?include_cold=true also
				// searches the cold archive (slower, no index — #365).
				includeCold := req.URL.Query().Get("include_cold") == "true"
				hits, err := c.Search(req.Context(), q, k, req.URL.Query().Get("project"), includeCold)
				if err != nil {
					handleCoreErr(req.Context(), w, err)
					return
				}
				writeJSON(w, http.StatusOK, hits)
			})
			r.Get("/recall", func(w http.ResponseWriter, req *http.Request) {
				q := req.URL.Query().Get("q")
				if q == "" {
					writeError(req.Context(), w, http.StatusBadRequest, errMissingQ)
					return
				}
				res, err := c.Recall(req.Context(), q, req.URL.Query().Get("project"))
				if err != nil {
					handleCoreErr(req.Context(), w, err)
					return
				}
				writeJSON(w, http.StatusOK, res)
			})

			// Direct entity lookup by name (+ optional project) or id, with edges.
			r.Get("/node", func(w http.ResponseWriter, req *http.Request) {
				qv := req.URL.Query()
				name, id := qv.Get("name"), qv.Get("id")
				if name == "" && id == "" {
					writeError(req.Context(), w, http.StatusBadRequest, errMissingNodeRef)
					return
				}
				det, err := c.GetNode(req.Context(), id, name, qv.Get("project"))
				if err != nil {
					handleCoreErr(req.Context(), w, err)
					return
				}
				if det == nil {
					writeError(req.Context(), w, http.StatusNotFound, errNodeNotFound)
					return
				}
				writeJSON(w, http.StatusOK, det)
			})

			// Operational snapshot: process footprint, container memory ceiling,
			// DB size + connection saturation (§9). Read-only, same posture as
			// the other read endpoints.
			r.Get("/system", func(w http.ResponseWriter, req *http.Request) {
				sm, err := c.SystemMetrics(req.Context())
				if err != nil {
					writeError(req.Context(), w, http.StatusInternalServerError, err)
					return
				}
				writeJSON(w, http.StatusOK, sm)
			})

			// Recent application + access logs (WebUI Logs tab, #166). Mounted only
			// when a log sink is configured; secrets are redacted at capture time.
			// Same open-read posture as /system — protect the surface via the proxy.
			// NOT mounted under hard isolation: the log ring is process-global and
			// its access lines include other tenants' ?q= search text and namespaces,
			// which a per-token principal must not read (#268). Logs stay operator-
			// only (docker logs / a Layer-1 deployment).
			if opts.Logs != nil && !hardIso {
				r.Get("/logs", func(w http.ResponseWriter, req *http.Request) {
					limit, _ := strconv.Atoi(req.URL.Query().Get("limit"))
					writeJSON(w, http.StatusOK, map[string]any{"lines": opts.Logs.Lines(limit)})
				})
			}

			r.Get("/graph", func(w http.ResponseWriter, req *http.Request) {
				limit, _ := strconv.Atoi(req.URL.Query().Get("limit"))
				g, err := c.Graph(req.Context(), limit)
				if err != nil {
					writeError(req.Context(), w, http.StatusInternalServerError, err)
					return
				}
				writeJSON(w, http.StatusOK, g)
			})

			// Consolidation queue (interactive).
			r.Get("/consolidate", func(w http.ResponseWriter, req *http.Request) {
				rep, err := c.Consolidate(req.Context(), false)
				if err != nil {
					writeError(req.Context(), w, http.StatusInternalServerError, err)
					return
				}
				writeJSON(w, http.StatusOK, rep)
			})

			// Extraction proposal queue (local-LLM output awaiting review). Empty
			// unless the extractor is enabled.
			r.Get("/proposals", func(w http.ResponseWriter, req *http.Request) {
				limit, _ := strconv.Atoi(req.URL.Query().Get("limit"))
				q, err := c.Proposals(req.Context(), limit)
				if err != nil {
					writeError(req.Context(), w, http.StatusInternalServerError, err)
					return
				}
				writeJSON(w, http.StatusOK, q)
			})
			// Write endpoints: mounted only when explicitly writable + a token is
			// set, and gated by bearer auth. Secure by default.
			if writeEnabled {
				r.Group(func(r chi.Router) {
					r.Use(bearerAuth(opts.AuthToken))
					r.Post("/merge", func(w http.ResponseWriter, req *http.Request) {
						var body struct{ Keep, Drop string }
						if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 64<<10)).Decode(&body); err != nil {
							writeError(req.Context(), w, http.StatusBadRequest, err)
							return
						}
						if err := c.ApplyMerge(req.Context(), body.Keep, body.Drop); err != nil {
							handleCoreErr(req.Context(), w, err)
							return
						}
						writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
					})
					r.Post("/split", func(w http.ResponseWriter, req *http.Request) {
						var body struct {
							NodeID string            `json:"node_id"`
							Axis   string            `json:"axis"`
							Routes map[string]string `json:"routes"`
						}
						if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 256<<10)).Decode(&body); err != nil {
							writeError(req.Context(), w, http.StatusBadRequest, err)
							return
						}
						res, err := c.Split(req.Context(), body.NodeID, body.Axis, body.Routes)
						if err != nil {
							handleCoreErr(req.Context(), w, err)
							return
						}
						writeJSON(w, http.StatusOK, res)
					})
					r.Post("/edges/{id}/confirm", func(w http.ResponseWriter, req *http.Request) {
						if err := c.Confirm(req.Context(), chi.URLParam(req, "id")); err != nil {
							handleCoreErr(req.Context(), w, err)
							return
						}
						writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
					})
					r.Post("/edges/{id}/flag-stale", func(w http.ResponseWriter, req *http.Request) {
						if err := c.FlagStale(req.Context(), chi.URLParam(req, "id")); err != nil {
							handleCoreErr(req.Context(), w, err)
							return
						}
						writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
					})
					r.Post("/edges/{id}/retire", func(w http.ResponseWriter, req *http.Request) {
						if err := c.RetireEdge(req.Context(), chi.URLParam(req, "id")); err != nil {
							handleCoreErr(req.Context(), w, err)
							return
						}
						writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
					})

					// Extraction proposal review: approve promotes to live, reject
					// retires. Node and edge each have their own path.
					r.Post("/proposals/nodes/{id}/approve", proposalHandler(c.ApproveNode))
					r.Post("/proposals/nodes/{id}/reject", proposalHandler(c.RejectNode))
					r.Post("/proposals/edges/{id}/approve", proposalHandler(c.ApproveEdge))
					r.Post("/proposals/edges/{id}/reject", proposalHandler(c.RejectEdge))
				})
			}
		})
	}

	// MCP over streamable HTTP (#440), behind the same bearer gate as writes: the
	// endpoint exposes write tools and the app binds localhost by default, so it
	// still requires AUTH_TOKEN. Mounted only when the app wired a handler in.
	if opts.MCP != nil {
		r.Handle("/mcp", bearerAuth(opts.AuthToken)(opts.MCP))
	}

	// Read-only WebUI as a catch-all (specific routes above win).
	r.Handle("/*", webui.Handler())

	return r
}

// accessLogger returns the request-logging middleware. When a log sink is set,
// each request line is teed into it (alongside stderr) so the WebUI Logs tab
// sees access logs — including 4xx like an auth-rejected write — not just the
// 5xx internal errors the app logs via the standard logger. Without a sink it
// falls back to chi's default logger.
func accessLogger(logs *logbuf.Buffer) func(http.Handler) http.Handler {
	// Structured JSON to stdout (Docker's json-file driver rotates it) so the
	// durable request log survives crashes — the 2000-line in-memory ring is only
	// a WebUI convenience now (#258). Each line carries the chi request-id so a
	// request can be correlated across log lines.
	var out io.Writer = os.Stdout
	if logs != nil {
		out = io.MultiWriter(os.Stdout, logs)
	}
	return middleware.RequestLogger(&jsonLogFormatter{w: out})
}

// jsonLogFormatter renders chi's per-request log as one JSON object per line.
type jsonLogFormatter struct{ w io.Writer }

func (f *jsonLogFormatter) NewLogEntry(r *http.Request) middleware.LogEntry {
	return &jsonLogEntry{
		w:      f.w,
		method: r.Method,
		path:   r.URL.Path, // path only — never the query, which can carry secrets
		remote: r.RemoteAddr,
		reqID:  middleware.GetReqID(r.Context()),
	}
}

type jsonLogEntry struct {
	w                           io.Writer
	method, path, remote, reqID string
}

func (e *jsonLogEntry) Write(status, bytes int, _ http.Header, elapsed time.Duration, _ any) {
	e.emit(map[string]any{
		"level":       "info",
		"msg":         "http_request",
		"method":      e.method,
		"path":        e.path,
		"status":      status,
		"bytes":       bytes,
		"duration_ms": float64(elapsed.Microseconds()) / 1000.0,
		"remote":      e.remote,
	})
}

func (e *jsonLogEntry) Panic(v any, _ []byte) {
	e.emit(map[string]any{
		"level":  "error",
		"msg":    "http_panic",
		"method": e.method,
		"path":   e.path,
		"panic":  fmt.Sprint(v),
	})
}

func (e *jsonLogEntry) emit(rec map[string]any) {
	rec["ts"] = time.Now().UTC().Format(time.RFC3339Nano)
	if e.reqID != "" {
		rec["request_id"] = e.reqID
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(e.w, string(b))
}

// proposalHandler adapts a core review action (approve/reject a node/edge by id)
// to an HTTP handler, so the four proposal routes share one shape.
func proposalHandler(action func(context.Context, string) error) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if err := action(req.Context(), chi.URLParam(req, "id")); err != nil {
			writeError(req.Context(), w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
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
		} else if err := embedder(ctx); errors.Is(err, ErrEmbedderModelMissing) {
			resp["embedder"] = "model-missing" // reachable, but the model isn't pulled yet
		} else if err != nil {
			resp["embedder"] = "unreachable" // reported, not fatal
		}
		writeJSON(w, code, resp)
	}
}

type stringError string

func (e stringError) Error() string { return string(e) }

const (
	errMissingQ         = stringError("missing required query parameter 'q'")
	errMissingNodeRef   = stringError("missing required query parameter 'name' or 'id'")
	errNodeNotFound     = stringError("entity not found")
	errUnauthorized     = stringError("unauthorized")
	errNotFound         = stringError("not found")
	errMethodNotAllowed = stringError("method not allowed")
	errRateLimited      = stringError("rate limit exceeded")
)

// bearerAuth requires a matching `Authorization: Bearer <token>` header.
func bearerAuth(token string) func(http.Handler) http.Handler {
	want := []byte("Bearer " + token)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got := []byte(r.Header.Get("Authorization"))
			if subtle.ConstantTimeCompare(got, want) != 1 {
				writeError(r.Context(), w, http.StatusUnauthorized, errUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// registerHealthGauges exposes the corpus/graph health signals — collected only
// as on-demand JSON before — as Prometheus gauges, so an operator can build a
// panel or alert (#255). The counts come from one c.Health() query cached ~10s so
// a scrape doesn't fan out into a query per gauge. Container memory is added too so
// index-vs-RAM (the ★ scaling ratio, #256) is alertable externally.
func registerHealthGauges(reg *metrics.Registry, c *core.Core) {
	var (
		mu       sync.Mutex
		at       time.Time
		snap     core.HealthMetrics
		haveSnap bool
	)
	health := func() core.HealthMetrics {
		mu.Lock()
		defer mu.Unlock()
		if haveSnap && time.Since(at) < 10*time.Second {
			return snap
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if h, err := c.Health(ctx); err == nil {
			snap, at, haveSnap = h, time.Now(), true
		}
		return snap
	}
	g := func(name, help string, f func(core.HealthMetrics) float64) {
		reg.SetGauge(name, help, func() float64 { return f(health()) })
	}
	g("brainiac_nodes_current", "current nodes in the graph", func(h core.HealthMetrics) float64 { return float64(h.Nodes) })
	g("brainiac_nodes_historical", "historical (superseded) nodes", func(h core.HealthMetrics) float64 { return float64(h.NodesHistorical) })
	g("brainiac_edges_current", "current edges", func(h core.HealthMetrics) float64 { return float64(h.Edges) })
	g("brainiac_edges_historical", "historical edges", func(h core.HealthMetrics) float64 { return float64(h.EdgesHistorical) })
	g("brainiac_edges_stale", "edges flagged stale for review", func(h core.HealthMetrics) float64 { return float64(h.EdgesStale) })
	g("brainiac_chunks_hot", "hot-tier chunks (searchable)", func(h core.HealthMetrics) float64 { return float64(h.ChunksHot) })
	g("brainiac_chunks_cold", "cold-tier chunks (archived)", func(h core.HealthMetrics) float64 { return float64(h.ChunksCold) })
	g("brainiac_edges_per_node", "average current edges per node", func(h core.HealthMetrics) float64 { return h.EdgesPerNode })
	g("brainiac_percent_nodes_historical", "percent of nodes that are historical", func(h core.HealthMetrics) float64 { return h.PercentNodesHistory })
	g("brainiac_percent_edges_stale", "percent of current edges flagged stale", func(h core.HealthMetrics) float64 { return h.PercentEdgesStale })
	// Consolidation queue depth: proposed nodes+edges awaiting review (#319). The
	// embedding backlog is already exposed as brainiac_chunks_cold.
	g("brainiac_review_queue_depth", "proposed nodes+edges awaiting review (consolidation queue)", func(h core.HealthMetrics) float64 { return float64(h.ReviewQueue) })

	reg.SetGauge("brainiac_container_mem_limit_bytes", "container memory limit (cgroup), 0 if unset", func() float64 {
		return float64(sysstat.ReadContainer().MemLimitBytes)
	})
	reg.SetGauge("brainiac_container_mem_used_bytes", "container memory in use (cgroup)", func() float64 {
		return float64(sysstat.ReadContainer().MemUsedBytes)
	})

	// Subsystem throughput/failure counters (#319) — read directly from the core's
	// process-lifetime atomics (not the ~10s-cached health snapshot).
	reg.SetCounter("brainiac_ingested_chunks_total", "chunks stored by ingest (cumulative)", func() float64 {
		return float64(c.IngestedChunksTotal())
	})
	reg.SetCounter("brainiac_extract_failures_total", "chunks whose extraction errored and was skipped (cumulative)", func() float64 {
		return float64(c.ExtractFailuresTotal())
	})
}

// PrincipalMatcher resolves a presented bearer token to its principal at a given
// time, or nil. Implemented by config.PrincipalAuthenticator (and a reloadable
// wrapper for hot revocation/rotation, #269). Passing now lets the matcher honor
// per-token expiry against the wall clock.
type PrincipalMatcher interface {
	Match(token string, now time.Time) *core.Principal
}

// principalAuth (Layer 2, #120) requires a bearer token the matcher recognizes
// and binds that principal to the request context for core enforcement. The
// matcher's comparison is constant-time and honors expiry/revocation (#269).
func principalAuth(m PrincipalMatcher) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Capabilities stays public so the WebUI can discover that a token is
			// required before it has one (booleans only, no memory data).
			if r.Method == http.MethodGet && r.URL.Path == "/api/capabilities" {
				next.ServeHTTP(w, r)
				return
			}
			match := m.Match(bearerToken(r.Header.Get("Authorization")), time.Now())
			if match == nil {
				writeError(r.Context(), w, http.StatusUnauthorized, errUnauthorized)
				return
			}
			next.ServeHTTP(w, r.WithContext(core.WithPrincipal(r.Context(), match)))
		})
	}
}

// routeMetrics records per-route latency and per-status request counts (#259). It
// reads the matched chi route pattern (bounded cardinality, e.g. "/api/search")
// after the handler runs, keeping the metrics package router-agnostic.
func routeMetrics(reg *metrics.Registry) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(sw, r)
			route := chi.RouteContext(r.Context()).RoutePattern()
			reg.ObserveRoute(route, sw.status, time.Since(start).Seconds())
		})
	}
}

// statusRecorder captures the response status code for the metrics middleware.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.wroteHeader = true // an implicit 200 if WriteHeader was never called
	return s.ResponseWriter.Write(b)
}

// Flush forwards to the underlying writer when it supports flushing, so the
// wrapper doesn't break streaming/SSE handlers.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// bearerToken extracts the token from an "Authorization: Bearer <token>" header,
// or "" if the scheme isn't Bearer. The scheme check isn't constant-time, but it
// leaks only the scheme, never token bytes — the matcher compares the token in
// constant time.
func bearerToken(header string) string {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return ""
	}
	return header[len(prefix):]
}

// rateLimiter is a per-client token-bucket limiter (#270). A client is the
// principal (Layer 2), else the bearer token, else the source IP — so one caller
// can't exhaust the shared Ollama/DB behind /api. Idle buckets are pruned
// opportunistically so the map stays bounded under open Layer-1 reads.
type rateLimiter struct {
	mu        sync.Mutex
	buckets   map[string]*tokenBucket
	rps       float64
	burst     float64
	lastPrune time.Time
}

type tokenBucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(rps float64, burst int) *rateLimiter {
	return &rateLimiter{buckets: make(map[string]*tokenBucket), rps: rps, burst: float64(burst)}
}

// allow charges one token to key at time now, returning whether it's permitted
// and, when not, how long until a token frees up (for Retry-After).
func (l *rateLimiter) allow(key string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[key]
	if b == nil {
		b = &tokenBucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	// Refill by elapsed time, capped at burst.
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens += elapsed * l.rps
		if b.tokens > l.burst {
			b.tokens = l.burst
		}
		b.last = now
	}
	l.pruneLocked(now)
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) / l.rps * float64(time.Second))
	return false, wait
}

// pruneLocked drops full, idle buckets at most once a minute so the map can't
// grow without bound under a churn of source IPs. A full bucket has no debt, so
// forgetting it is equivalent to never having seen the client.
func (l *rateLimiter) pruneLocked(now time.Time) {
	if now.Sub(l.lastPrune) < time.Minute {
		return
	}
	l.lastPrune = now
	for k, b := range l.buckets {
		if b.tokens >= l.burst && now.Sub(b.last) > 2*time.Minute {
			delete(l.buckets, k)
		}
	}
}

// rateLimit rejects requests over the per-client budget with 429 + Retry-After.
func rateLimit(l *rateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ok, wait := l.allow(clientKey(r), time.Now())
			if !ok {
				w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
				writeError(r.Context(), w, http.StatusTooManyRequests, errRateLimited)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// clientKey identifies the caller for rate limiting: the resolved principal name
// (Layer 2), else a short hash of the bearer token (never the raw secret), else
// the source IP. The prefixes keep the three key spaces disjoint.
func clientKey(r *http.Request) string {
	if p := core.PrincipalFrom(r.Context()); p != nil {
		return "p:" + p.Name
	}
	if tok := bearerToken(r.Header.Get("Authorization")); tok != "" {
		sum := sha256.Sum256([]byte(tok))
		return "t:" + hex.EncodeToString(sum[:8])
	}
	ip := r.RemoteAddr
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}
	return "ip:" + ip
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
func handleCoreErr(ctx context.Context, w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, core.ErrEmbed):
		writeError(ctx, w, http.StatusServiceUnavailable, err)
	case errors.Is(err, core.ErrForbiddenNamespace):
		writeError(ctx, w, http.StatusForbidden, err)
	case errors.Is(err, core.ErrQuotaExceeded):
		writeError(ctx, w, http.StatusTooManyRequests, err)
	default:
		writeError(ctx, w, http.StatusInternalServerError, err)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError logs server-side (≥500) with the real error and returns a generic
// message to the client, so internal details never leak (#77). The ≥500 log line
// carries the request's request_id (#348) — the same id the JSON access log
// records — so a 5xx can be correlated across both logs.
func writeError(ctx context.Context, w http.ResponseWriter, code int, err error) {
	if code >= http.StatusInternalServerError {
		loggerFrom(ctx).Error("http error", "status", code, "err", err.Error())
		writeJSON(w, code, map[string]string{"error": http.StatusText(code)})
		return
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

// ctxKey is the private context key for the request-scoped logger (#348).
type ctxKey struct{}

// requestLogger is middleware that binds a logger tagged with the chi request-id
// to the request context, so app logs emitted while handling the request carry the
// same request_id as the access log. Register it after middleware.RequestID.
func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l := slog.With("request_id", middleware.GetReqID(r.Context()))
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, l)))
	})
}

// loggerFrom returns the request-scoped logger bound by requestLogger, or the
// default logger outside a request.
func loggerFrom(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}
