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
	DefaultRecallNodes    = 5
	evidenceChunksPerEdge = 3
	maxEdgesPerNode       = 50  // bound hub-node fan-out (#73)
	maxRecallEdges        = 100 // cap the evidence bundle size (#73)
	maxRecallEvidence     = 30
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

	// 1. Vector search over chunks (scoped to the lens).
	chunks, err := c.Search(ctx, query, DefaultRecallChunks, project)
	if err != nil {
		return nil, err
	}
	res.Chunks = chunks

	// 2. Relevant nodes by summary-embedding proximity, same lens.
	emb, err := c.embedder.Embed(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEmbed, err)
	}
	nodeHits, err := store.FindSimilarNodes(ctx, c.pool, emb, DefaultRecallNodes, store.LensFor(project))
	if err != nil {
		return nil, fmt.Errorf("find nodes: %w", err)
	}

	names := make(map[string]string)
	relevant := nodeHits[:0]
	for _, nh := range nodeHits {
		if nh.Distance > MaxRelevantDistance {
			continue // drop off-topic nodes (#70)
		}
		relevant = append(relevant, nh)
		res.Nodes = append(res.Nodes, nh.Node)
		names[nh.Node.ID] = nh.Node.CanonicalName
	}
	nodeHits = relevant

	// 3. Traverse edges (incl. supersedes history) and join raw chunks by URI,
	//    bounded so a hub node can't flood the evidence bundle (#73).
	seenEdge := make(map[string]bool)
	seenURI := make(map[string]bool)
	for _, nh := range nodeHits {
		if len(res.Edges) >= maxRecallEdges {
			break
		}
		edges, err := store.EdgesForNode(ctx, c.pool, nh.Node.ID, true, maxEdgesPerNode)
		if err != nil {
			return nil, fmt.Errorf("traverse edges: %w", err)
		}
		for _, e := range edges {
			if seenEdge[e.ID] || len(res.Edges) >= maxRecallEdges {
				continue
			}
			seenEdge[e.ID] = true
			res.Edges = append(res.Edges, EdgeView{
				Edge:     e,
				FromName: c.nodeName(ctx, names, e.FromID),
				ToName:   c.nodeName(ctx, names, e.ToID),
			})
			if e.SourceURI != "" && !seenURI[e.SourceURI] && len(res.EvidenceChunks) < maxRecallEvidence {
				seenURI[e.SourceURI] = true
				evidence, err := store.GetChunksBySourceURI(ctx, c.pool, e.SourceURI, evidenceChunksPerEdge)
				if err != nil {
					return nil, fmt.Errorf("join evidence: %w", err)
				}
				res.EvidenceChunks = append(res.EvidenceChunks, evidence...)
			}
		}
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
