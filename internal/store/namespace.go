package store

import "context"

// DeleteCounts reports how many rows a namespace delete removed.
type DeleteCounts struct {
	Edges  int64 `json:"edges"`
	Nodes  int64 `json:"nodes"`
	Chunks int64 `json:"chunks"`
}

// DeleteNamespace removes everything in a project namespace: first every edge
// touching a namespace node (either endpoint — to satisfy the FK before the nodes
// go, and because a half-namespace edge is meaningless once one side is deleted),
// then the nodes, then the chunks. Must run in a transaction so a partial delete
// never leaves dangling edges (#188).
func DeleteNamespace(ctx context.Context, db DBTX, namespace string) (DeleteCounts, error) {
	var c DeleteCounts
	arg := []string{namespace}

	et, err := db.Exec(ctx, `
		DELETE FROM edges e
		WHERE EXISTS (
			SELECT 1 FROM nodes n
			WHERE (n.id = e.from_id OR n.id = e.to_id)
			  AND n.project = ANY($1::text[]))`, arg)
	if err != nil {
		return c, err
	}
	c.Edges = et.RowsAffected()

	nt, err := db.Exec(ctx, `DELETE FROM nodes WHERE project = ANY($1::text[])`, arg)
	if err != nil {
		return c, err
	}
	c.Nodes = nt.RowsAffected()

	ct, err := db.Exec(ctx, `DELETE FROM chunks WHERE project = ANY($1::text[])`, arg)
	if err != nil {
		return c, err
	}
	c.Chunks = ct.RowsAffected()
	return c, nil
}
