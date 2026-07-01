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
