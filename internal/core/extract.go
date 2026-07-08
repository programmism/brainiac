package core

import (
	"context"
	"fmt"

	"github.com/programmism/brainiac/internal/model"
	"github.com/programmism/brainiac/internal/store"
)

// extractAuthor is the provenance stamped on nodes/edges the local-LLM extractor
// produces, so their origin is legible in the graph and the review queue.
const extractAuthor = "local-llm"

// extractedStatus is the status extracted nodes/edges are written with: proposed
// (awaiting review) by default, current (live) when review is disabled.
func (c *Core) extractedStatus() model.Status {
	if c.extractReview {
		return model.StatusProposed
	}
	return model.StatusCurrent
}

// extractChunk runs the optional extractor over one chunk and persists the
// resulting nodes and edges — at the configured status — in a single
// transaction. It is best-effort: an error is returned so ingest can count the
// chunk as failed extraction and keep going (graceful degradation, §11); the
// chunk itself is already stored regardless.
func (c *Core) extractChunk(ctx context.Context, text, sourceURI string, disc map[string]string) (nodes, edges int, err error) {
	ext, err := c.extractor.Extract(ctx, text)
	if err != nil {
		return 0, 0, err
	}
	if len(ext.Entities) == 0 && len(ext.Relations) == 0 {
		return 0, 0, nil
	}
	status := c.extractedStatus()

	err = store.WithTx(ctx, c.pool, func(db store.DBTX) error {
		ids := make(map[string]string, len(ext.Entities))
		resolve := func(name, typ string, aliases []string) (string, bool, error) {
			if id, ok := ids[name]; ok {
				return id, false, nil
			}
			n, created, err := c.resolveOrProposeNode(ctx, db, name, typ, aliases, disc, status)
			if err != nil {
				return "", false, err
			}
			ids[name] = n.ID
			return n.ID, created, nil
		}

		for _, en := range ext.Entities {
			_, created, err := resolve(en.Name, en.Type, en.Aliases)
			if err != nil {
				return err
			}
			if created {
				nodes++
			}
		}
		for _, r := range ext.Relations {
			fromID, fc, err := resolve(r.From, "", nil)
			if err != nil {
				return err
			}
			if fc {
				nodes++
			}
			toID, tc, err := resolve(r.To, "", nil)
			if err != nil {
				return err
			}
			if tc {
				nodes++
			}
			edge := &model.Edge{
				FromID:    fromID,
				ToID:      toID,
				Type:      normalizeType(r.Type),
				Why:       r.Why,
				SourceURI: sourceURI,
				Author:    extractAuthor,
				Status:    status,
			}
			if err := store.InsertEdge(ctx, db, edge); err != nil {
				return err
			}
			edges++
		}
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return nodes, edges, nil
}

// resolveOrProposeNode returns the node to attach an extracted edge to, creating
// one at the given status if needed. A live (current) node always wins, so
// extraction links into the real graph rather than duplicating it; otherwise an
// existing pending node for the same identity is reused, so repeated mentions
// across chunks don't stack duplicate proposals.
func (c *Core) resolveOrProposeNode(ctx context.Context, db store.DBTX, name, typ string, aliases []string, disc map[string]string, status model.Status) (*model.Node, bool, error) {
	scope := model.ScopeKey(disc)
	cur, err := store.GetNodeByCanonicalNameScoped(ctx, db, name, scope)
	if err != nil {
		return nil, false, err
	}
	if cur != nil {
		return cur, false, nil
	}
	if status == model.StatusProposed {
		p, err := store.GetNodeByNameScopeStatus(ctx, db, name, scope, model.StatusProposed)
		if err != nil {
			return nil, false, err
		}
		if p != nil {
			return p, false, nil
		}
	}
	n := &model.Node{
		CanonicalName:  name,
		Type:           normalizeType(typ),
		Aliases:        aliases,
		Discriminators: disc,
		Status:         status,
	}
	if err := store.InsertNode(ctx, db, n); err != nil {
		return nil, false, err
	}
	return n, true, nil
}

// ProposalQueue is the extractor's pending output for human review (§8): nodes
// and edges the local model suggested but that are invisible to recall until
// approved.
type ProposalQueue struct {
	Nodes []model.Node         `json:"nodes"`
	Edges []store.ProposedEdge `json:"edges"`
}

// Proposals returns the pending extraction proposals, capped at limit each.
func (c *Core) Proposals(ctx context.Context, limit int) (*ProposalQueue, error) {
	if limit <= 0 {
		limit = 100
	}
	nodes, err := store.ListProposedNodes(ctx, c.pool, limit)
	if err != nil {
		return nil, fmt.Errorf("list proposed nodes: %w", err)
	}
	edges, err := store.ListProposedEdges(ctx, c.pool, limit)
	if err != nil {
		return nil, fmt.Errorf("list proposed edges: %w", err)
	}
	return &ProposalQueue{Nodes: nodes, Edges: edges}, nil
}

// ApproveNode promotes a proposed node to current (live in the memory).
func (c *Core) ApproveNode(ctx context.Context, id string) error {
	return store.UpdateNodeStatus(ctx, c.pool, id, model.StatusCurrent)
}

// RejectNode retires a proposed node (kept as a historical record, out of every
// read) rather than deleting it, preserving the trail of what was suggested.
func (c *Core) RejectNode(ctx context.Context, id string) error {
	return store.UpdateNodeStatus(ctx, c.pool, id, model.StatusHistorical)
}

// ApproveEdge promotes a proposed edge to current, first promoting any still-
// proposed endpoint so a live edge never dangles off an invisible node. If a
// current edge already covers the same (from, to, type), the proposal is retired
// instead of promoted — the live memory already holds that relationship.
func (c *Core) ApproveEdge(ctx context.Context, id string) error {
	return store.WithTx(ctx, c.pool, func(db store.DBTX) error {
		conflict, err := store.CurrentEdgeConflict(ctx, db, id)
		if err != nil {
			return err
		}
		if conflict {
			_, err := store.UpdateEdgeStatus(ctx, db, id, model.StatusHistorical)
			return err
		}
		if err := store.PromoteProposedEndpoints(ctx, db, id); err != nil {
			return err
		}
		_, err = store.UpdateEdgeStatus(ctx, db, id, model.StatusCurrent)
		return err
	})
}

// RejectEdge retires a proposed edge (historical), leaving its endpoint nodes for
// separate review.
func (c *Core) RejectEdge(ctx context.Context, id string) error {
	_, err := store.UpdateEdgeStatus(ctx, c.pool, id, model.StatusHistorical)
	return err
}
