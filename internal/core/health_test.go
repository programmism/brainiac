package core

import (
	"context"
	"testing"
)

func TestHealthCounts(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	// One edge between two new nodes.
	if _, err := c.Link(ctx, LinkInput{From: "A", Type: "depends_on", To: "B", Why: "x"}); err != nil {
		t.Fatalf("link: %v", err)
	}

	m, err := c.Health(ctx)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if m.Nodes != 2 {
		t.Errorf("nodes = %d, want 2", m.Nodes)
	}
	if m.Edges != 1 {
		t.Errorf("edges = %d, want 1", m.Edges)
	}
	if m.EdgesPerNode != 0.5 {
		t.Errorf("edges/node = %.2f, want 0.5", m.EdgesPerNode)
	}
}

// The subsystem telemetry (#319): ingest bumps the process chunk counter, and the
// review queue reflects proposed nodes/edges.
func TestSubsystemTelemetry(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	before := c.IngestedChunksTotal()
	s, err := c.IngestText(ctx, "doc://tel", "OrderService writes 1200 orders to Kafka for durability during peak load.", "")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	stored := uint64(s.Kept + s.Queued)
	if got := c.IngestedChunksTotal() - before; got != stored {
		t.Errorf("ingested-chunks counter rose by %d, want %d", got, stored)
	}

	// A proposed node/edge shows up in the review-queue depth.
	if _, err := pool.Exec(ctx, `INSERT INTO nodes (canonical_name, status, scope_key, discriminators) VALUES ('Proposed', 'proposed', '', '{}'::jsonb)`); err != nil {
		t.Fatalf("insert proposed: %v", err)
	}
	m, err := c.Health(ctx)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if m.ReviewQueue < 1 {
		t.Errorf("review queue = %d, want >= 1", m.ReviewQueue)
	}
}
