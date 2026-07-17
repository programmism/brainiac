// Package metrics is a tiny, dependency-free metrics registry: an HTTP latency
// histogram plus pull-based gauges, rendered in Prometheus text format. Kept
// hand-rolled to match the project's minimal-dependency stance (SYSTEM.md §3).
package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"
)

var defaultBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}

// Registry holds the overall request-latency histogram, per-route histograms and
// status counters, and named gauges.
type Registry struct {
	mu      sync.Mutex
	buckets []float64
	counts  []uint64
	sum     float64
	total   uint64
	gauges  []gauge
	// routes holds a latency histogram per matched route pattern (e.g. "/api/search"),
	// so /healthz and static assets don't pollute the /api p95 scaling signal (#259).
	routes map[string]*histogram
	// reqs counts requests by (route, status code) — the error-rate signal (#259).
	reqs map[routeCode]uint64
}

type histogram struct {
	counts []uint64
	sum    float64
	total  uint64
}

type routeCode struct {
	route string
	code  int
}

type gauge struct {
	name, help string
	fn         func() float64
}

// New creates an empty registry.
func New() *Registry {
	return &Registry{
		buckets: defaultBuckets,
		counts:  make([]uint64, len(defaultBuckets)),
		routes:  map[string]*histogram{},
		reqs:    map[routeCode]uint64{},
	}
}

func (r *Registry) observe(seconds float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.total++
	r.sum += seconds
	for i, b := range r.buckets {
		if seconds <= b {
			r.counts[i]++
		}
	}
}

// ObserveRoute records one request's route (a low-cardinality matched pattern),
// status code, and latency. The caller supplies the route so this package stays
// router-agnostic (the server extracts it from chi). An empty route is bucketed
// as "other" to bound cardinality against unmatched paths.
func (r *Registry) ObserveRoute(route string, code int, seconds float64) {
	if route == "" {
		route = "other"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	h := r.routes[route]
	if h == nil {
		h = &histogram{counts: make([]uint64, len(r.buckets))}
		r.routes[route] = h
	}
	h.total++
	h.sum += seconds
	for i, b := range r.buckets {
		if seconds <= b {
			h.counts[i]++
		}
	}
	r.reqs[routeCode{route, code}]++
}

// sortedKeys returns the route names in deterministic order for stable scrapes.
func sortedKeys(m map[string]histogram) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// sortedReqKeys orders (route, code) pairs by route then code.
func sortedReqKeys(m map[routeCode]uint64) []routeCode {
	keys := make([]routeCode, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].route != keys[j].route {
			return keys[i].route < keys[j].route
		}
		return keys[i].code < keys[j].code
	})
	return keys
}

// SetGauge registers a pull-based gauge (fn is called at scrape time).
func (r *Registry) SetGauge(name, help string, fn func() float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gauges = append(r.gauges, gauge{name, help, fn})
}

// Middleware records each request's wall-clock duration.
func (r *Registry) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, req)
		r.observe(time.Since(start).Seconds())
	})
}

// Quantile approximates a latency quantile (seconds) from the histogram buckets.
func (r *Registry) Quantile(q float64) float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.total == 0 {
		return 0
	}
	target := uint64(float64(r.total) * q)
	for i, c := range r.counts {
		if c >= target {
			return r.buckets[i]
		}
	}
	return r.buckets[len(r.buckets)-1]
}

// Handler serves the Prometheus text exposition.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Snapshot under lock, then render (gauge fns may hit the DB — don't hold it).
		r.mu.Lock()
		buckets := append([]float64(nil), r.buckets...)
		counts := append([]uint64(nil), r.counts...)
		sum, total := r.sum, r.total
		gauges := append([]gauge(nil), r.gauges...)
		routes := make(map[string]histogram, len(r.routes))
		for name, h := range r.routes {
			routes[name] = histogram{counts: append([]uint64(nil), h.counts...), sum: h.sum, total: h.total}
		}
		reqs := make(map[routeCode]uint64, len(r.reqs))
		for k, v := range r.reqs {
			reqs[k] = v
		}
		r.mu.Unlock()

		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprintln(w, "# HELP brainiac_http_request_duration_seconds HTTP request latency")
		fmt.Fprintln(w, "# TYPE brainiac_http_request_duration_seconds histogram")
		for i, b := range buckets {
			fmt.Fprintf(w, "brainiac_http_request_duration_seconds_bucket{le=\"%g\"} %d\n", b, counts[i])
		}
		fmt.Fprintf(w, "brainiac_http_request_duration_seconds_bucket{le=\"+Inf\"} %d\n", total)
		fmt.Fprintf(w, "brainiac_http_request_duration_seconds_sum %g\n", sum)
		fmt.Fprintf(w, "brainiac_http_request_duration_seconds_count %d\n", total)

		// Per-route latency (#259) — a separate metric so the overall one above stays
		// intact for existing dashboards. Routes rendered in sorted order for stable
		// scrapes.
		fmt.Fprintln(w, "# HELP brainiac_http_route_duration_seconds HTTP request latency by route")
		fmt.Fprintln(w, "# TYPE brainiac_http_route_duration_seconds histogram")
		for _, name := range sortedKeys(routes) {
			h := routes[name]
			for i, b := range buckets {
				fmt.Fprintf(w, "brainiac_http_route_duration_seconds_bucket{route=%q,le=\"%g\"} %d\n", name, b, h.counts[i])
			}
			fmt.Fprintf(w, "brainiac_http_route_duration_seconds_bucket{route=%q,le=\"+Inf\"} %d\n", name, h.total)
			fmt.Fprintf(w, "brainiac_http_route_duration_seconds_sum{route=%q} %g\n", name, h.sum)
			fmt.Fprintf(w, "brainiac_http_route_duration_seconds_count{route=%q} %d\n", name, h.total)
		}

		// Per-route, per-status request counter (#259) — the error-rate signal.
		fmt.Fprintln(w, "# HELP brainiac_http_requests_total HTTP requests by route and status code")
		fmt.Fprintln(w, "# TYPE brainiac_http_requests_total counter")
		for _, k := range sortedReqKeys(reqs) {
			fmt.Fprintf(w, "brainiac_http_requests_total{route=%q,code=\"%d\"} %d\n", k.route, k.code, reqs[k])
		}

		for _, g := range gauges {
			fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %g\n", g.name, g.help, g.name, g.name, g.fn())
		}
	})
}
