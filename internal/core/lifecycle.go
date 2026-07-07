package core

import (
	"context"
	"fmt"

	"github.com/programmism/brainiac/internal/model"
	"github.com/programmism/brainiac/internal/store"
)

// FlagStale marks an edge as possibly stale, for later human review (§11.3).
func (c *Core) FlagStale(ctx context.Context, edgeID string) error {
	return store.FlagStale(ctx, c.pool, edgeID)
}

// Confirm clears an edge's stale flag and refreshes its last_confirmed_at.
func (c *Core) Confirm(ctx context.Context, edgeID string) error {
	return store.ConfirmEdge(ctx, c.pool, edgeID)
}

// RetireEdge marks an edge historical — the edge-level mirror of supersession,
// used to resolve a conflict by keeping the winning edge and retiring the losing
// one (#148). Replacement, not deletion: recall still reaches it via history.
func (c *Core) RetireEdge(ctx context.Context, edgeID string) error {
	if edgeID == "" {
		return fmt.Errorf("retire requires an edge id")
	}
	n, err := store.UpdateEdgeStatus(ctx, c.pool, edgeID, model.StatusHistorical)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("edge %q not found", edgeID)
	}
	return nil
}

// ProposeMerges returns groups of likely-duplicate nodes (by normalized name)
// for the consolidation queue. Nothing is merged — merges are human-approved.
func (c *Core) ProposeMerges(ctx context.Context) ([][]model.Node, error) {
	return store.ProposeNodeMerges(ctx, c.pool)
}
