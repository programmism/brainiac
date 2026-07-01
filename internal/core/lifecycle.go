package core

import (
	"context"

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

// ProposeMerges returns groups of likely-duplicate nodes (by normalized name)
// for the consolidation queue. Nothing is merged — merges are human-approved.
func (c *Core) ProposeMerges(ctx context.Context) ([][]model.Node, error) {
	return store.ProposeNodeMerges(ctx, c.pool)
}
