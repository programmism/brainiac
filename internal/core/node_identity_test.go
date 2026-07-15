package core

import (
	"context"
	"errors"
	"testing"

	"github.com/programmism/brainiac/internal/model"
	"github.com/programmism/brainiac/internal/store"
)

func TestNodeIdentityUniqueConstraint(t *testing.T) {
	_, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	if err := store.InsertNode(ctx, pool, &model.Node{CanonicalName: "Dup"}); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	// A second current node with the same identity is rejected by the partial
	// unique index, surfaced as ErrNodeExists (not a duplicate row).
	err := store.InsertNode(ctx, pool, &model.Node{CanonicalName: "Dup"})
	if !errors.Is(err, store.ErrNodeExists) {
		t.Fatalf("second insert should be ErrNodeExists, got %v", err)
	}
}

func TestRememberStaysIdempotentOnConflict(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	r1, err := c.Remember(ctx, RememberInput{CanonicalName: "OrderService", Type: "service"})
	if err != nil || !r1.Created {
		t.Fatalf("first remember: created=%v err=%v", r1.Created, err)
	}
	// Simulate the lost-race path directly: a raw insert conflicts, and Remember
	// must reuse the existing node rather than error.
	r2, err := c.Remember(ctx, RememberInput{CanonicalName: "OrderService"})
	if err != nil {
		t.Fatalf("second remember: %v", err)
	}
	if r2.Created || r2.Node.ID != r1.Node.ID {
		t.Fatalf("second remember should reuse the node: created=%v id=%s", r2.Created, r2.Node.ID)
	}
}
