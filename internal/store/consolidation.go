package store

import (
	"context"

	"github.com/programmism/brainiac/internal/model"
)

// normExpr normalizes a name for dedup: lowercase, strip non-alphanumerics.
const normExpr = `regexp_replace(lower(canonical_name), '[^a-z0-9]', '', 'g')`

// FlagStale marks an edge as possibly stale (for human review).
func FlagStale(ctx context.Context, db DBTX, edgeID string) error {
	_, err := db.Exec(ctx, `UPDATE edges SET flagged_stale = true WHERE id = $1`, edgeID)
	return err
}

// ConfirmEdge clears the stale flag and refreshes last_confirmed_at.
func ConfirmEdge(ctx context.Context, db DBTX, edgeID string) error {
	_, err := db.Exec(ctx, `UPDATE edges SET flagged_stale = false, last_confirmed_at = now() WHERE id = $1`, edgeID)
	return err
}

// ProposeNodeMerges returns groups of current nodes that share a normalized
// name (likely duplicates), each group ordered oldest-first.
func ProposeNodeMerges(ctx context.Context, db DBTX) ([][]model.Node, error) {
	rows, err := db.Query(ctx, `
		SELECT `+nodeCols+`, `+normExpr+` AS norm
		FROM nodes
		WHERE status = 'current' AND `+normExpr+` IN (
			SELECT `+normExpr+` FROM nodes WHERE status = 'current' GROUP BY 1 HAVING count(*) > 1
		)
		ORDER BY norm, created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var groups [][]model.Node
	var cur []model.Node
	var curNorm string
	for rows.Next() {
		var (
			n      model.Node
			typ    *string
			status string
			disc   []byte
			norm   string
		)
		if err := rows.Scan(&n.ID, &n.CanonicalName, &n.Aliases, &typ, &status, &disc, &n.CreatedAt, &n.LastConfirmedAt, &norm); err != nil {
			return nil, err
		}
		if typ != nil {
			n.Type = *typ
		}
		n.Status = model.Status(status)
		n.Discriminators = decodeDiscriminators(disc)
		if norm != curNorm && len(cur) > 0 {
			groups = append(groups, cur)
			cur = nil
		}
		curNorm = norm
		cur = append(cur, n)
	}
	if len(cur) > 0 {
		groups = append(groups, cur)
	}
	return groups, rows.Err()
}

// ConflictRow is a contradiction: the same source and relationship type point at
// two different targets.
type ConflictRow struct {
	FromID string
	Type   string
	ToA    string
	ToB    string
}

// FindConflicts surfaces current edges from the same node with the same type but
// different targets (e.g. writes_to Kafka vs writes_to RabbitMQ).
func FindConflicts(ctx context.Context, db DBTX) ([]ConflictRow, error) {
	rows, err := db.Query(ctx, `
		SELECT e1.from_id, e1.type, e1.to_id, e2.to_id
		FROM edges e1 JOIN edges e2
			ON e1.from_id = e2.from_id AND e1.type = e2.type AND e1.to_id < e2.to_id
		WHERE e1.status = 'current' AND e2.status = 'current'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ConflictRow
	for rows.Next() {
		var c ConflictRow
		if err := rows.Scan(&c.FromID, &c.Type, &c.ToA, &c.ToB); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// FindStaleEdges returns edges currently flagged stale.
func FindStaleEdges(ctx context.Context, db DBTX) ([]model.Edge, error) {
	rows, err := db.Query(ctx, `SELECT `+edgeCols+` FROM edges WHERE flagged_stale = true ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var edges []model.Edge
	for rows.Next() {
		e, err := scanEdge(rows)
		if err != nil {
			return nil, err
		}
		edges = append(edges, e)
	}
	return edges, rows.Err()
}

// RollupCandidate is a node with enough edges to warrant a "current state of X"
// summary (§11.5).
type RollupCandidate struct {
	NodeID    string `json:"node_id"`
	Name      string `json:"name"`
	EdgeCount int    `json:"edge_count"`
}

// FindRollupCandidates returns current nodes with at least minEdges edges.
func FindRollupCandidates(ctx context.Context, db DBTX, minEdges int) ([]RollupCandidate, error) {
	rows, err := db.Query(ctx, `
		SELECT n.id, n.canonical_name, count(e.id) AS cnt
		FROM nodes n JOIN edges e ON (e.from_id = n.id OR e.to_id = n.id) AND e.status = 'current'
		WHERE n.status = 'current'
		GROUP BY n.id, n.canonical_name
		HAVING count(e.id) >= $1
		ORDER BY cnt DESC
		LIMIT 50`, minEdges)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RollupCandidate
	for rows.Next() {
		var rc RollupCandidate
		if err := rows.Scan(&rc.NodeID, &rc.Name, &rc.EdgeCount); err != nil {
			return nil, err
		}
		out = append(out, rc)
	}
	return out, rows.Err()
}

// RepointEdges moves current edges from oldID to newID during a merge. Edges
// that would collide with an existing current (from,to,type) at newID are marked
// historical instead of repointed, so the (from,to,type) uniqueness invariant
// (migration 0003) holds. Historical edges keep their original endpoints.
func RepointEdges(ctx context.Context, db DBTX, oldID, newID string) error {
	// Repoint from_id where it does not collide, then retire the rest.
	if _, err := db.Exec(ctx, `
		UPDATE edges e SET from_id = $2
		WHERE e.from_id = $1 AND e.status = 'current'
		  AND NOT EXISTS (SELECT 1 FROM edges x
		                  WHERE x.from_id = $2 AND x.to_id = e.to_id AND x.type = e.type
		                    AND x.status = 'current' AND x.id <> e.id)`, oldID, newID); err != nil {
		return err
	}
	if _, err := db.Exec(ctx, `UPDATE edges SET status = 'historical' WHERE from_id = $1 AND status = 'current'`, oldID); err != nil {
		return err
	}
	// Same for to_id.
	if _, err := db.Exec(ctx, `
		UPDATE edges e SET to_id = $2
		WHERE e.to_id = $1 AND e.status = 'current'
		  AND NOT EXISTS (SELECT 1 FROM edges x
		                  WHERE x.to_id = $2 AND x.from_id = e.from_id AND x.type = e.type
		                    AND x.status = 'current' AND x.id <> e.id)`, oldID, newID); err != nil {
		return err
	}
	_, err := db.Exec(ctx, `UPDATE edges SET status = 'historical' WHERE to_id = $1 AND status = 'current'`, oldID)
	return err
}
