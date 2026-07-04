package core

import (
	"context"
	"fmt"
	"time"

	"github.com/programmism/brainiac/internal/store"
	"github.com/programmism/brainiac/internal/sysstat"
)

// Warning thresholds for the system status roll-up. A resource crossing Warn is
// worth watching; crossing Crit means the deployment is at its ceiling and an
// operator should raise the limit (SYSTEM.md §9). Kept here, in the core, so
// every client shows the same verdict.
const (
	memWarnPercent  = 85.0
	memCritPercent  = 95.0
	connWarnPercent = 80.0
	poolWarnPercent = 80.0
)

// PoolStats mirrors the pgx pool's saturation — how many connections are checked
// out against the configured ceiling. Acquired approaching Max means requests
// are starting to queue for a connection.
type PoolStats struct {
	Acquired int32 `json:"acquired"`
	Idle     int32 `json:"idle"`
	Total    int32 `json:"total"`
	Max      int32 `json:"max"`
}

// SystemMetrics is a point-in-time snapshot of operational health: the process
// footprint, the container's allocated-memory ceiling, and database size +
// connection saturation. Distinct from HealthMetrics, which counts the corpus.
type SystemMetrics struct {
	Status        string            `json:"status"` // "ok" | "warn" | "critical"
	Warnings      []string          `json:"warnings"`
	UptimeSeconds int64             `json:"uptime_seconds"`
	Process       sysstat.Process   `json:"process"`
	Container     sysstat.Container `json:"container"`
	DB            dbSection         `json:"db"`
}

type dbSection struct {
	store.DBStats
	VectorIndexBytes int64     `json:"vector_index_bytes"`
	Pool             PoolStats `json:"pool"`
}

// SystemMetrics assembles the operational snapshot and derives an overall status
// from the resource thresholds. DB reads that fail are non-fatal — the process
// and container sections still render — but a DB error is surfaced as a warning
// and downgrades the status, since a memory system with an unreachable store is
// not healthy.
func (c *Core) SystemMetrics(ctx context.Context) (SystemMetrics, error) {
	m := SystemMetrics{
		Status:        "ok",
		Warnings:      []string{},
		UptimeSeconds: int64(time.Since(c.startedAt).Seconds()),
		Process:       sysstat.ReadProcess(),
		Container:     sysstat.ReadContainer(),
	}

	if st := c.pool.Stat(); st != nil {
		m.DB.Pool = PoolStats{
			Acquired: st.AcquiredConns(),
			Idle:     st.IdleConns(),
			Total:    st.TotalConns(),
			Max:      st.MaxConns(),
		}
	}
	if s, err := store.DBStatsFor(ctx, c.pool); err != nil {
		m.raise("critical", fmt.Sprintf("database stats unavailable: %v", err))
	} else {
		m.DB.DBStats = s
	}
	if idx, err := c.IndexSizeBytes(ctx); err == nil {
		m.DB.VectorIndexBytes = idx
	}

	m.deriveStatus()
	return m, nil
}

// deriveStatus turns the raw numbers into warnings and an overall verdict.
func (m *SystemMetrics) deriveStatus() {
	if m.Container.Available {
		switch {
		case m.Container.UsedPercent >= memCritPercent:
			m.raise("critical", fmt.Sprintf("container memory at %.0f%% of its limit", m.Container.UsedPercent))
		case m.Container.UsedPercent >= memWarnPercent:
			m.raise("warn", fmt.Sprintf("container memory at %.0f%% of its limit", m.Container.UsedPercent))
		}
	}
	if m.DB.MaxConnections > 0 {
		if pct := 100 * float64(m.DB.ActiveConnections) / float64(m.DB.MaxConnections); pct >= connWarnPercent {
			m.raise("warn", fmt.Sprintf("database connections at %.0f%% of max_connections", pct))
		}
	}
	if m.DB.Pool.Max > 0 {
		if pct := 100 * float64(m.DB.Pool.Acquired) / float64(m.DB.Pool.Max); pct >= poolWarnPercent {
			m.raise("warn", fmt.Sprintf("connection pool at %.0f%% of its size", pct))
		}
	}
}

// raise records a warning and escalates the status. "critical" never downgrades
// to "warn"; order of calls doesn't matter.
func (m *SystemMetrics) raise(level, msg string) {
	m.Warnings = append(m.Warnings, msg)
	if level == "critical" || m.Status == "critical" {
		m.Status = "critical"
	} else {
		m.Status = "warn"
	}
}
