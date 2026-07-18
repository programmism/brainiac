package store

import "context"

// Counts holds the raw corpus/graph counts backing the health metrics (§14).
type Counts struct {
	ChunksHot       int `json:"chunks_hot"`
	ChunksCold      int `json:"chunks_cold"`
	Nodes           int `json:"nodes"`
	NodesHistorical int `json:"nodes_historical"`
	Edges           int `json:"edges"`
	EdgesHistorical int `json:"edges_historical"`
	EdgesStale      int `json:"edges_stale"`
	// ReviewQueue is proposed (awaiting-review) nodes + edges — the consolidation
	// queue depth (#319): how much extracted/proposed content is pending approval.
	ReviewQueue int `json:"review_queue"`
}

// IndexSizeBytes returns the on-disk size of the hot-tier HNSW vector index —
// the ★ scaling signal (index vs RAM; SYSTEM.md §9).
func IndexSizeBytes(ctx context.Context, db DBTX) (int64, error) {
	var n int64
	err := db.QueryRow(ctx, `SELECT COALESCE(pg_relation_size('chunks_embedding_hot_idx'), 0)`).Scan(&n)
	return n, err
}

// DBStats holds Postgres-level operational signals for the system view:
// on-disk database size and connection saturation (§9). These are cheap
// catalog reads, not the corpus counts of HealthCounts.
type DBStats struct {
	DatabaseSizeBytes int64 `json:"database_size_bytes"`
	ActiveConnections int   `json:"active_connections"`
	MaxConnections    int   `json:"max_connections"`
}

// DBStatsFor returns the database size and connection usage in one round-trip.
// ActiveConnections counts backends on the current database; MaxConnections is
// the server's `max_connections` setting — together they show how close the
// deployment is to exhausting its connection budget.
func DBStatsFor(ctx context.Context, db DBTX) (DBStats, error) {
	var s DBStats
	err := db.QueryRow(ctx, `
		SELECT
			pg_database_size(current_database()),
			(SELECT count(*) FROM pg_stat_activity WHERE datname = current_database()),
			current_setting('max_connections')::int`,
	).Scan(&s.DatabaseSizeBytes, &s.ActiveConnections, &s.MaxConnections)
	return s, err
}

// HealthCounts returns corpus and graph counts in a single round-trip.
func HealthCounts(ctx context.Context, db DBTX) (Counts, error) {
	var c Counts
	err := db.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM chunks WHERE tier = 'hot'),
			(SELECT count(*) FROM chunks WHERE tier = 'cold'),
			(SELECT count(*) FROM nodes  WHERE status = 'current'),
			(SELECT count(*) FROM nodes  WHERE status = 'historical'),
			(SELECT count(*) FROM edges  WHERE status = 'current'),
			(SELECT count(*) FROM edges  WHERE status = 'historical'),
			(SELECT count(*) FROM edges  WHERE flagged_stale = true),
			(SELECT count(*) FROM nodes  WHERE status = 'proposed')
				+ (SELECT count(*) FROM edges WHERE status = 'proposed')`,
	).Scan(&c.ChunksHot, &c.ChunksCold, &c.Nodes, &c.NodesHistorical, &c.Edges, &c.EdgesHistorical, &c.EdgesStale, &c.ReviewQueue)
	return c, err
}
