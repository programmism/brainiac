package core

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/programmism/brainiac/internal/model"
	"github.com/programmism/brainiac/internal/plugins"
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
// nearest first (§10 step 1). It is the vector half of retrieval. The project
// scopes the soft retrieval lens (project + global); an empty project spans all
// scopes, preserving cross-project search (#119).
func (c *Core) Search(ctx context.Context, query string, k int, project string) ([]model.ChunkHit, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	emb, err := c.embedQuery(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEmbed, err)
	}
	return c.searchByEmbedding(ctx, emb, k, project)
}

// searchByEmbedding runs the vector search for an already-computed query
// embedding, so a caller that also needs the vector elsewhere (recall) embeds the
// query only once (#221).
func (c *Core) searchByEmbedding(ctx context.Context, emb []float32, k int, project string) ([]model.ChunkHit, error) {
	if k <= 0 {
		k = DefaultSearchK
	}
	scope, wall := c.readScope(ctx, project)
	hits, err := store.SearchChunks(ctx, c.pool, emb, k, scope, wall)
	if err != nil {
		return nil, err
	}
	return filterByDistance(hits), nil
}

// embedQuery embeds a search query, using the embedder's asymmetric query path
// (nomic `search_query:` prefix, #210) when it exposes one, else the plain path.
func (c *Core) embedQuery(ctx context.Context, text string) ([]float32, error) {
	if qe, ok := c.embedder.(plugins.QueryEmbedder); ok {
		return qe.EmbedQuery(ctx, text)
	}
	return c.embedder.Embed(ctx, text)
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
