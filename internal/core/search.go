package core

import (
	"context"
	"errors"
	"fmt"
	"sort"
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
	return c.hybridSearch(ctx, emb, query, k, project)
}

// rrfK is the reciprocal-rank-fusion constant (the standard 60): a larger value
// flattens the contribution of top ranks, blending the two lists more evenly.
const rrfK = 60

// hybridSearch fuses dense vector search with lexical full-text search over chunks
// via reciprocal-rank fusion (#211), so exact-token queries (error codes, IDs,
// config keys) that dense vectors miss are still surfaced. It embeds nothing — the
// caller passes the precomputed query vector (so recall embeds once, #221) and the
// raw query text for the FTS side.
func (c *Core) hybridSearch(ctx context.Context, emb []float32, query string, k int, project string) ([]model.ChunkHit, error) {
	if k <= 0 {
		k = DefaultSearchK
	}
	scope, wall := c.readScope(ctx, project)
	// Over-fetch each arm so fusion has depth to work with.
	pool := k * 4
	if pool < 20 {
		pool = 20
	}
	dense, err := store.SearchChunks(ctx, c.pool, emb, pool, scope, wall)
	if err != nil {
		return nil, err
	}
	dense = filterByDistance(dense)
	lexical, err := store.SearchChunksLexical(ctx, c.pool, query, pool, scope, wall)
	if err != nil {
		return nil, err
	}
	return rrfFuse(dense, lexical, k), nil
}

// rrfFuse merges two ranked chunk lists by reciprocal-rank fusion and returns the
// top k. A chunk in both arms accumulates both contributions; its returned record
// prefers the dense hit (which carries the cosine distance).
func rrfFuse(dense, lexical []model.ChunkHit, k int) []model.ChunkHit {
	score := make(map[string]float64)
	rec := make(map[string]model.ChunkHit)
	order := make([]string, 0, len(dense)+len(lexical))
	add := func(list []model.ChunkHit) {
		for rank, h := range list {
			if _, seen := score[h.ID]; !seen {
				order = append(order, h.ID)
			}
			score[h.ID] += 1.0 / float64(rrfK+rank+1)
			if _, ok := rec[h.ID]; !ok {
				rec[h.ID] = h // first writer wins; dense is added first
			}
		}
	}
	add(dense)
	add(lexical)
	sort.SliceStable(order, func(i, j int) bool { return score[order[i]] > score[order[j]] })
	if len(order) > k {
		order = order[:k]
	}
	out := make([]model.ChunkHit, 0, len(order))
	for _, id := range order {
		out = append(out, rec[id])
	}
	return out
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
