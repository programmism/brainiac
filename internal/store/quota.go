package store

import "context"

// CountNodes returns how many nodes (any status) fall inside the wall — the live
// row count a per-namespace storage quota is checked against (#186).
func CountNodes(ctx context.Context, db DBTX, wall Wall) (int, error) {
	var n int
	err := db.QueryRow(ctx, `SELECT count(*) FROM nodes WHERE `+projectClause(1), wall.arg()).Scan(&n)
	return n, err
}

// CountChunks returns how many chunks fall inside the wall — the row count a
// per-namespace chunk quota is checked against (#186).
func CountChunks(ctx context.Context, db DBTX, wall Wall) (int, error) {
	var n int
	err := db.QueryRow(ctx, `SELECT count(*) FROM chunks WHERE `+projectClause(1), wall.arg()).Scan(&n)
	return n, err
}
