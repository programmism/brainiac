package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/programmism/brainiac/internal/model"
)

// edgeCols is the shared edge column list.
const edgeCols = "id, from_id, to_id, type, why, source_uri, source_locator, author, status, flagged_stale, created_at, last_confirmed_at, trust"

// scanEdge reads the edgeCols projection into a model.Edge.
func scanEdge(s rowScanner) (model.Edge, error) {
	var (
		e                      model.Edge
		why, sourceURI, author *string
		locator                []byte
		status                 string
	)
	if err := s.Scan(&e.ID, &e.FromID, &e.ToID, &e.Type, &why, &sourceURI, &locator,
		&author, &status, &e.FlaggedStale, &e.CreatedAt, &e.LastConfirmedAt, &e.Trust); err != nil {
		return e, err
	}
	e.Why = deref(why)
	dec, derr := decryptText(e.Why)
	if derr != nil {
		return e, derr
	}
	e.Why = dec
	e.SourceURI = deref(sourceURI)
	e.Author = deref(author)
	e.Status = model.Status(status)
	if len(locator) > 0 {
		if err := json.Unmarshal(locator, &e.SourceLocator); err != nil {
			return e, err
		}
	}
	return e, nil
}

// NodeNamesByIDs resolves many node ids to canonical names in one query — the
// batch replacement for per-edge-endpoint GetNodeByID lookups on the recall hot
// path (#221). Missing ids are simply absent from the map.
func NodeNamesByIDs(ctx context.Context, db DBTX, ids []string) (map[string]string, error) {
	out := make(map[string]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := db.Query(ctx, `SELECT id, canonical_name FROM nodes WHERE id = ANY($1::uuid[])`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}

// GetNodeByID returns the node with the given id, or (nil, nil) if none.
func GetNodeByID(ctx context.Context, db DBTX, id string) (*model.Node, error) {
	n, err := scanNode(db.QueryRow(ctx, `SELECT `+nodeCols+` FROM nodes WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &n, nil
}

// UpdateNodeStatus sets a node's status (e.g. to historical on supersession). It
// also stamps superseded_at = now() on the flip to historical (cleared on a flip
// back to current), so the node's valid-time window is recorded for as-of queries
// (#200) — covering every path that retires a node (supersede, merge, split).
func UpdateNodeStatus(ctx context.Context, db DBTX, id string, status model.Status) error {
	_, err := db.Exec(ctx, `
		UPDATE nodes
		SET status = $2,
		    superseded_at = CASE WHEN $2 = 'historical' THEN now() ELSE NULL END
		WHERE id = $1`, id, string(status))
	return err
}

// UpdateNodeRollup sets a node's "current state of X" rollup text; returns how
// many rows changed so a caller can detect a missing node id (#198).
func UpdateNodeRollup(ctx context.Context, db DBTX, id, rollup string) (int64, error) {
	tag, err := db.Exec(ctx, `UPDATE nodes SET rollup = $2 WHERE id = $1`, id, nullStr(rollup))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// UpdateEdgeStatus sets an edge's status (e.g. to historical when retiring the
// losing side of a conflict — the edge-level mirror of node supersession, #148).
// Returns how many rows changed so callers can detect a missing edge id.
func UpdateEdgeStatus(ctx context.Context, db DBTX, id string, status model.Status) (int64, error) {
	tag, err := db.Exec(ctx, `
		UPDATE edges
		SET status = $2,
		    superseded_at = CASE WHEN $2 = 'historical' THEN now() ELSE NULL END
		WHERE id = $1`, id, string(status))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// EdgesForNode returns up to limit edges touching nodeID in either direction,
// most recent first. With includeHistorical, superseded edges are included (for
// the "why we changed our minds" history, §10, §11.2). The limit bounds recall
// on hub nodes (#73).
func EdgesForNode(ctx context.Context, db DBTX, nodeID string, includeHistorical bool, limit int, wall Wall) ([]model.Edge, error) {
	q := `SELECT ` + edgeCols + ` FROM edges e WHERE (from_id = $1 OR to_id = $1)`
	if !includeHistorical {
		q += ` AND status = 'current'`
	}
	q += ` AND ` + edgeEndpointsClause(3) + ` ORDER BY created_at DESC LIMIT $2`

	rows, err := db.Query(ctx, q, nodeID, limit, wall.arg())
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

// EdgesForNodeAsOf returns the edges touching nodeID that were live at the given
// instant — created by then and not yet superseded (#200). A row already
// historical before valid-time was recorded (superseded_at NULL) is excluded: its
// window is unknown, so it can't be placed in time. Same both-endpoints wall as
// the live read.
func EdgesForNodeAsOf(ctx context.Context, db DBTX, nodeID string, asOf time.Time, limit int, wall Wall) ([]model.Edge, error) {
	rows, err := db.Query(ctx, `
		SELECT `+edgeCols+` FROM edges e
		WHERE (from_id = $1 OR to_id = $1)
		  AND created_at <= $2
		  AND (superseded_at IS NULL OR superseded_at > $2)
		  AND NOT (status = 'historical' AND superseded_at IS NULL)
		  AND `+edgeEndpointsClause(4)+`
		ORDER BY created_at DESC LIMIT $3`, nodeID, asOf, limit, wall.arg())
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

// GraphSnapshot returns up to limit current nodes and the current edges among
// them, for visualization.
func GraphSnapshot(ctx context.Context, db DBTX, limit int, wall Wall) ([]model.Node, []model.Edge, error) {
	nrows, err := db.Query(ctx, `SELECT `+nodeCols+` FROM nodes WHERE status = 'current' AND `+projectClause(2)+` ORDER BY created_at LIMIT $1`, limit, wall.arg())
	if err != nil {
		return nil, nil, err
	}
	defer nrows.Close()
	var nodes []model.Node
	for nrows.Next() {
		n, err := scanNode(nrows)
		if err != nil {
			return nil, nil, err
		}
		nodes = append(nodes, n)
	}
	if err := nrows.Err(); err != nil {
		return nil, nil, err
	}

	erows, err := db.Query(ctx, `SELECT `+edgeCols+` FROM edges e WHERE status = 'current' AND `+edgeEndpointsClause(2)+` ORDER BY created_at LIMIT $1`, limit*4, wall.arg())
	if err != nil {
		return nil, nil, err
	}
	defer erows.Close()
	var edges []model.Edge
	for erows.Next() {
		e, err := scanEdge(erows)
		if err != nil {
			return nil, nil, err
		}
		edges = append(edges, e)
	}
	return nodes, edges, erows.Err()
}

// GetChunksBySourceURI returns up to limit chunks sharing a source URI — the raw
// text behind an edge's provenance (§10 step 3).
func GetChunksBySourceURI(ctx context.Context, db DBTX, uri string, limit int, wall Wall) ([]model.Chunk, error) {
	rows, err := db.Query(ctx, `
		SELECT id, text, source_uri, source_locator, quality_score::float8, tier, content_hash, created_at, source_modified_at
		FROM chunks
		WHERE source_uri = $1 AND tier = 'hot'
		  AND `+projectClause(3)+`
		ORDER BY created_at
		LIMIT $2`, uri, limit, wall.arg())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var chunks []model.Chunk
	for rows.Next() {
		var (
			c           model.Chunk
			locator     []byte
			tier        string
			contentHash *string
		)
		if err := rows.Scan(&c.ID, &c.Text, &c.SourceURI, &locator, &c.QualityScore, &tier,
			&contentHash, &c.CreatedAt, &c.SourceModifiedAt); err != nil {
			return nil, err
		}
		if err := decryptInto(&c.Text); err != nil {
			return nil, err
		}
		c.Tier = model.Tier(tier)
		if contentHash != nil {
			c.ContentHash = *contentHash
		}
		if len(locator) > 0 {
			if err := json.Unmarshal(locator, &c.SourceLocator); err != nil {
				return nil, err
			}
		}
		chunks = append(chunks, c)
	}
	return chunks, rows.Err()
}
