package store

import (
	"context"

	"github.com/pgvector/pgvector-go"

	"github.com/programmism/brainiac/internal/model"
)

// nodeCols is the shared node column list (without embedding).
const nodeCols = "id, canonical_name, aliases, type, status, created_at, last_confirmed_at"

// rowScanner is satisfied by both pgx.Row and pgx.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanNode reads the nodeCols projection into a model.Node.
func scanNode(s rowScanner) (model.Node, error) {
	var (
		n      model.Node
		typ    *string
		status string
	)
	if err := s.Scan(&n.ID, &n.CanonicalName, &n.Aliases, &typ, &status, &n.CreatedAt, &n.LastConfirmedAt); err != nil {
		return n, err
	}
	if typ != nil {
		n.Type = *typ
	}
	n.Status = model.Status(status)
	return n, nil
}

// UpdateNodeAliases replaces a node's alias list.
func UpdateNodeAliases(ctx context.Context, db DBTX, id string, aliases []string) error {
	if aliases == nil {
		aliases = []string{}
	}
	_, err := db.Exec(ctx, `UPDATE nodes SET aliases = $2 WHERE id = $1`, id, aliases)
	return err
}

// FindNodesByNormalizedName returns current nodes whose name matches after
// lowercasing and stripping non-alphanumerics ("Order Service" == "OrderService"),
// excluding the exact-name match (handled separately).
func FindNodesByNormalizedName(ctx context.Context, db DBTX, name string) ([]model.Node, error) {
	rows, err := db.Query(ctx, `
		SELECT `+nodeCols+`
		FROM nodes
		WHERE status = 'current'
		  AND regexp_replace(lower(canonical_name), '[^a-z0-9]', '', 'g') = regexp_replace(lower($1), '[^a-z0-9]', '', 'g')
		  AND canonical_name <> $1`, name)
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

// NodeHit is a node plus its cosine distance to a query embedding.
type NodeHit struct {
	Node     model.Node
	Distance float64
}

// FindSimilarNodes returns the k current nodes whose summary_embedding is
// nearest to emb by cosine distance.
func FindSimilarNodes(ctx context.Context, db DBTX, emb []float32, k int) ([]NodeHit, error) {
	vec := pgvector.NewHalfVector(emb).String()
	rows, err := db.Query(ctx, `
		SELECT `+nodeCols+`, (summary_embedding <=> $1::halfvec)::float8 AS distance
		FROM nodes
		WHERE status = 'current' AND summary_embedding IS NOT NULL
		ORDER BY summary_embedding <=> $1::halfvec
		LIMIT $2`, vec, k)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var hits []NodeHit
	for rows.Next() {
		var (
			h      NodeHit
			typ    *string
			status string
		)
		if err := rows.Scan(&h.Node.ID, &h.Node.CanonicalName, &h.Node.Aliases, &typ, &status,
			&h.Node.CreatedAt, &h.Node.LastConfirmedAt, &h.Distance); err != nil {
			return nil, err
		}
		if typ != nil {
			h.Node.Type = *typ
		}
		h.Node.Status = model.Status(status)
		hits = append(hits, h)
	}
	return hits, rows.Err()
}
