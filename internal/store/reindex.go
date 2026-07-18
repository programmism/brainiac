package store

import (
	"context"
	"fmt"
)

// hnswIndex identifies a rebuildable HNSW index and the definition needed to
// recreate it with new build parameters. The historical migrations (#0001, #0009)
// create these with pgvector's defaults (m=16, ef_construction=64); the params
// are baked into CREATE INDEX and can't be read from runtime config, so tuning
// them (#233) means rebuilding the index — that's what ReindexHNSW does.
type hnswIndex struct {
	name, table, column, where string
}

var hnswIndexes = []hnswIndex{
	{"chunks_embedding_hot_idx", "chunks", "embedding", "tier = 'hot'"},
	{"nodes_summary_embedding_idx", "nodes", "summary_embedding", "status = 'current'"},
}

// ReindexHNSW rebuilds the vector indexes with the given HNSW build parameters
// (#233): a higher m / ef_construction builds a denser graph that recalls better
// at 10M+ rows, at the cost of build time and index size.
//
// It rebuilds online — CREATE INDEX CONCURRENTLY a replacement, drop the old,
// rename into place — so search keeps serving on the old index throughout and is
// never left unindexed. CONCURRENTLY cannot run inside a transaction, so db MUST
// be the pool (autocommit), not a WithTx handle.
func ReindexHNSW(ctx context.Context, db DBTX, m, efConstruction int) error {
	for _, ix := range hnswIndexes {
		tmp := ix.name + "_rebuild"
		// Clean up any leftover temp from an interrupted prior run so the retry is
		// idempotent.
		if _, err := db.Exec(ctx, "DROP INDEX IF EXISTS "+tmp); err != nil {
			return fmt.Errorf("reindex %s: drop stale temp: %w", ix.name, err)
		}
		create := fmt.Sprintf(
			"CREATE INDEX CONCURRENTLY %s ON %s USING hnsw (%s halfvec_cosine_ops) WITH (m=%d, ef_construction=%d) WHERE %s",
			tmp, ix.table, ix.column, m, efConstruction, ix.where)
		if _, err := db.Exec(ctx, create); err != nil {
			return fmt.Errorf("reindex %s: build replacement: %w", ix.name, err)
		}
		if _, err := db.Exec(ctx, "DROP INDEX CONCURRENTLY IF EXISTS "+ix.name); err != nil {
			return fmt.Errorf("reindex %s: drop old: %w", ix.name, err)
		}
		if _, err := db.Exec(ctx, fmt.Sprintf("ALTER INDEX %s RENAME TO %s", tmp, ix.name)); err != nil {
			return fmt.Errorf("reindex %s: rename replacement: %w", ix.name, err)
		}
	}
	return nil
}
