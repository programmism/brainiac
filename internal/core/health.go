package core

import (
	"context"

	"github.com/programmism/brainiac/internal/store"
)

// HealthMetrics reports corpus and graph health (SYSTEM.md §9, PRD §14). The
// load-bearing scaling metrics (index size vs RAM, p95 latency) are observed at
// the infra layer; this covers the countable graph/corpus signals.
type HealthMetrics struct {
	store.Counts
	EdgesPerNode        float64 `json:"edges_per_node"`
	PercentNodesHistory float64 `json:"percent_nodes_historical"`
	PercentEdgesStale   float64 `json:"percent_edges_stale"`
}

// Health returns the current health metrics.
func (c *Core) Health(ctx context.Context) (HealthMetrics, error) {
	counts, err := store.HealthCounts(ctx, c.pool)
	if err != nil {
		return HealthMetrics{}, err
	}
	m := HealthMetrics{Counts: counts}
	if counts.Nodes > 0 {
		m.EdgesPerNode = float64(counts.Edges) / float64(counts.Nodes)
	}
	if total := counts.Nodes + counts.NodesHistorical; total > 0 {
		m.PercentNodesHistory = 100 * float64(counts.NodesHistorical) / float64(total)
	}
	if counts.Edges > 0 {
		m.PercentEdgesStale = 100 * float64(counts.EdgesStale) / float64(counts.Edges)
	}
	return m, nil
}
