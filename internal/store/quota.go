package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// CountNodes returns how many nodes (any status) fall inside the wall — the live
// row count a per-namespace storage quota is checked against (#186). It is an
// exact count(*); the quota hot path uses NamespaceUsage instead (#229), but this
// remains the source of truth for backfill and reconciliation.
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

// NamespaceUsage returns the maintained node and chunk counts for a single
// project namespace (#229): an O(1) primary-key read of the namespace_usage
// counter, kept in sync with count(*) by in-transaction triggers, rather than a
// count(*) aggregate per write. A namespace with no rows returns (0, 0). Reading
// inside a transaction sees that transaction's own uncommitted inserts (so a link
// that creates two endpoints counts the first against the second's quota).
func NamespaceUsage(ctx context.Context, db DBTX, project string) (nodes, chunks int, err error) {
	err = db.QueryRow(ctx,
		`SELECT nodes, chunks FROM namespace_usage WHERE project = $1`, project).Scan(&nodes, &chunks)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, nil
	}
	return nodes, chunks, err
}
