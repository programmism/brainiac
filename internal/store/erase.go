package store

import "context"

// EraseNode hard-deletes a node and every edge touching it (GDPR right-to-erasure,
// #272) — a real DELETE, not supersession. Edges go first so the from_id/to_id FKs
// (which have no ON DELETE CASCADE) are satisfied. Must run in a transaction so a
// partial erase never leaves a dangling edge.
func EraseNode(ctx context.Context, db DBTX, id string) (DeleteCounts, error) {
	var c DeleteCounts
	et, err := db.Exec(ctx, `DELETE FROM edges WHERE from_id = $1 OR to_id = $1`, id)
	if err != nil {
		return c, err
	}
	c.Edges = et.RowsAffected()
	nt, err := db.Exec(ctx, `DELETE FROM nodes WHERE id = $1`, id)
	if err != nil {
		return c, err
	}
	c.Nodes = nt.RowsAffected()
	return c, nil
}

// EraseBySourceURI hard-deletes every chunk and edge carrying source_uri (#272) —
// e.g. purging everything ingested from one document. Nodes are not source-scoped
// (they are curated identities), so they are left for EraseNode. Runs in the
// caller's transaction.
func EraseBySourceURI(ctx context.Context, db DBTX, uri string) (DeleteCounts, error) {
	var c DeleteCounts
	et, err := db.Exec(ctx, `DELETE FROM edges WHERE source_uri = $1`, uri)
	if err != nil {
		return c, err
	}
	c.Edges = et.RowsAffected()
	ct, err := db.Exec(ctx, `DELETE FROM chunks WHERE source_uri = $1`, uri)
	if err != nil {
		return c, err
	}
	c.Chunks = ct.RowsAffected()
	return c, nil
}
