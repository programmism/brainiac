package core

import (
	"context"
	"fmt"

	"github.com/programmism/brainiac/internal/model"
	"github.com/programmism/brainiac/internal/store"
)

// DefaultSearchK is used when a caller does not specify k.
const DefaultSearchK = 10

// Search embeds the query and returns the k nearest hot-tier chunks with
// provenance (§10 step 1). It is the vector half of retrieval.
func (c *Core) Search(ctx context.Context, query string, k int) ([]model.ChunkHit, error) {
	if k <= 0 {
		k = DefaultSearchK
	}
	emb, err := c.embedder.Embed(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	return store.SearchChunks(ctx, c.pool, emb, k)
}
