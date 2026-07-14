package core

import (
	"context"
	"fmt"

	"github.com/programmism/brainiac/internal/model"
	"github.com/programmism/brainiac/internal/store"
)

// NodeDetail is a single entity with its edges — the direct-lookup counterpart to
// recall's semantic discovery. It answers "I already know this entity; give me its
// full record and relationships" without a query.
type NodeDetail struct {
	Node  model.Node `json:"node"`
	Edges []EdgeView `json:"edges"`
}

// GetNode looks up one entity — by id when non-empty, otherwise by canonical name
// within the project's lens (the project's own scope first, then global) — and
// returns it with its edges (including supersedes history) resolved for citation.
// Returns (nil, nil) when nothing matches.
func (c *Core) GetNode(ctx context.Context, id, name, project string) (*NodeDetail, error) {
	var (
		node *model.Node
		err  error
	)
	switch {
	case id != "":
		node, err = store.GetNodeByID(ctx, c.pool, id)
	case name != "":
		scopeKey := model.ScopeKey(discFromProject(project))
		node, err = store.GetNodeByCanonicalNameScoped(ctx, c.pool, name, scopeKey)
		if err == nil && node == nil && scopeKey != "" {
			// A project name lookup falls back to the global entity of that name.
			node, err = store.GetNodeByCanonicalNameScoped(ctx, c.pool, name, "")
		}
	default:
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get node: %w", err)
	}
	if node == nil {
		return nil, nil
	}

	edges, err := store.EdgesForNode(ctx, c.pool, node.ID, true, maxEdgesPerNode)
	if err != nil {
		return nil, fmt.Errorf("get node edges: %w", err)
	}
	names := map[string]string{node.ID: node.CanonicalName}
	views := make([]EdgeView, 0, len(edges))
	for _, e := range edges {
		views = append(views, EdgeView{
			Edge:     e,
			FromName: c.nodeName(ctx, names, e.FromID),
			ToName:   c.nodeName(ctx, names, e.ToID),
		})
	}
	return &NodeDetail{Node: *node, Edges: views}, nil
}
