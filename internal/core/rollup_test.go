package core

import (
	"context"
	"errors"
	"testing"
)

func TestRollupRoundTrip(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	r, err := c.Remember(ctx, RememberInput{CanonicalName: "Hub", Type: "service"})
	if err != nil {
		t.Fatalf("remember: %v", err)
	}
	const state = "Currently writes to Postgres; Kafka path retired 2026-06."
	node, err := c.Rollup(ctx, r.Node.ID, state)
	if err != nil || node.Rollup != state {
		t.Fatalf("rollup: node.Rollup=%q err=%v", node.Rollup, err)
	}
	// It round-trips through get_node.
	det, err := c.GetNode(ctx, r.Node.ID, "", "")
	if err != nil || det == nil || det.Node.Rollup != state {
		t.Fatalf("get_node rollup: %+v err=%v", det, err)
	}
}

func TestRollupPrincipalScoped(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ids := isoFixture(t, c, pool)

	// Principal for A cannot roll up a B node.
	a := &Principal{Name: "a", Read: []string{"A"}, Write: "A"}
	if _, err := c.Rollup(ctxAs(a), ids["Gamma"], "x"); !errors.Is(err, ErrForbiddenNamespace) {
		t.Fatalf("rollup of foreign-namespace node must be forbidden, got %v", err)
	}
	// ...but can roll up its own.
	if _, err := c.Rollup(ctxAs(a), ids["Alpha"], "current state of Alpha"); err != nil {
		t.Fatalf("rollup of own node: %v", err)
	}
}
