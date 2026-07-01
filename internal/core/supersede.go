package core

import (
	"context"
	"fmt"

	"github.com/programmism/brainiac/internal/model"
	"github.com/programmism/brainiac/internal/store"
)

// Supersede records that newID replaces oldID: it adds a `supersedes` edge
// (new → old) and marks the old node historical. Replacement, not deletion —
// the "why we changed our minds" is preserved (§11.2). Atomic.
func (c *Core) Supersede(ctx context.Context, oldID, newID, why, author string) error {
	if oldID == "" || newID == "" {
		return fmt.Errorf("supersede requires both old and new node ids")
	}
	if oldID == newID {
		return fmt.Errorf("a node cannot supersede itself")
	}
	return store.WithTx(ctx, c.pool, func(db store.DBTX) error {
		if err := store.InsertEdge(ctx, db, &model.Edge{
			FromID: newID,
			ToID:   oldID,
			Type:   "supersedes",
			Why:    why,
			Author: author,
		}); err != nil {
			return fmt.Errorf("insert supersedes edge: %w", err)
		}
		return store.UpdateNodeStatus(ctx, db, oldID, model.StatusHistorical)
	})
}
