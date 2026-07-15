package core

import (
	"context"
	"fmt"

	"github.com/programmism/brainiac/internal/model"
	"github.com/programmism/brainiac/internal/store"
)

// Rollup records a "current state of X" synthesis on a hub node (#198) — a curated
// summary over its detailed edge history, distinct from the node's identity
// summary. A principal may only roll up a node in its own write namespace. Returns
// the updated node.
func (c *Core) Rollup(ctx context.Context, nodeID, text string) (*model.Node, error) {
	if nodeID == "" {
		return nil, fmt.Errorf("rollup requires a node id")
	}
	node, err := store.GetNodeByID(ctx, c.pool, nodeID)
	if err != nil {
		return nil, fmt.Errorf("lookup node: %w", err)
	}
	if node == nil {
		return nil, fmt.Errorf("no node with id %q", nodeID)
	}
	if p := PrincipalFrom(ctx); p != nil && node.Discriminators["project"] != p.Write {
		return nil, ErrForbiddenNamespace
	}
	if _, err := store.UpdateNodeRollup(ctx, c.pool, nodeID, text); err != nil {
		return nil, fmt.Errorf("update rollup: %w", err)
	}
	node.Rollup = text
	return node, nil
}
