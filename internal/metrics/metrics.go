// Package metrics is a tiny, dependency-free metrics registry: an HTTP latency
// histogram plus pull-based gauges, rendered in Prometheus text format. Kept
// hand-rolled to match the project's minimal-dependency stance (SYSTEM.md §3).
package metrics

import (
	"fmt"
	"net/http"
	"sync"
	"time"
)

var defaultBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}

// Registry holds the request-latency histogram and named gauges.
type Registry struct {
	mu      sync.Mutex
	buckets []float64
	counts  []uint64
	sum     float64
	total   uint64
	gauges  []gauge
}

type gauge struct {
	name, help string
	fn         func() float64
}

// New creates an empty registry.
func New() *Registry {
	return &Registry{buckets: defaultBuckets, counts: make([]uint64, len(defaultBuckets))}
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
		for _, g := range gauges {
			fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %g\n", g.name, g.help, g.name, g.name, g.fn())
		}
	})
}
