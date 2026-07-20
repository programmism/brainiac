package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/programmism/brainiac/internal/model"
)

// GetNodeByNameScopeStatus returns the most recent node with the given name and
// identity scope in a specific status, or (nil, nil) if none. The extractor uses
// it to dedup within the proposed set: a second chunk mentioning the same entity
// reuses the pending node instead of stacking duplicate proposals.
func GetNodeByNameScopeStatus(ctx context.Context, db DBTX, name, scopeKey string, status model.Status) (*model.Node, error) {
	n, err := scanNode(db.QueryRow(ctx, `
		SELECT `+nodeCols+`
		FROM nodes
		WHERE canonical_name = $1 AND scope_key = $2 AND status = $3
		ORDER BY created_at DESC
		LIMIT 1`, name, scopeKey, string(status)))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &n, nil
}

// ListProposedNodes returns up to limit proposed nodes, oldest first (review in
// arrival order).
func ListProposedNodes(ctx context.Context, db DBTX, limit int) ([]model.Node, error) {
	rows, err := db.Query(ctx, `
		SELECT `+nodeCols+`
		FROM nodes
		WHERE status = 'proposed'
		ORDER BY created_at
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var nodes []model.Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}

// ListProposedNodesInScopes returns the proposed nodes whose identity scope is in
// scopeKeys, oldest first — the working set for cross-document entity resolution
// after a batch applies (#431). An empty scopeKeys returns nothing (no lens = no
// work, rather than the whole proposed set).
func ListProposedNodesInScopes(ctx context.Context, db DBTX, scopeKeys []string) ([]model.Node, error) {
	if len(scopeKeys) == 0 {
		return nil, nil
	}
	rows, err := db.Query(ctx, `
		SELECT `+nodeCols+`
		FROM nodes
		WHERE status = 'proposed' AND scope_key = ANY($1::text[])
		ORDER BY created_at, id`, scopeKeys)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var nodes []model.Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}

// ProposedEdge is a proposed edge with its endpoints' canonical names resolved,
// so a reviewer sees "A depends-on B" instead of two opaque ids.
type ProposedEdge struct {
	model.Edge
	FromName string `json:"from_name"`
	ToName   string `json:"to_name"`
}

// ListProposedEdges returns up to limit proposed edges (endpoints named),
// oldest first.
func ListProposedEdges(ctx context.Context, db DBTX, limit int) ([]ProposedEdge, error) {
	rows, err := db.Query(ctx, `
		SELECT e.id, e.from_id, e.to_id, e.type, e.why, e.source_uri, e.source_locator,
		       e.author, e.status, e.flagged_stale, e.created_at, e.last_confirmed_at, e.trust,
		       nf.canonical_name, nt.canonical_name
		FROM edges e
		JOIN nodes nf ON nf.id = e.from_id
		JOIN nodes nt ON nt.id = e.to_id
		WHERE e.status = 'proposed'
		ORDER BY e.created_at
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var edges []ProposedEdge
	for rows.Next() {
		var pe ProposedEdge
		e, err := scanEdge(rowScannerFunc(func(dest ...any) error {
			// The projection is edgeCols + two name columns; scanEdge reads the
			// first 13, so peel the trailing two names off here.
			return rows.Scan(append(dest, &pe.FromName, &pe.ToName)...)
		}))
		if err != nil {
			return nil, err
		}
		pe.Edge = e
		edges = append(edges, pe)
	}
	return edges, rows.Err()
}

// rowScannerFunc adapts a scan closure to the rowScanner interface, letting
// scanEdge read its columns while ListProposedEdges appends the two extra name
// columns in the same Scan call.
type rowScannerFunc func(dest ...any) error

func (f rowScannerFunc) Scan(dest ...any) error { return f(dest...) }

// PromoteProposedEndpoints flips any still-proposed endpoint of the given edge
// to current, so approving an edge never leaves a live edge pointing at an
// invisible (proposed) node.
func PromoteProposedEndpoints(ctx context.Context, db DBTX, edgeID string) error {
	_, err := db.Exec(ctx, `
		UPDATE nodes SET status = 'current'
		WHERE status = 'proposed' AND id IN (
			SELECT from_id FROM edges WHERE id = $1
			UNION
			SELECT to_id FROM edges WHERE id = $1)`, edgeID)
	return err
}

// CurrentEdgeConflict reports whether a current edge already covers the same
// (from, to, type) as the given proposed edge — in which case approving it would
// violate the one-current-edge-per-triple index (0003), so the caller retires
// the proposal instead of promoting a duplicate.
func CurrentEdgeConflict(ctx context.Context, db DBTX, edgeID string) (bool, error) {
	var exists bool
	err := db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM edges cur
			JOIN edges prop ON prop.id = $1
			WHERE cur.status = 'current' AND cur.id <> prop.id
			  AND cur.from_id = prop.from_id AND cur.to_id = prop.to_id AND cur.type = prop.type)`, edgeID).Scan(&exists)
	return exists, err
}
