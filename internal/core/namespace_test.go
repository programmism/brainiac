package core

import (
	"context"
	"errors"
	"testing"

	"github.com/programmism/brainiac/internal/model"
	"github.com/programmism/brainiac/internal/store"
)

func nodeInScope(t *testing.T, pool store.DBTX, name, scopeKey string) *model.Node {
	t.Helper()
	n, err := store.GetNodeByCanonicalNameScoped(context.Background(), pool, name, scopeKey)
	if err != nil {
		t.Fatalf("lookup %s@%s: %v", name, scopeKey, err)
	}
	return n
}

func TestDeleteNamespace(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	isoFixture(t, c, pool)
	ctx := context.Background()

	counts, err := c.DeleteNamespace(ctx, "A")
	if err != nil {
		t.Fatalf("delete A: %v", err)
	}
	if counts.Nodes != 2 || counts.Chunks != 1 || counts.Edges < 2 {
		t.Fatalf("unexpected delete counts: %+v", counts)
	}
	// A is gone...
	if nodeInScope(t, pool, "Alpha", "project=A") != nil {
		t.Fatalf("Alpha survived namespace delete")
	}
	// ...B and global are untouched.
	if nodeInScope(t, pool, "Gamma", "project=B") == nil {
		t.Fatalf("delete of A removed B's node")
	}
	if nodeInScope(t, pool, "Shared", "") == nil {
		t.Fatalf("delete of A removed the global node")
	}
}

func TestDeleteNamespacePrincipalScoped(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	isoFixture(t, c, pool)

	a := &Principal{Name: "a", Read: []string{"A"}, Write: "A"}
	// A principal cannot delete a namespace other than its own.
	if _, err := c.DeleteNamespace(ctxAs(a), "B"); !errors.Is(err, ErrForbiddenNamespace) {
		t.Fatalf("principal delete of foreign namespace must be forbidden, got %v", err)
	}
	// It can delete its own.
	if _, err := c.DeleteNamespace(ctxAs(a), "A"); err != nil {
		t.Fatalf("principal delete of own namespace: %v", err)
	}
}

func TestHandoffNamespace(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ids := isoFixture(t, c, pool)
	ctx := context.Background()

	counts, err := c.HandoffNamespace(ctx, "A", "C")
	if err != nil {
		t.Fatalf("handoff A->C: %v", err)
	}
	if counts.Nodes != 2 || counts.Chunks != 1 {
		t.Fatalf("unexpected handoff counts: %+v", counts)
	}
	// A is empty, C now holds the entities.
	if nodeInScope(t, pool, "Alpha", "project=A") != nil {
		t.Fatalf("Alpha still in A after handoff")
	}
	if nodeInScope(t, pool, "Alpha", "project=C") == nil {
		t.Fatalf("Alpha not moved to C")
	}
	// Edges follow their endpoints by id (untouched by the re-scope).
	edges, err := store.EdgesForNode(ctx, pool, ids["Alpha"], false, 10, store.NoWall())
	if err != nil {
		t.Fatalf("edges: %v", err)
	}
	var sawInternal bool
	for _, e := range edges {
		sawInternal = sawInternal || e.Type == "writes_to"
	}
	if !sawInternal {
		t.Fatalf("handoff lost Alpha's edge")
	}
}

func TestHandoffRejectsNonEmptyTargetAndPrincipal(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	isoFixture(t, c, pool)

	// B is non-empty → handoff into it is refused (no silent identity collision).
	if _, err := c.HandoffNamespace(context.Background(), "A", "B"); err == nil {
		t.Fatalf("handoff into a non-empty namespace must error")
	}
	// Handoff is operator-only.
	a := &Principal{Name: "a", Read: []string{"A"}, Write: "A"}
	if _, err := c.HandoffNamespace(ctxAs(a), "A", "C"); !errors.Is(err, ErrForbiddenNamespace) {
		t.Fatalf("principal handoff must be forbidden, got %v", err)
	}
}
