package store

import (
	"context"
	"encoding/json"

	"github.com/pgvector/pgvector-go"

	"github.com/programmism/brainiac/internal/model"
)

// nodeCols is the shared node column list (without embedding).
const nodeCols = "id, canonical_name, aliases, type, status, discriminators, created_at, last_confirmed_at"

// AnyScope is a sentinel scope_key that matches nodes in every identity scope.
// It cannot collide with a real scope_key (those are "k=v" pairs or "" for global).
const AnyScope = "*"

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
		disc   []byte
	)
	if err := s.Scan(&n.ID, &n.CanonicalName, &n.Aliases, &typ, &status, &disc, &n.CreatedAt, &n.LastConfirmedAt); err != nil {
		return n, err
	}
	if typ != nil {
		n.Type = *typ
	}
	n.Status = model.Status(status)
	n.Discriminators = decodeDiscriminators(disc)
	return n, nil
}

// decodeDiscriminators unmarshals the jsonb discriminators column; an empty or
// "{}" value yields nil (global).
func decodeDiscriminators(b []byte) map[string]string {
	if len(b) == 0 {
		return nil
	}
	m := map[string]string{}
	if err := json.Unmarshal(b, &m); err != nil || len(m) == 0 {
		return nil
	}
	return m
}

// UpdateNodeAliases replaces a node's alias list.
func UpdateNodeAliases(ctx context.Context, db DBTX, id string, aliases []string) error {
	if aliases == nil {
		aliases = []string{}
	}
	_, err := db.Exec(ctx, `UPDATE nodes SET aliases = $2 WHERE id = $1`, id, aliases)
	return err
}

// FindNodesByNormalizedName returns current nodes in the given identity scope
// whose name matches after lowercasing and stripping non-alphanumerics
// ("Order Service" == "OrderService"), excluding the exact-name match (handled
// separately). Scoping keeps same-named entities in different projects from being
// flagged as duplicates of each other (#117).
func FindNodesByNormalizedName(ctx context.Context, db DBTX, name, scopeKey string) ([]model.Node, error) {
	rows, err := db.Query(ctx, `
		SELECT `+nodeCols+`
		FROM nodes
		WHERE status = 'current'
		  AND scope_key = $2
		  AND regexp_replace(lower(canonical_name), '[^a-z0-9]', '', 'g') = regexp_replace(lower($1), '[^a-z0-9]', '', 'g')
		  AND canonical_name <> $1`, name, scopeKey)
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
// nearest to emb by cosine distance. When scopeKey is non-empty results are
// restricted to that identity scope; the sentinel AnyScope spans all scopes
// (used by recall, which reads across projects).
func FindSimilarNodes(ctx context.Context, db DBTX, emb []float32, k int, scopeKey string) ([]NodeHit, error) {
	vec := pgvector.NewHalfVector(emb).String()
	rows, err := db.Query(ctx, `
		SELECT `+nodeCols+`, (summary_embedding <=> $1::halfvec)::float8 AS distance
		FROM nodes
		WHERE status = 'current' AND summary_embedding IS NOT NULL
		  AND ($3 = '*' OR scope_key = $3)
		ORDER BY summary_embedding <=> $1::halfvec
		LIMIT $2`, vec, k, scopeKey)
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
			disc   []byte
		)
		if err := rows.Scan(&h.Node.ID, &h.Node.CanonicalName, &h.Node.Aliases, &typ, &status,
			&disc, &h.Node.CreatedAt, &h.Node.LastConfirmedAt, &h.Distance); err != nil {
			return nil, err
		}
		if typ != nil {
			h.Node.Type = *typ
		}
		h.Node.Status = model.Status(status)
		h.Node.Discriminators = decodeDiscriminators(disc)
		hits = append(hits, h)
	}
	return hits, rows.Err()
}
