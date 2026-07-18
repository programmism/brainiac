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
	disc := c.Discriminators
	if disc == nil {
		disc = map[string]string{}
	}
	discJSON, err := json.Marshal(disc)
	if err != nil {
		return err
	}
	trust := c.Trust
	if trust == "" {
		trust = model.TrustTrusted // matches the column default; core sets it explicitly
	}
	err = db.QueryRow(ctx, `
		INSERT INTO chunks (text, embedding, source_uri, source_locator, quality_score, tier, content_hash, source_modified_at, discriminators, scope_key, trust)
		VALUES ($1, $2::halfvec, $3, $4::jsonb, $5::real, $6, $7, $8, $9::jsonb, $10, $11)
		ON CONFLICT (source_uri, content_hash) WHERE content_hash IS NOT NULL DO NOTHING
		RETURNING id, created_at`,
		c.Text, encodeVec(c.Embedding), c.SourceURI, locator, c.QualityScore,
		string(tier), nullStr(c.ContentHash), c.SourceModifiedAt, discJSON, model.ScopeKey(c.Discriminators), trust,
	).Scan(&c.ID, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// A concurrent ingest already stored this exact (source_uri, content_hash);
		// the chunk exists, so treat the insert as an idempotent no-op (#225).
		return nil
	}
	return err
}

