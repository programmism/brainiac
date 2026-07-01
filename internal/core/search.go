package core

import (
	"context"
	"errors"
	"fmt"

	"github.com/programmism/brainiac/internal/model"
	"github.com/programmism/brainiac/internal/store"
)

// ErrEmbed marks a failure to embed (e.g. Ollama down), so clients can map it to
// 503 rather than a generic 500 (#77).
var ErrEmbed = errors.New("embedder unavailable")

// DefaultSearchK is used when a caller does not specify k.
const DefaultSearchK = 10

// MaxRelevantDistance is the cosine-distance cutoff above which a hit is treated
// as irrelevant and dropped. Without it, retrieval returns the k nearest chunks
// even for an off-topic query, feeding the client confidently-cited garbage
// (#70). Tunable against the eval harness (#29); a larger value is more lenient.
const MaxRelevantDistance = 0.75

// Search embeds the query and returns hot-tier chunks within MaxRelevantDistance,
// nearest first (§10 step 1). It is the vector half of retrieval.
func (c *Core) Search(ctx context.Context, query string, k int) ([]model.ChunkHit, error) {
	if k <= 0 {
		k = DefaultSearchK
	}
	emb, err := c.embedder.Embed(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEmbed, err)
	}
	hits, err := store.SearchChunks(ctx, c.pool, emb, k)
	if err != nil {
		return nil, err
	}
	return filterByDistance(hits), nil
}

// filterByDistance drops chunk hits beyond the relevance cutoff (results are
// already sorted nearest-first, so it keeps a prefix).
func filterByDistance(hits []model.ChunkHit) []model.ChunkHit {
	for i, h := range hits {
		if h.Distance > MaxRelevantDistance {
			return hits[:i]
		}
	}
	return hits
}
