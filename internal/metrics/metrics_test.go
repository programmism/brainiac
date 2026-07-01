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
