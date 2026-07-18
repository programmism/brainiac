package core

import (
	"context"
	"errors"
	"testing"

	"github.com/programmism/brainiac/internal/model"
	"github.com/programmism/brainiac/internal/store"
)

// TestEraseNodeAndSource: EraseNode hard-deletes an entity + its edges (FK-safe),
// EraseSource purges a document's chunks + edges, and both are wall-guarded (#272).
func TestEraseNodeAndSource(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	a := &model.Node{CanonicalName: "Alice", Type: "person"}
	b := &model.Node{CanonicalName: "Bob", Type: "person"}
	if err := store.InsertNode(ctx, pool, a); err != nil {
		t.Fatalf("insert a: %v", err)
	}
	if err := store.InsertNode(ctx, pool, b); err != nil {
		t.Fatalf("insert b: %v", err)
	}
	if err := store.InsertEdge(ctx, pool, &model.Edge{
		FromID: a.ID, ToID: b.ID, Type: "knows", Why: "colleagues", SourceURI: "doc://x", Author: "op",
	}); err != nil {
		t.Fatalf("insert edge: %v", err)
	}
	if _, err := c.IngestText(ctx, "doc://x", "Alice knows Bob from the payments team roster.", ""); err != nil {
		t.Fatalf("ingest chunk: %v", err)
	}

	// Erase node A → its edge (FK) and the node go; B survives.
	counts, err := c.EraseNode(ctx, a.ID)
	if err != nil {
		t.Fatalf("erase node: %v", err)
	}
	if counts.Nodes != 1 || counts.Edges != 1 {
		t.Fatalf("erase node counts = %+v, want 1 node + 1 edge", counts)
	}
	if n, _ := store.GetNodeByID(ctx, pool, a.ID); n != nil {
		t.Fatalf("node A still present after erase")
	}
	if n, _ := store.GetNodeByID(ctx, pool, b.ID); n == nil {
		t.Fatalf("node B wrongly deleted")
	}

	// Erase the source → the chunk goes (the edge was already erased above).
	sc, err := c.EraseSource(ctx, "doc://x")
	if err != nil {
		t.Fatalf("erase source: %v", err)
	}
	if sc.Chunks != 1 {
		t.Fatalf("erase source chunks = %d, want 1", sc.Chunks)
	}

	// Wall checks: a principal cannot erase outside its namespace, and EraseSource
	// is operator-only.
	g := &model.Node{CanonicalName: "Global", Type: "person"}
	if err := store.InsertNode(ctx, pool, g); err != nil {
		t.Fatalf("insert global: %v", err)
	}
	p := &Principal{Name: "p", Read: []string{"proj"}, Write: "proj"}
	if _, err := c.EraseNode(ctxAs(p), g.ID); !errors.Is(err, ErrForbiddenNamespace) {
		t.Fatalf("cross-namespace erase should be forbidden, got %v", err)
	}
	if _, err := c.EraseSource(ctxAs(p), "doc://x"); !errors.Is(err, ErrForbiddenNamespace) {
		t.Fatalf("EraseSource under a principal should be forbidden, got %v", err)
	}
}
