package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/programmism/brainiac/internal/model"
)

// edgeCols is the shared edge column list.
const edgeCols = "id, from_id, to_id, type, why, source_uri, source_locator, author, status, flagged_stale, created_at, last_confirmed_at"

// scanEdge reads the edgeCols projection into a model.Edge.
func scanEdge(s rowScanner) (model.Edge, error) {
	var (
		e                      model.Edge
		why, sourceURI, author *string
		locator                []byte
		status                 string
	)
	if err := s.Scan(&e.ID, &e.FromID, &e.ToID, &e.Type, &why, &sourceURI, &locator,
		&author, &status, &e.FlaggedStale, &e.CreatedAt, &e.LastConfirmedAt); err != nil {
		return e, err
	}
	e.Why = deref(why)
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

// UpdateNodeStatus sets a node's status (e.g. to historical on supersession).
func UpdateNodeStatus(ctx context.Context, db DBTX, id string, status model.Status) error {
	_, err := db.Exec(ctx, `UPDATE nodes SET status = $2 WHERE id = $1`, id, string(status))
	return err
}

// UpdateEdgeStatus sets an edge's status (e.g. to historical when retiring the
// losing side of a conflict — the edge-level mirror of node supersession, #148).
// Returns how many rows changed so callers can detect a missing edge id.
func UpdateEdgeStatus(ctx context.Context, db DBTX, id string, status model.Status) (int64, error) {
	tag, err := db.Exec(ctx, `UPDATE edges SET status = $2 WHERE id = $1`, id, string(status))
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
