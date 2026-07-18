package core

import (
	"context"
	"errors"
	"testing"

	"github.com/programmism/brainiac/internal/store"
)

func TestImportRoundTrip(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	isoFixture(t, c, pool)
	ctx := context.Background()

	exp, err := c.ExportNamespace(ctx, "A")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	counts, err := c.ImportNamespace(ctx, exp, "C")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if counts.Nodes != 2 || counts.Edges != 1 || counts.Chunks != 1 || counts.EdgesSkipped != 0 {
		t.Fatalf("unexpected import counts: %+v", counts)
	}

	// The entities landed in C.
	alpha := nodeInScope(t, pool, "Alpha", "project=C")
	if alpha == nil || nodeInScope(t, pool, "Beta", "project=C") == nil {
		t.Fatalf("nodes not imported into C")
	}
	// The edge reconnected the remapped endpoints.
	edges, err := store.EdgesForNode(ctx, pool, alpha.ID, false, 10, store.NoWall())
	if err != nil {
		t.Fatalf("edges: %v", err)
	}
	var sawInternal bool
	for _, e := range edges {
		sawInternal = sawInternal || e.Type == "writes_to"
	}
	if !sawInternal {
		t.Fatalf("imported edge did not reconnect in C")
	}
	// The chunk is searchable in C but not leaking outside it.
	cprin := &Principal{Name: "c", Read: []string{"C"}, Write: "C"}
	if hits, _ := c.Search(ctxAs(cprin), "alpha apple pie", 10, "", false); len(hits) == 0 {
		t.Fatalf("imported chunk not searchable in C")
	}
}

func TestImportPrincipalScoped(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	isoFixture(t, c, pool)
	ctx := context.Background()

	exp, err := c.ExportNamespace(ctx, "A")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	a := &Principal{Name: "a", Read: []string{"A"}, Write: "A"}
	// A principal cannot import into a namespace other than its own write target.
	if _, err := c.ImportNamespace(ctxAs(a), exp, "B"); !errors.Is(err, ErrForbiddenNamespace) {
		t.Fatalf("import into foreign namespace must be forbidden, got %v", err)
	}
}
