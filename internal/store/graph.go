package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/programmism/brainiac/internal/model"
)

// edgeCols is the shared edge column list.
const edgeCols = "id, from_id, to_id, type, why, source_uri, source_locator, author, status, created_at, last_confirmed_at"

// scanEdge reads the edgeCols projection into a model.Edge.
func scanEdge(s rowScanner) (model.Edge, error) {
	var (
		e                      model.Edge
		why, sourceURI, author *string
		locator                []byte
		status                 string
	)
	if err := s.Scan(&e.ID, &e.FromID, &e.ToID, &e.Type, &why, &sourceURI, &locator,
		&author, &status, &e.CreatedAt, &e.LastConfirmedAt); err != nil {
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

// EdgesForNode returns edges touching nodeID in either direction, oldest first.
// With includeHistorical, superseded edges are included (for the "why we changed
// our minds" history, §10, §11.2).
func EdgesForNode(ctx context.Context, db DBTX, nodeID string, includeHistorical bool) ([]model.Edge, error) {
	q := `SELECT ` + edgeCols + ` FROM edges WHERE (from_id = $1 OR to_id = $1)`
	if !includeHistorical {
		q += ` AND status = 'current'`
	}
	q += ` ORDER BY created_at`

	rows, err := db.Query(ctx, q, nodeID)
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

// GetChunksBySourceURI returns up to limit chunks sharing a source URI — the raw
// text behind an edge's provenance (§10 step 3).
func GetChunksBySourceURI(ctx context.Context, db DBTX, uri string, limit int) ([]model.Chunk, error) {
	rows, err := db.Query(ctx, `
		SELECT id, text, source_uri, source_locator, quality_score::float8, tier, content_hash, created_at, source_modified_at
		FROM chunks
		WHERE source_uri = $1
		ORDER BY created_at
		LIMIT $2`, uri, limit)
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
