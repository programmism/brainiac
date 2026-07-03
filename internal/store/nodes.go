package store

import (
	"context"
	"encoding/json"

	"github.com/pgvector/pgvector-go"

	"github.com/programmism/brainiac/internal/model"
)

// nodeCols is the shared node column list (without embedding).
const nodeCols = "id, canonical_name, aliases, type, status, discriminators, created_at, last_confirmed_at"

// ScopeFilter is the set of identity scope_keys a read spans. An empty filter
// spans ALL scopes (no lens); otherwise a row matches if its scope_key is in the
// set. "" in the set means the global scope.
type ScopeFilter []string

// AllScopes spans every identity scope (used by dedup-free reads and the WebUI).
func AllScopes() ScopeFilter { return ScopeFilter{} }

// ExactScope matches a single scope_key (used by dedup, which compares like with like).
func ExactScope(scopeKey string) ScopeFilter { return ScopeFilter{scopeKey} }

// LensFor is the soft retrieval lens for a project: the project's own scope plus
// global. An empty project means no lens (all scopes) — preserving cross-project
// search for callers that don't specify one (#119).
func LensFor(project string) ScopeFilter {
	if project == "" {
		return AllScopes()
	}
	return ScopeFilter{"", model.ScopeKey(map[string]string{"project": project})}
}

// arg returns the filter as a non-nil []string for the SQL predicate
//
//	(cardinality($n::text[]) = 0 OR scope_key = ANY($n::text[]))
//
// where an empty array means "all scopes".
func (f ScopeFilter) arg() []string {
	if f == nil {
		return []string{}
	}
	return f
}

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
func FindNodesByNormalizedName(ctx context.Context, db DBTX, name string, scope ScopeFilter) ([]model.Node, error) {
	rows, err := db.Query(ctx, `
		SELECT `+nodeCols+`
		FROM nodes
		WHERE status = 'current'
		  AND (cardinality($2::text[]) = 0 OR scope_key = ANY($2::text[]))
		  AND regexp_replace(lower(canonical_name), '[^a-z0-9]', '', 'g') = regexp_replace(lower($1), '[^a-z0-9]', '', 'g')
		  AND canonical_name <> $1`, name, scope.arg())
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
// nearest to emb by cosine distance, restricted to the given scope filter (an
// empty filter spans all scopes).
func FindSimilarNodes(ctx context.Context, db DBTX, emb []float32, k int, scope ScopeFilter) ([]NodeHit, error) {
	vec := pgvector.NewHalfVector(emb).String()
	rows, err := db.Query(ctx, `
		SELECT `+nodeCols+`, (summary_embedding <=> $1::halfvec)::float8 AS distance
		FROM nodes
		WHERE status = 'current' AND summary_embedding IS NOT NULL
		  AND (cardinality($3::text[]) = 0 OR scope_key = ANY($3::text[]))
		ORDER BY summary_embedding <=> $1::halfvec
		LIMIT $2`, vec, k, scope.arg())
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