// ChunkExistsByHash reports whether a chunk with the given content hash is
// already stored — used for dedup and change detection during ingest.
func ChunkExistsByHash(ctx context.Context, db DBTX, hash string) (bool, error) {
	var exists bool
	err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM chunks WHERE content_hash = $1)`, hash).Scan(&exists)
	return exists, err
}

// SearchChunks returns the k nearest chunks to embedding by cosine distance, with
// provenance. By default only the hot (HNSW-indexed) tier is searched; includeCold
// also scans cold-tier chunks (#365) — a sequential scan with no vector index, so
// noticeably slower, for the on-demand "search the archive too" path.
func SearchChunks(ctx context.Context, db DBTX, embedding []float32, k int, scope ScopeFilter, wall Wall, includeCold bool) ([]model.ChunkHit, error) {
	vec := pgvector.NewHalfVector(embedding).String()
	rows, err := db.Query(ctx, `
		SELECT id, text, source_uri, source_locator, quality_score::float8, tier,
		       content_hash, created_at, source_modified_at, discriminators, trust,
		       (embedding <=> $1::halfvec)::float8 AS distance
		FROM chunks
		WHERE `+tierPredicate(includeCold)+`embedding IS NOT NULL
		  AND (cardinality($3::text[]) = 0 OR scope_key = ANY($3::text[]))
		  AND `+projectClause(4)+`
		ORDER BY embedding <=> $1::halfvec
		LIMIT $2`, vec, k, scope.arg(), wall.arg())
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
			disc        []byte
		)
		if err := rows.Scan(&h.ID, &h.Text, &h.SourceURI, &locator, &h.QualityScore, &tier,
			&contentHash, &h.CreatedAt, &h.SourceModifiedAt, &disc, &h.Trust, &h.Distance); err != nil {
			return nil, err
		}
		h.Tier = model.Tier(tier)
		h.Scope = model.ScopeLabel(decodeDiscriminators(disc))
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

// tierPredicate returns the SQL restricting a chunk search to the hot tier, or ""
// (no tier restriction — hot + cold) when includeCold is set (#365). It always
// ends with "AND " so it can prefix another condition.
func tierPredicate(includeCold bool) string {
	if includeCold {
		return ""
	}
	return "tier = 'hot' AND "
}

// SearchChunksLexical returns up to k hot-tier chunks whose full-text index
// matches the query, ranked by ts_rank — the lexical/keyword half of hybrid
// retrieval (#211), which catches exact tokens (error codes, IDs, config keys)
// that dense vectors miss. Same scope + wall as the vector path. Distance is left
// 0 (not a cosine distance); fusion ranks by list position.
func SearchChunksLexical(ctx context.Context, db DBTX, query string, k int, scope ScopeFilter, wall Wall, includeCold bool) ([]model.ChunkHit, error) {
	rows, err := db.Query(ctx, `
		SELECT id, text, source_uri, source_locator, quality_score::float8, tier,
		       content_hash, created_at, source_modified_at, discriminators, trust
		FROM chunks
		WHERE `+tierPredicate(includeCold)+`tsv @@ plainto_tsquery('english', $1)
		  AND (cardinality($3::text[]) = 0 OR scope_key = ANY($3::text[]))
		  AND `+projectClause(4)+`
		ORDER BY ts_rank(tsv, plainto_tsquery('english', $1)) DESC
		LIMIT $2`, query, k, scope.arg(), wall.arg())
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
			disc        []byte
		)
		if err := rows.Scan(&h.ID, &h.Text, &h.SourceURI, &locator, &h.QualityScore, &tier,
			&contentHash, &h.CreatedAt, &h.SourceModifiedAt, &disc, &h.Trust); err != nil {
			return nil, err
		}
		h.Tier = model.Tier(tier)
		h.Scope = model.ScopeLabel(decodeDiscriminators(disc))
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

// InsertNode inserts a node and fills in its generated ID and CreatedAt. The
// node's discriminators (identity axes; empty = global) are stored alongside a
// canonical scope_key so identity lookups never have to recompute it (#117).
func InsertNode(ctx context.Context, db DBTX, n *model.Node) error {
	status := n.Status
	if status == "" {
		status = model.StatusCurrent
	}
	aliases := n.Aliases
	if aliases == nil {
		aliases = []string{}
	}
	disc := n.Discriminators
	if disc == nil {
		disc = map[string]string{}
	}
	discJSON, err := json.Marshal(disc)
	if err != nil {
		return err
	}
	err = db.QueryRow(ctx, `
		INSERT INTO nodes (canonical_name, aliases, type, summary, summary_embedding, status, discriminators, scope_key)
		VALUES ($1, $2, $3, $4, $5::halfvec, $6, $7::jsonb, $8)
		ON CONFLICT (scope_key, canonical_name) WHERE status = 'current' DO NOTHING
		RETURNING id, created_at`,
		n.CanonicalName, aliases, nullStr(n.Type), nullStr(n.Summary), encodeVec(n.SummaryEmbedding), string(status),
		discJSON, model.ScopeKey(n.Discriminators),
	).Scan(&n.ID, &n.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// A concurrent writer already created the current node with this identity;
		// the caller re-reads and reuses it, keeping remember/link idempotent (#220).
		return ErrNodeExists
	}
	return err
}

// ErrNodeExists is returned by InsertNode when a current node with the same
// identity (scope_key, canonical_name) already exists — a concurrent create lost
// the race. Callers re-read the existing node instead of failing.
var ErrNodeExists = errors.New("node with this identity already exists")

// UpdateNodeSummary replaces a node's summary text and its derived embedding
// together, so the prose and the vector never drift apart. Used when an entity is
// re-remembered with a description — the backfill path for nodes created before
// summaries were persisted (#181).
func UpdateNodeSummary(ctx context.Context, db DBTX, id, summary string, emb []float32) error {
	_, err := db.Exec(ctx, `UPDATE nodes SET summary = $2, summary_embedding = $3::halfvec WHERE id = $1`,
		id, nullStr(summary), encodeVec(emb))
	return err
}

// GetNodeByCanonicalName returns the most recent current node with the given
// name in the global scope, or (nil, nil) if none exists. Equivalent to
// GetNodeByCanonicalNameScoped with an empty scope.
func GetNodeByCanonicalName(ctx context.Context, db DBTX, name string) (*model.Node, error) {
	return GetNodeByCanonicalNameScoped(ctx, db, name, "")
}

// GetNodeByCanonicalNameScoped returns the most recent current node with the
// given name within the given identity scope, or (nil, nil) if none exists.
func GetNodeByCanonicalNameScoped(ctx context.Context, db DBTX, name, scopeKey string) (*model.Node, error) {
	n, err := scanNode(db.QueryRow(ctx, `
		SELECT `+nodeCols+`
		FROM nodes
		WHERE canonical_name = $1 AND scope_key = $2 AND status = 'current'
		ORDER BY created_at DESC
		LIMIT 1`, name, scopeKey))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &n, nil
}

// GetNodeByNameWalled returns the most recent current node with the given name
// whose project namespace is inside the wall, or (nil, nil) if none. Used by the
// by-name get_node path under a principal, where the caller's own namespace — not
// the global scope — is what a bare name should resolve within (#120).
func GetNodeByNameWalled(ctx context.Context, db DBTX, name string, wall Wall) (*model.Node, error) {
	n, err := scanNode(db.QueryRow(ctx, `
		SELECT `+nodeCols+`
		FROM nodes
		WHERE canonical_name = $1 AND status = 'current'
		  AND `+projectClause(2)+`
		ORDER BY created_at DESC
		LIMIT 1`, name, wall.arg()))
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
	trust := e.Trust
	if trust == "" {
		trust = model.TrustTrusted // chat-captured edges are trusted; extractor sets untrusted
	}
	// Upsert: a repeated link for the same current (from,to,type) refreshes the
	// rationale/provenance instead of creating a duplicate edge (§11, #71).
	return db.QueryRow(ctx, `
		INSERT INTO edges (from_id, to_id, type, why, source_uri, source_locator, author, status, trust)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8, $9)
		ON CONFLICT (from_id, to_id, type) WHERE status = 'current'
		DO UPDATE SET why = EXCLUDED.why, source_uri = EXCLUDED.source_uri,
		              source_locator = EXCLUDED.source_locator, author = EXCLUDED.author,
		              last_confirmed_at = now()
		RETURNING id, created_at`,
		e.FromID, e.ToID, e.Type, nullStr(e.Why), nullStr(e.SourceURI), locator, nullStr(e.Author), string(status), trust,
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
