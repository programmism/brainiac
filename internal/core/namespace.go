package core

import (
	"context"
	"fmt"

	"github.com/programmism/brainiac/internal/store"
)

// DeleteNamespace removes an entire namespace — its nodes, edges, and chunks — in
// one transaction (#188). An operator (no principal) may delete any namespace; a
// principal may delete only its own write namespace. Returns the row counts.
func (c *Core) DeleteNamespace(ctx context.Context, namespace string) (store.DeleteCounts, error) {
	var counts store.DeleteCounts
	if namespace == "" {
		return counts, fmt.Errorf("delete requires a project namespace")
	}
	if p := PrincipalFrom(ctx); p != nil && namespace != p.Write {
		return counts, ErrForbiddenNamespace
	}
	err := store.WithTx(ctx, c.pool, func(db store.DBTX) error {
		var err error
		counts, err = store.DeleteNamespace(ctx, db, namespace)
		return err
	})
	return counts, err
}

// HandoffCounts reports how many rows a namespace handoff moved.
type HandoffCounts struct {
	Nodes  int `json:"nodes"`
	Chunks int `json:"chunks"`
}

// HandoffNamespace moves every node and chunk from one project namespace to
// another — a rename / ownership transfer (#188). Edges reference nodes by id, so
// they follow their endpoints untouched. Operator-only (a principal renaming its
// own namespace would lock itself out) and the target must be empty, so a handoff
// never silently collides two same-named entities into one identity.
func (c *Core) HandoffNamespace(ctx context.Context, from, to string) (HandoffCounts, error) {
	var counts HandoffCounts
	if from == "" || to == "" {
		return counts, fmt.Errorf("handoff requires both from and to namespaces")
	}
	if from == to {
		return counts, fmt.Errorf("handoff from and to are the same namespace")
	}
	if PrincipalFrom(ctx) != nil {
		return counts, fmt.Errorf("%w: handoff is an operator action", ErrForbiddenNamespace)
	}

	toWall := store.Namespaces([]string{to})
	if n, err := store.CountNodes(ctx, c.pool, toWall); err != nil {
		return counts, err
	} else if n > 0 {
		return counts, fmt.Errorf("target namespace %q is not empty (%d nodes) — handoff needs an empty target", to, n)
	}
	if n, err := store.CountChunks(ctx, c.pool, toWall); err != nil {
		return counts, err
	} else if n > 0 {
		return counts, fmt.Errorf("target namespace %q is not empty (%d chunks) — handoff needs an empty target", to, n)
	}

	fromWall := store.Namespaces([]string{from})
	err := store.WithTx(ctx, c.pool, func(db store.DBTX) error {
		nodes, err := store.ExportNodes(ctx, db, fromWall)
		if err != nil {
			return err
		}
		for _, n := range nodes {
			if err := store.UpdateNodeScope(ctx, db, n.ID, reproject(n.Discriminators, to)); err != nil {
				return err
			}
		}
		chunks, err := store.ExportChunks(ctx, db, fromWall)
		if err != nil {
			return err
		}
		for _, ch := range chunks {
			if err := store.UpdateChunkScope(ctx, db, ch.ID, reproject(ch.Discriminators, to)); err != nil {
				return err
			}
		}
		counts = HandoffCounts{Nodes: len(nodes), Chunks: len(chunks)}
		return nil
	})
	return counts, err
}

// reproject returns a copy of disc with the project axis set to `to`, preserving
// any other identity axes (env, client, …).
func reproject(disc map[string]string, to string) map[string]string {
	out := map[string]string{"project": to}
	for k, v := range disc {
		if k != "project" {
			out[k] = v
		}
	}
	return out
}
