package core

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

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

// ChunkDistanceGap adds *relative* gating on top of the absolute cutoff (#215): a
// chunk is dropped once it sits more than this far behind the best (nearest) hit,
// even if still under MaxRelevantDistance. A single absolute const doesn't
// calibrate per query — a strong query keeps its tight cluster of good matches,
// while a weak query with only mediocre hits doesn't return a long tail of
// barely-relevant chunks. Mirrors the node gating in recall.go
// (MaxNodeDistance + NodeDistanceGap). Eval-tunable (#29); this const is the
// default, overridable per deployment via WithRetrievalThresholds / config (#332).
const ChunkDistanceGap = 0.15

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
	dense = filterByDistance(dense, c.retrieval.MaxChunkDistance, c.retrieval.ChunkDistanceGap)
	lexical, err := store.SearchChunksLexical(ctx, c.pool, query, pool, scope, wall)
	if err != nil {
		return nil, err
	}
	// Fuse the two arms over the whole pool, collapse near-duplicate chunks so
	// redundancy doesn't eat the k budget (#217), optionally rerank, then cut to k.
	fused := collapseNearDuplicates(rrfFuse(dense, lexical, pool))
	if c.reranker != nil {
		fused, err = c.reranker.Rerank(ctx, query, fused)
		if err != nil {
			return nil, fmt.Errorf("rerank: %w", err)
		}
	}
	if len(fused) > k {
		fused = fused[:k]
	}
	return fused, nil
}

// nearDupJaccard is the word-set overlap above which two chunks are treated as
// near-duplicates and the lower-ranked one is dropped (#217).
const nearDupJaccard = 0.9

// collapseNearDuplicates removes chunks whose text nearly duplicates an
// already-kept, higher-ranked chunk (word-set Jaccard >= nearDupJaccard), so the
// same content mirrored across sources doesn't crowd out diverse evidence. Order
// is preserved.
func collapseNearDuplicates(hits []model.ChunkHit) []model.ChunkHit {
	type keptChunk struct {
		set   map[string]struct{}
		scope string
	}
	kept := make([]model.ChunkHit, 0, len(hits))
	seen := make([]keptChunk, 0, len(hits))
	for _, h := range hits {
		set := wordSet(h.Text)
		dup := false
		for _, ks := range seen {
			// Only within the same scope: identical content in different projects
			// (e.g. alpha vs global) is legitimately distinct provenance (#217/#143).
			if ks.scope == h.Scope && jaccard(set, ks.set) >= nearDupJaccard {
				dup = true
				break
			}
		}
		if !dup {
			kept = append(kept, h)
			seen = append(seen, keptChunk{set: set, scope: h.Scope})
		}
	}
	return kept
}

func wordSet(s string) map[string]struct{} {
	set := make(map[string]struct{})
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	}) {
		set[w] = struct{}{}
	}
	return set
}

func jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1
	}
	inter := 0
	for w := range a {
		if _, ok := b[w]; ok {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
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
	// Freshness prior (#218): a small recency bonus nudges up-to-date content above
	// stale content on near-ties, without overriding a clearly-more-relevant hit.
	for id := range score {
		score[id] += freshnessBonus(rec[id].SourceModifiedAt)
	}
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

// freshnessWeight is the max recency bonus; ~a third of one RRF rank step
// (1/(rrfK+1)), so it only breaks near-ties, never overriding relevance.
const freshnessWeight = 0.006

// freshnessHalfLifeDays: content this old gets half the max bonus.
const freshnessHalfLifeDays = 90.0

// freshnessBonus returns a small recency prior in [0, freshnessWeight]; 0 when the
// source modification time is unknown (#218).
func freshnessBonus(modified *time.Time) float64 {
	if modified == nil {
		return 0
	}
	ageDays := time.Since(*modified).Hours() / 24
	if ageDays < 0 {
		ageDays = 0
	}
	return freshnessWeight * (freshnessHalfLifeDays / (freshnessHalfLifeDays + ageDays))
}

// embedQuery embeds a search query, using the embedder's asymmetric query path
// (nomic `search_query:` prefix, #210) when it exposes one, else the plain path.
func (c *Core) embedQuery(ctx context.Context, text string) ([]float32, error) {
	if qe, ok := c.embedder.(plugins.QueryEmbedder); ok {
		return qe.EmbedQuery(ctx, text)
	}
	return c.embedder.Embed(ctx, text)
}

// filterByDistance drops chunk hits that are either absolutely far (> maxDist) or
// far relative to the best hit (> best + gap), calibrating relevance per query
// (#215). The thresholds are passed in (config-tunable, #332). Results are already
// sorted nearest-first, so the best hit is hits[0] and both gates are monotonic —
// once a hit fails, all later ones do too — so it returns a prefix.
func filterByDistance(hits []model.ChunkHit, maxDist, gap float64) []model.ChunkHit {
	if len(hits) == 0 {
		return hits
	}
	best := hits[0].Distance
	for i, h := range hits {
		if h.Distance > maxDist || h.Distance > best+gap {
			return hits[:i]
		}
	}
	return hits
}
