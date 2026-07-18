package core

import (
	"context"
	"fmt"

	"github.com/programmism/brainiac/internal/store"
)

// EraseNode hard-deletes a node and all its edges (GDPR right-to-erasure, #272) —
// a real delete, unlike supersede/merge which keep the rows as history. It is
// wall-checked: under a principal, only a node in the caller's own namespace may
// be erased (assertNodeWritable, #265). Audited. The edge + node deletes run in
// one transaction so a failure never leaves a dangling edge.
func (c *Core) EraseNode(ctx context.Context, id string) (store.DeleteCounts, error) {
	if id == "" {
		return store.DeleteCounts{}, fmt.Errorf("erase: node id is required")
	}
	var counts store.DeleteCounts
	err := store.WithTx(ctx, c.pool, func(db store.DBTX) error {
		if _, err := c.assertNodeWritable(ctx, db, id); err != nil {
			return err
		}
		var err error
		counts, err = store.EraseNode(ctx, db, id)
		return err
	})
	if err != nil {
		return counts, err
	}
	c.audit(ctx, "erase_node", id, "")
	return counts, nil
}

// EraseSource hard-deletes every chunk and edge from a source_uri (#272) — the
// document-granularity erasure path. It is an operator action (rejected under a
// principal), since a source's rows can span namespaces and edges are not
// namespace-scoped; scoping that safely is out of the Layer-2 wall's reach.
// Audited.
func (c *Core) EraseSource(ctx context.Context, sourceURI string) (store.DeleteCounts, error) {
	if sourceURI == "" {
		return store.DeleteCounts{}, fmt.Errorf("erase: source_uri is required")
	}
	if PrincipalFrom(ctx) != nil {
		return store.DeleteCounts{}, fmt.Errorf("%w: erase by source is an operator action", ErrForbiddenNamespace)
	}
	var counts store.DeleteCounts
	err := store.WithTx(ctx, c.pool, func(db store.DBTX) error {
		var err error
		counts, err = store.EraseBySourceURI(ctx, db, sourceURI)
		return err
	})
	if err != nil {
		return counts, err
	}
	c.audit(ctx, "erase_source", sourceURI, "")
	return counts, nil
}
