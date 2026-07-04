package core

import (
	"context"
	"testing"

	"github.com/programmism/brainiac/internal/store"
	"github.com/programmism/brainiac/internal/sysstat"
)

// deriveStatus is pure — thresholds turn numbers into a verdict. Covered without
// a database so the roll-up logic is verified everywhere CI runs.
func TestDeriveStatus(t *testing.T) {
	cases := []struct {
		name     string
		m        SystemMetrics
		want     string
		wantWarn int
	}{
		{"all healthy",
			SystemMetrics{Container: sysstat.Container{Available: true, UsedPercent: 40},
				DB: dbSection{DBStats: store.DBStats{ActiveConnections: 2, MaxConnections: 100},
					Pool: PoolStats{Acquired: 1, Max: 10}}},
			"ok", 0},
		{"memory warn",
			SystemMetrics{Container: sysstat.Container{Available: true, UsedPercent: 88}},
			"warn", 1},
		{"memory critical",
			SystemMetrics{Container: sysstat.Container{Available: true, UsedPercent: 97}},
			"critical", 1},
		{"connections warn",
			SystemMetrics{DB: dbSection{DBStats: store.DBStats{ActiveConnections: 90, MaxConnections: 100}}},
			"warn", 1},
		{"pool warn",
			SystemMetrics{DB: dbSection{Pool: PoolStats{Acquired: 9, Max: 10}}},
			"warn", 1},
		{"unavailable container is ignored",
			SystemMetrics{Container: sysstat.Container{Available: false, UsedPercent: 99}},
			"ok", 0},
		{"critical wins over warn",
			SystemMetrics{Container: sysstat.Container{Available: true, UsedPercent: 99},
				DB: dbSection{DBStats: store.DBStats{ActiveConnections: 90, MaxConnections: 100}}},
			"critical", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.m.Status = "ok"
			tc.m.deriveStatus()
			if tc.m.Status != tc.want {
				t.Errorf("status = %q, want %q", tc.m.Status, tc.want)
			}
			if len(tc.m.Warnings) != tc.wantWarn {
				t.Errorf("warnings = %v, want %d", tc.m.Warnings, tc.wantWarn)
			}
		})
	}
}

// SystemMetrics against a live DB: the byte counts and connection stats come
// back sane, and a fresh corpus is healthy.
func TestSystemMetricsDBGated(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()

	m, err := c.SystemMetrics(context.Background())
	if err != nil {
		t.Fatalf("system metrics: %v", err)
	}
	if m.DB.DatabaseSizeBytes <= 0 {
		t.Errorf("database size = %d, want > 0", m.DB.DatabaseSizeBytes)
	}
	if m.DB.MaxConnections <= 0 {
		t.Errorf("max_connections = %d, want > 0", m.DB.MaxConnections)
	}
	if m.DB.Pool.Max <= 0 {
		t.Errorf("pool max = %d, want > 0", m.DB.Pool.Max)
	}
	if m.Process.NumCPU < 1 {
		t.Errorf("num_cpu = %d, want >= 1", m.Process.NumCPU)
	}
	if m.Status != "ok" && m.Status != "warn" && m.Status != "critical" {
		t.Errorf("unexpected status %q", m.Status)
	}
}
