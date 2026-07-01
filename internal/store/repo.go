package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"github.com/programmism/brainiac/internal/model"
)

// DBTX is the subset of pgx used by the repository functions. Both
// *pgxpool.Pool and pgx.Tx satisfy it, so every function works inside or
// outside a transaction.
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// WithTx runs fn inside a transaction, committing on success and rolling back
// on error. This is how node+edge+chunk+provenance land atomically (§3.2).
func WithTx(ctx context.Context, pool *pgxpool.Pool, fn func(DBTX) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// InsertChunk inserts a chunk and fills in its generated ID and CreatedAt. The
// embedding is sent as text and cast to halfvec, avoiding a type registration
// that would fail before the vector extension exists.
func InsertChunk(ctx context.Context, db DBTX, c *model.Chunk) error {
	locator, err := marshalJSON(c.SourceLocator)
	if err != nil {
		return err
	}
	tier := c.Tier
	if tier == "" {
		tier = model.TierHot
	}
	return db.QueryRow(ctx, `
		INSERT INTO chunks (text, embedding, source_uri, source_locator, quality_score, tier, content_hash, source_modified_at)
		VALUES ($1, $2::halfvec, $3, $4::jsonb, $5::real, $6, $7, $8)
		RETURNING id, created_at`,
		c.Text, encodeVec(c.Embedding), c.SourceURI, locator, c.QualityScore,
		string(tier), nullStr(c.ContentHash), c.SourceModifiedAt,
	).Scan(&c.ID, &c.CreatedAt)
}

// ChunkExistsByHash reports whether a chunk with the given content hash is
// already stored — used for dedup and change detection during ingest.
func ChunkExistsByHash(ctx context.Context, db DBTX, hash string) (bool, error) {
	var exists bool
	err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM chunks WHERE content_hash = $1)`, hash).Scan(&exists)
	return exists, err
}

// SearchChunks returns the k nearest hot-tier chunks to embedding by cosine
// distance, with provenance.
func SearchChunks(ctx context.Context, db DBTX, embedding []float32, k int) ([]model.ChunkHit, error) {
	vec := pgvector.NewHalfVector(embedding).String()
	rows, err := db.Query(ctx, `
		SELECT id, text, source_uri, source_locator, quality_score::float8, tier,
		       content_hash, created_at, source_modified_at,
		       (embedding <=> $1::halfvec)::float8 AS distance
		FROM chunks
		WHERE tier = 'hot' AND embedding IS NOT NULL
		ORDER BY embedding <=> $1::halfvec
		LIMIT $2`, vec, k)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var hits []model.ChunkHit
	for rows.Next() {
		var (
			h           model.ChunkHit
			locator     []byte
			tier        string
			contentHash *string
		)
		if err := rows.Scan(&h.ID, &h.Text, &h.SourceURI, &locator, &h.QualityScore, &tier,
			&contentHash, &h.CreatedAt, &h.SourceModifiedAt, &h.Distance); err != nil {
			return nil, err
		}
		h.Tier = model.Tier(tier)
		if contentHash != nil {
			h.ContentHash = *contentHash
		}
		if len(locator) > 0 {
			if err := json.Unmarshal(locator, &h.SourceLocator); err != nil {
				return nil, err
			}
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// InsertNode inserts a node and fills in its generated ID and CreatedAt.
func InsertNode(ctx context.Context, db DBTX, n *model.Node) error {
	status := n.Status
	if status == "" {
		status = model.StatusCurrent
	}
	aliases := n.Aliases
	if aliases == nil {
		aliases = []string{}
	}
	return db.QueryRow(ctx, `
		INSERT INTO nodes (canonical_name, aliases, type, summary_embedding, status)
		VALUES ($1, $2, $3, $4::halfvec, $5)
		RETURNING id, created_at`,
		n.CanonicalName, aliases, nullStr(n.Type), encodeVec(n.SummaryEmbedding), string(status),
	).Scan(&n.ID, &n.CreatedAt)
}

// GetNodeByCanonicalName returns the most recent current node with the given
// name, or (nil, nil) if none exists.
func GetNodeByCanonicalName(ctx context.Context, db DBTX, name string) (*model.Node, error) {
	n, err := scanNode(db.QueryRow(ctx, `
		SELECT `+nodeCols+`
		FROM nodes
		WHERE canonical_name = $1 AND status = 'current'
		ORDER BY created_at DESC
		LIMIT 1`, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &n, nil
}

// InsertEdge inserts an edge and fills in its generated ID and CreatedAt.
func InsertEdge(ctx context.Context, db DBTX, e *model.Edge) error {
	locator, err := marshalJSON(e.SourceLocator)
	if err != nil {
		return err
	}
	status := e.Status
	if status == "" {
		status = model.StatusCurrent
	}
	// Upsert: a repeated link for the same current (from,to,type) refreshes the
	// rationale/provenance instead of creating a duplicate edge (§11, #71).
	return db.QueryRow(ctx, `
		INSERT INTO edges (from_id, to_id, type, why, source_uri, source_locator, author, status)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8)
		ON CONFLICT (from_id, to_id, type) WHERE status = 'current'
		DO UPDATE SET why = EXCLUDED.why, source_uri = EXCLUDED.source_uri,
		              source_locator = EXCLUDED.source_locator, author = EXCLUDED.author,
		              last_confirmed_at = now()
		RETURNING id, created_at`,
		e.FromID, e.ToID, e.Type, nullStr(e.Why), nullStr(e.SourceURI), locator, nullStr(e.Author), string(status),
	).Scan(&e.ID, &e.CreatedAt)
}

// ListEdgesFrom returns the current edges originating at fromID, oldest first.
func ListEdgesFrom(ctx context.Context, db DBTX, fromID string) ([]model.Edge, error) {
	rows, err := db.Query(ctx, `
		SELECT `+edgeCols+`
		FROM edges
		WHERE from_id = $1 AND status = 'current'
		ORDER BY created_at`, fromID)
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

// --- helpers ---

// encodeVec returns a text-encoded halfvec (to be cast with ::halfvec), or nil
// for a NULL embedding.
func encodeVec(v []float32) any {
	if v == nil {
		return nil
	}
	return pgvector.NewHalfVector(v).String()
}

// marshalJSON turns a locator map into a jsonb-castable string, or nil.
func marshalJSON(m map[string]any) (any, error) {
	if m == nil {
		return nil, nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

// nullStr maps an empty string to a SQL NULL.
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
