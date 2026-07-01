package core

import (
	"context"
	"fmt"

	"github.com/programmism/brainiac/internal/model"
	"github.com/programmism/brainiac/internal/store"
)

// Recall tuning.
const (
	DefaultRecallChunks   = 8
	DefaultRecallNodes    = 5
	evidenceChunksPerEdge = 3
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
}

// Recall runs the retrieval flow: vector search + graph traversal + join of the
// raw chunks behind relevant edges. It returns an evidence bundle; the client
// composes the answer and must cite every claim.
func (c *Core) Recall(ctx context.Context, query string) (*RecallResult, error) {
	res := &RecallResult{Query: query}

	// 1. Vector search over chunks.
	chunks, err := c.Search(ctx, query, DefaultRecallChunks)
	if err != nil {
		return nil, err
	}
	res.Chunks = chunks

	// 2. Relevant nodes by summary-embedding proximity.
	emb, err := c.embedder.Embed(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	nodeHits, err := store.FindSimilarNodes(ctx, c.pool, emb, DefaultRecallNodes)
	if err != nil {
		return nil, fmt.Errorf("find nodes: %w", err)
	}

	names := make(map[string]string)
	for _, nh := range nodeHits {
		res.Nodes = append(res.Nodes, nh.Node)
		names[nh.Node.ID] = nh.Node.CanonicalName
	}

	// 3. Traverse edges (incl. supersedes history) and join raw chunks by URI.
	seenEdge := make(map[string]bool)
	seenURI := make(map[string]bool)
	for _, nh := range nodeHits {
		edges, err := store.EdgesForNode(ctx, c.pool, nh.Node.ID, true)
		if err != nil {
			return nil, fmt.Errorf("traverse edges: %w", err)
		}
		for _, e := range edges {
			if seenEdge[e.ID] {
				continue
			}
			seenEdge[e.ID] = true
			res.Edges = append(res.Edges, EdgeView{
				Edge:     e,
				FromName: c.nodeName(ctx, names, e.FromID),
				ToName:   c.nodeName(ctx, names, e.ToID),
			})
			if e.SourceURI != "" && !seenURI[e.SourceURI] {
				seenURI[e.SourceURI] = true
				evidence, err := store.GetChunksBySourceURI(ctx, c.pool, e.SourceURI, evidenceChunksPerEdge)
				if err != nil {
					return nil, fmt.Errorf("join evidence: %w", err)
				}
				res.EvidenceChunks = append(res.EvidenceChunks, evidence...)
			}
		}
	}
	return res, nil
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
