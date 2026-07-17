package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestQuantileAndRender(t *testing.T) {
	r := New()
	// 100 fast requests (~1ms) + 1 slow (5s).
	for i := 0; i < 100; i++ {
		r.observe(0.001)
	}
	r.observe(5)

	if p50 := r.Quantile(0.5); p50 > 0.005 {
		t.Errorf("p50 = %v, want small", p50)
	}
	r.SetGauge("brainiac_test_gauge", "test", func() float64 { return 42 })

	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "brainiac_http_request_duration_seconds_count 101") {
		t.Errorf("missing/incorrect count line:\n%s", body)
	}
	if !strings.Contains(body, "brainiac_test_gauge 42") {
		t.Errorf("gauge not rendered:\n%s", body)
	}
}

func TestPerRouteMetricsRender(t *testing.T) {
	r := New()
	r.ObserveRoute("/api/search", 200, 0.01)
	r.ObserveRoute("/api/search", 200, 0.02)
	r.ObserveRoute("/api/search", 500, 0.2)
	r.ObserveRoute("", 404, 0.001) // unmatched → "other"

	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()

	for _, want := range []string{
		`brainiac_http_route_duration_seconds_count{route="/api/search"} 3`,
		`brainiac_http_route_duration_seconds_count{route="other"} 1`,
		`brainiac_http_requests_total{route="/api/search",code="200"} 2`,
		`brainiac_http_requests_total{route="/api/search",code="500"} 1`,
		`brainiac_http_requests_total{route="other",code="404"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q:\n%s", want, body)
		}
	}
}

func TestMiddlewareObserves(t *testing.T) {
	r := New()
	h := r.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if r.total != 1 {
		t.Fatalf("total = %d, want 1", r.total)
	}
}
