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
			(SELECT count(*) FROM edges  WHERE flagged_stale = true)`,
	).Scan(&c.ChunksHot, &c.ChunksCold, &c.Nodes, &c.NodesHistorical, &c.Edges, &c.EdgesHistorical, &c.EdgesStale)
	return c, err
}
