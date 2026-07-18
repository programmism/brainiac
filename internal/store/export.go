package store

import (
	"context"
	"encoding/json"

	"github.com/programmism/brainiac/internal/model"
)

// ExportNodes returns every node (current AND historical) inside the wall, for a
// namespace backup. Embeddings are omitted — they are recomputable from summary
// text on import, keeping the dump portable and small (§7). Ordered by created_at
// for a stable, diffable export.
func ExportNodes(ctx context.Context, db DBTX, wall Wall) ([]model.Node, error) {
	rows, err := db.Query(ctx, `SELECT `+nodeCols+` FROM nodes WHERE `+projectClause(1)+` ORDER BY created_at`, wall.arg())
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

// ExportEdges returns every edge (current AND historical) both of whose endpoints
// are inside the wall — the same both-endpoints rule the read wall uses, so an
// export never carries a dangling reference to a node outside the namespace.
func ExportEdges(ctx context.Context, db DBTX, wall Wall) ([]model.Edge, error) {
	rows, err := db.Query(ctx, `SELECT `+edgeCols+` FROM edges e WHERE `+edgeEndpointsClause(1)+` ORDER BY created_at`, wall.arg())
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

// ExportChunks returns every chunk inside the wall with its discriminators (so a
// re-import lands it in the same namespace); the embedding is omitted and
// recomputed from the retained raw text on import.
func ExportChunks(ctx context.Context, db DBTX, wall Wall) ([]model.Chunk, error) {
	rows, err := db.Query(ctx, `
		SELECT id, text, source_uri, source_locator, quality_score::float8, tier, content_hash,
		       created_at, source_modified_at, discriminators
		FROM chunks
		WHERE `+projectClause(1)+`
		ORDER BY created_at`, wall.arg())
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
			disc        []byte
		)
		if err := rows.Scan(&c.ID, &c.Text, &c.SourceURI, &locator, &c.QualityScore, &tier,
			&contentHash, &c.CreatedAt, &c.SourceModifiedAt, &disc); err != nil {
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
		c.Discriminators = decodeDiscriminators(disc)
		chunks = append(chunks, c)
	}
	return chunks, rows.Err()
}
