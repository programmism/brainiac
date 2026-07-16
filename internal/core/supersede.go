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
	err := store.WithTx(ctx, c.pool, func(db store.DBTX) error {
		// Both endpoints must be in the caller's own namespace (#265).
		if _, err := c.assertNodeWritable(ctx, db, oldID); err != nil {
			return err
		}
		if _, err := c.assertNodeWritable(ctx, db, newID); err != nil {
			return err
		}
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
	if err == nil {
		c.audit(ctx, "supersede", newID+" supersedes "+oldID, "")
	}
	return err
}
