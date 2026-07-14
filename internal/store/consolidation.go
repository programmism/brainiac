package store

import (
	"context"
	"fmt"

	"github.com/programmism/brainiac/internal/model"
)

// normExpr normalizes a name for dedup: lowercase, strip non-alphanumerics.
const normExpr = `regexp_replace(lower(canonical_name), '[^a-z0-9]', '', 'g')`

// ProposeNodeSplits returns ids of current nodes whose current edges contradict —
// the same (from, type) points at two or more different targets. That is a signal
// the node may conflate two entities that should be separated by a discriminator
// (the mirror of merge; #127). Proposal only — a human/agent reviews and routes.
func ProposeNodeSplits(ctx context.Context, db DBTX) ([]string, error) {
	rows, err := db.Query(ctx, `
		SELECT DISTINCT e1.from_id
		FROM edges e1
		WHERE e1.status = 'current' AND EXISTS (
			SELECT 1 FROM edges e2
			WHERE e2.status = 'current' AND e2.from_id = e1.from_id
			  AND e2.type = e1.type AND e2.to_id <> e1.to_id)
		ORDER BY e1.from_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// RepointEdgeEndpoint moves one edge's endpoint from oldID to newID (whichever
// side matched), collision-safe: if a current edge with the resulting
// (from, to, type) already exists, this edge is retired (historical) instead —
// preserving the uniqueness invariant (migration 0003). Returns true if it was
// repointed, false if it was retired as a duplicate. Used by Split (#127).
func RepointEdgeEndpoint(ctx context.Context, db DBTX, edgeID, oldID, newID string) (bool, error) {
	var from, to, typ, status string
	err := db.QueryRow(ctx, `SELECT from_id, to_id, type, status FROM edges WHERE id = $1`, edgeID).Scan(&from, &to, &typ, &status)
	if err != nil {
		return false, err
	}
	if status != string(model.StatusCurrent) {
		return false, fmt.Errorf("edge %s is not current", edgeID)
	}
	newFrom, newTo := from, to
	if from == oldID {
		newFrom = newID
	}
	if to == oldID {
		newTo = newID
	}
	if newFrom == from && newTo == to {
		return false, fmt.Errorf("edge %s does not touch node %s", edgeID, oldID)
	}

	var collides bool
	if err := db.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM edges
			WHERE from_id = $1 AND to_id = $2 AND type = $3 AND status = 'current' AND id <> $4)`,
		newFrom, newTo, typ, edgeID).Scan(&collides); err != nil {
		return false, err
	}
	if collides {
		_, err := db.Exec(ctx, `UPDATE edges SET status = 'historical' WHERE id = $1`, edgeID)
		return false, err
	}
	_, err = db.Exec(ctx, `UPDATE edges SET from_id = $1, to_id = $2 WHERE id = $3`, newFrom, newTo, edgeID)
	return true, err
}

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

// FlagStaleBySource marks current edges "possibly stale, verify" when their
// backing source has changed more recently than we last recorded/confirmed the
// edge: a chunk for the edge's source_uri has source_modified_at newer than the
// edge's last_confirmed_at (or created_at if never confirmed). This is the
// automatic staleness signal specified in SYSTEM.md §8.3 (#147) — it only flags
// for human review (reversible via ConfirmEdge), it never changes the edge's
// meaning. Comparing against last_confirmed_at means a confirmed edge is not
// re-flagged until the source changes again, so it does not loop. Returns how
// many edges were newly flagged.
func FlagStaleBySource(ctx context.Context, db DBTX) (int64, error) {
	tag, err := db.Exec(ctx, `
		UPDATE edges e SET flagged_stale = true
		WHERE e.status = 'current' AND e.flagged_stale = false AND e.source_uri <> ''
		  AND EXISTS (
		    SELECT 1 FROM chunks c
		    WHERE c.source_uri = e.source_uri
		      AND c.source_modified_at IS NOT NULL
		      AND c.source_modified_at > COALESCE(e.last_confirmed_at, e.created_at))`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ProposeNodeMerges returns groups of current nodes that share a normalized
// name AND identity scope (likely duplicates), each group ordered oldest-first.
// Scoping by scope_key means same-named entities in different projects are never
// proposed for merge — Consolidate must respect the identity model (#117/#118).
func ProposeNodeMerges(ctx context.Context, db DBTX) ([][]model.Node, error) {
	rows, err := db.Query(ctx, `
		SELECT `+nodeCols+`, scope_key, `+normExpr+` AS norm
		FROM nodes
		WHERE status = 'current' AND (scope_key, `+normExpr+`) IN (
			SELECT scope_key, `+normExpr+` FROM nodes WHERE status = 'current' GROUP BY scope_key, `+normExpr+` HAVING count(*) > 1
		)
		ORDER BY scope_key, norm, created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var groups [][]model.Node
	var cur []model.Node
	var curKey string
	for rows.Next() {
		var (
			n        model.Node
			typ      *string
			status   string
			disc     []byte
			summary  *string
			scopeKey string
			norm     string
		)
		if err := rows.Scan(&n.ID, &n.CanonicalName, &n.Aliases, &typ, &status, &disc, &summary, &n.CreatedAt, &n.LastConfirmedAt, &scopeKey, &norm); err != nil {
			return nil, err
		}
		if typ != nil {
			n.Type = *typ
		}
		n.Summary = deref(summary)
		n.Status = model.Status(status)
		n.Discriminators = decodeDiscriminators(disc)
		// Group boundary is (scope, normalized name); the NUL separator can't
		// occur in either, so distinct pairs never collide.
		key := scopeKey + "\x00" + norm
		if key != curKey && len(cur) > 0 {
			groups = append(groups, cur)
			cur = nil
		}
		curKey = key
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
	EdgeA  string // edge id from FromID → ToA
	EdgeB  string // edge id from FromID → ToB
}

// FindConflicts surfaces current edges from the same node with the same type but
// different targets (e.g. writes_to Kafka vs writes_to RabbitMQ).
func FindConflicts(ctx context.Context, db DBTX) ([]ConflictRow, error) {
	rows, err := db.Query(ctx, `
		SELECT e1.from_id, e1.type, e1.to_id, e2.to_id, e1.id, e2.id
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
		if err := rows.Scan(&c.FromID, &c.Type, &c.ToA, &c.ToB, &c.EdgeA, &c.EdgeB); err != nil {
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
