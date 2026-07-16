package core

import (
	"context"
	"fmt"
	"strings"

	"github.com/programmism/brainiac/internal/model"
	"github.com/programmism/brainiac/internal/store"
)

// Recall tuning.
const (
	DefaultRecallChunks   = 8
	DefaultRecallNodes    = 3 // vector node top-k (down from 5): a tighter budget so weakly-similar nodes don't dilute the bundle (recall precision fix)
	maxRecallNodes        = 8 // hard ceiling on admitted nodes (lexical mentions + vector neighbors)
	minMentionLen         = 2 // ignore name/alias mentions shorter than this many alphanumeric chars
	evidenceChunksPerEdge = 3
	maxEdgesPerNode       = 50 // bound hub-node fan-out (#73)
	maxRecallEdges        = 40 // cap the evidence bundle size (#73; tightened so the budget favors the most relevant nodes, traversed first)
	maxRecallEvidence     = 30
)

// Node-relevance cutoffs for vector node hits (recall precision fix). A hit is
// admitted only when it is both absolutely close (<= MaxNodeDistance) and close
// relative to the best hit (<= best + NodeDistanceGap). Grounded in measured
// nomic-embed cosine distances: a query that names a real entity lands it well
// under 0.5 with the next, unrelated node above ~0.55, while a fully off-corpus
// query bottoms out near 0.59 — so a ~0.55 absolute cap plus a 0.10 relative gap
// isolates the real hit and returns nothing for a foreign query. Deliberately
// distinct from the chunk cutoff (search.go MaxRelevantDistance=0.75): node
// summaries are short, so the chunk-tuned leniency would admit noise here.
const (
	MaxNodeDistance = 0.55
	NodeDistanceGap = 0.10
)

// EdgeView is an edge with its endpoint names resolved, for citation.
type EdgeView struct {
	Edge     model.Edge
	FromName string
	ToName   string
}

// RecallResult is the evidence bundle for Claude to synthesize an answer with
// citations (§10). Graph supplies the "why"; vectors supply breadth; every item
// carries provenance.
type RecallResult struct {
	Query          string
	Chunks         []model.ChunkHit // vector evidence
	Nodes          []model.Node     // relevant entities
	Edges          []EdgeView       // rationale + associations (incl. supersedes history)
	EvidenceChunks []model.Chunk    // raw chunks behind the edges, by source_uri
	// Scope is the requested retrieval scope ("global" or "project:NAME"), and
	// ScopeFallback is true when a scoped query found nothing in its project and
	// every returned result is global — i.e. the results don't belong to the
	// requested project (#143).
	Scope         string `json:"scope"`
	ScopeFallback bool   `json:"scope_fallback"`
}

