package core

import (
	"context"
	"testing"

	"github.com/programmism/brainiac/internal/plugins"
	"github.com/programmism/brainiac/internal/store"
)

// fakeExtractor returns a fixed Extraction, so the extraction→review→approve
// flow is exercised without a live Ollama.
type fakeExtractor struct{ ext plugins.Extraction }

func (f fakeExtractor) Extract(_ context.Context, _ string) (plugins.Extraction, error) {
	return f.ext, nil
}

func sampleExtraction() plugins.Extraction {
	return plugins.Extraction{
		Entities: []plugins.Entity{
			{Name: "OrderService", Type: "service"},
			{Name: "OrdersDB", Type: "datastore"},
		},
		Relations: []plugins.Relation{
			{From: "OrderService", Type: "writes to", To: "OrdersDB", Why: "persists orders"},
		},
	}
}

// TestExtractProposesThenApprove: with review on, extraction writes proposed
// nodes/edges that are invisible to the live graph until approved.
func TestExtractProposesThenApprove(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	c.extractor = fakeExtractor{sampleExtraction()}
	c.extractReview = true

	nodes, edges, err := c.extractChunk(ctx, "OrderService writes to OrdersDB", "src://doc1", nil)
	if err != nil {
		t.Fatalf("extractChunk: %v", err)
	}
	if nodes != 2 || edges != 1 {
		t.Fatalf("created nodes=%d edges=%d, want 2/1", nodes, edges)
	}

	// Invisible to the live graph while proposed.
	gn, ge, err := store.GraphSnapshot(ctx, pool, 100, store.NoWall())
	if err != nil {
		t.Fatalf("graph snapshot: %v", err)
	}
	if len(gn) != 0 || len(ge) != 0 {
		t.Fatalf("proposed rows leaked into live graph: %d nodes, %d edges", len(gn), len(ge))
	}

	// Visible in the proposal queue.
	q, err := c.Proposals(ctx, 100)
	if err != nil {
		t.Fatalf("proposals: %v", err)
	}
	if len(q.Nodes) != 2 || len(q.Edges) != 1 {
		t.Fatalf("queue nodes=%d edges=%d, want 2/1", len(q.Nodes), len(q.Edges))
	}
	if q.Edges[0].FromName != "OrderService" || q.Edges[0].ToName != "OrdersDB" {
		t.Fatalf("edge endpoints not resolved: %+v", q.Edges[0])
	}

	// Approving the edge promotes it AND both endpoints (no dangling live edge).
	if err := c.ApproveEdge(ctx, q.Edges[0].ID); err != nil {
		t.Fatalf("approve edge: %v", err)
	}
	gn, ge, err = store.GraphSnapshot(ctx, pool, 100, store.NoWall())
	if err != nil {
		t.Fatalf("graph snapshot 2: %v", err)
	}
	if len(gn) != 2 || len(ge) != 1 {
		t.Fatalf("after approve: %d nodes, %d edges, want 2/1", len(gn), len(ge))
	}
}

// TestExtractDedupWithinQueue: a second chunk naming the same entities reuses the
// pending nodes instead of stacking duplicates.
func TestExtractDedupWithinQueue(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	c.extractor = fakeExtractor{sampleExtraction()}
	c.extractReview = true

	if _, _, err := c.extractChunk(ctx, "chunk 1", "src://doc1", nil); err != nil {
		t.Fatalf("extractChunk 1: %v", err)
	}
	// Second chunk: same entities, so no new nodes; the edge upserts, not duplicates.
	n2, _, err := c.extractChunk(ctx, "chunk 2", "src://doc1", nil)
	if err != nil {
		t.Fatalf("extractChunk 2: %v", err)
	}
	if n2 != 0 {
		t.Fatalf("second chunk created %d nodes, want 0 (reuse pending)", n2)
	}
	q, err := c.Proposals(ctx, 100)
	if err != nil {
		t.Fatalf("proposals: %v", err)
	}
	if len(q.Nodes) != 2 {
		t.Fatalf("queue has %d proposed nodes, want 2 (deduped)", len(q.Nodes))
	}
}

// TestExtractReviewOffWritesLive: with review disabled, extraction writes current
// nodes/edges straight into the live graph.
func TestExtractReviewOffWritesLive(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	c.extractor = fakeExtractor{sampleExtraction()}
	c.extractReview = false

	if _, _, err := c.extractChunk(ctx, "OrderService writes to OrdersDB", "src://doc1", nil); err != nil {
		t.Fatalf("extractChunk: %v", err)
	}
	gn, ge, err := store.GraphSnapshot(ctx, pool, 100, store.NoWall())
	if err != nil {
		t.Fatalf("graph snapshot: %v", err)
	}
	if len(gn) != 2 || len(ge) != 1 {
		t.Fatalf("review-off: %d nodes, %d edges, want 2/1 live", len(gn), len(ge))
	}
	// And nothing sits in the proposal queue.
	q, err := c.Proposals(ctx, 100)
	if err != nil {
		t.Fatalf("proposals: %v", err)
	}
	if len(q.Nodes) != 0 || len(q.Edges) != 0 {
		t.Fatalf("review-off left proposals: %d nodes, %d edges", len(q.Nodes), len(q.Edges))
	}
}