// Recall runs the retrieval flow: vector search + graph traversal + join of the
// raw chunks behind relevant edges. It returns an evidence bundle; the client
// composes the answer and must cite every claim. The project scopes the soft
// retrieval lens (project + global) over both chunks and nodes; an empty project
// spans all scopes (#119).
func (c *Core) Recall(ctx context.Context, query, project string) (*RecallResult, error) {
	query = strings.TrimSpace(query)
	res := &RecallResult{Query: query, Scope: model.ScopeLabel(discFromProject(project))}
	if query == "" {
		return res, nil
	}

	// Embed the query ONCE and reuse the vector for both chunk and node search — a
	// recall previously paid two Ollama round-trips for the same query (#221).
	emb, err := c.embedQuery(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEmbed, err)
	}
	scope, wall := c.readScope(ctx, project)

	// 1. Hybrid search over chunks: dense vector + lexical FTS, fused (scoped to
	//    the lens). Reuses the query embedding computed above (#211, #221).
	chunks, err := c.hybridSearch(ctx, emb, query, DefaultRecallChunks, project)
	if err != nil {
		return nil, err
	}
	res.Chunks = chunks

	// 2. Relevant nodes, same lens, from two paths: lexical name/alias mentions
	//    first (a query that literally names an entity must reach it — names and
	//    aliases are not embedded), then summary-embedding neighbors that pass the
	//    relevance cutoffs. Lexical hits rank highest, so their edges are traversed
	//    first under the budget below.
	names := make(map[string]string)
	seenNode := make(map[string]bool)
	addNode := func(n model.Node) {
		if seenNode[n.ID] || len(res.Nodes) >= maxRecallNodes {
			return
		}
		seenNode[n.ID] = true
		res.Nodes = append(res.Nodes, n)
		names[n.ID] = n.CanonicalName
	}

	// 2a. Lexical mentions — distance-independent, highest priority.
	mentioned, err := store.FindNodesByMention(ctx, c.pool, query, minMentionLen, scope, wall)
	if err != nil {
		return nil, fmt.Errorf("find nodes by mention: %w", err)
	}
	for _, n := range mentioned {
		addNode(n)
	}

	// 2b. Vector neighbors, admitted by an absolute cutoff and a relative gap from
	//     the best hit, so a single strong match isn't diluted by a weakly-similar
	//     tail (recall precision fix). Hits are sorted nearest-first. Reuses the
	//     query embedding computed above.
	nodeHits, err := store.FindSimilarNodes(ctx, c.pool, emb, DefaultRecallNodes, scope, wall)
	if err != nil {
		return nil, fmt.Errorf("find nodes: %w", err)
	}
	for i, nh := range nodeHits {
		if nh.Distance > MaxNodeDistance {
			break // past the absolute cutoff — nothing further qualifies
		}
		if i > 0 && nh.Distance > nodeHits[0].Distance+NodeDistanceGap {
			break // much farther than the best hit — a diluting tail
		}
		addNode(nh.Node)
	}

	// 3. Traverse edges (incl. supersedes history) for the admitted nodes, in
	//    priority order, and join raw chunks by URI — bounded so a hub node can't
	//    flood the evidence bundle (#73). Endpoint names are resolved in one batch
	//    after traversal, not per edge (#221).
	seenEdge := make(map[string]bool)
	seenURI := make(map[string]bool)
	var collected []model.Edge
	for _, n := range res.Nodes {
		if len(collected) >= maxRecallEdges {
			break
		}
		edges, err := store.EdgesForNode(ctx, c.pool, n.ID, true, maxEdgesPerNode, wall)
		if err != nil {
			return nil, fmt.Errorf("traverse edges: %w", err)
		}
		for _, e := range edges {
			if seenEdge[e.ID] || len(collected) >= maxRecallEdges {
				continue
			}
			seenEdge[e.ID] = true
			collected = append(collected, e)
			if e.SourceURI != "" && !seenURI[e.SourceURI] && len(res.EvidenceChunks) < maxRecallEvidence {
				seenURI[e.SourceURI] = true
				evidence, err := store.GetChunksBySourceURI(ctx, c.pool, e.SourceURI, evidenceChunksPerEdge, wall)
				if err != nil {
					return nil, fmt.Errorf("join evidence: %w", err)
				}
				res.EvidenceChunks = append(res.EvidenceChunks, evidence...)
			}
		}
	}

	// Batch-resolve any endpoint names not already known from the admitted nodes.
	var missing []string
	for _, e := range collected {
		if _, ok := names[e.FromID]; !ok {
			missing = append(missing, e.FromID)
		}
		if _, ok := names[e.ToID]; !ok {
			missing = append(missing, e.ToID)
		}
	}
	if len(missing) > 0 {
		resolved, err := store.NodeNamesByIDs(ctx, c.pool, missing)
		if err != nil {
			return nil, fmt.Errorf("resolve edge endpoints: %w", err)
		}
		for id, name := range resolved {
			names[id] = name
		}
	}
	for _, e := range collected {
		res.Edges = append(res.Edges, EdgeView{Edge: e, FromName: names[e.FromID], ToName: names[e.ToID]})
	}

	// A scoped query that surfaced only global results is a silent fallback: the
	// project had no matching content, so what came back isn't the project's (#143).
	if project != "" {
		res.ScopeFallback = onlyGlobal(res)
	}
	return res, nil
}

// onlyGlobal reports whether a non-empty result set is entirely global-scoped.
func onlyGlobal(res *RecallResult) bool {
	if len(res.Chunks)+len(res.Nodes) == 0 {
		return false // nothing came back — not a fallback, just empty
	}
	for _, h := range res.Chunks {
		if h.Scope != "global" {
			return false
		}
	}
	for _, n := range res.Nodes {
		if model.ScopeLabel(n.Discriminators) != "global" {
			return false
		}
	}
	return true
}

// nodeName resolves a node id to its canonical name, caching lookups.
func (c *Core) nodeName(ctx context.Context, cache map[string]string, id string) string {
	if name, ok := cache[id]; ok {
		return name
	}
	node, err := store.GetNodeByID(ctx, c.pool, id)
	if err != nil || node == nil {
		return ""
	}
	cache[id] = node.CanonicalName
	return node.CanonicalName
}
