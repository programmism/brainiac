package core

import (
	"context"
	"fmt"
	"time"

	"github.com/programmism/brainiac/internal/model"
	"github.com/programmism/brainiac/internal/store"
)

// NodeDetail is a single entity with its edges — the direct-lookup counterpart to
// recall's semantic discovery. It answers "I already know this entity; give me its
// full record and relationships" without a query.
type NodeDetail struct {
	Node model.Node `json:"node"`
	// Trust is the node's derived trust (#375): TrustUntrusted when the entity is
	// known *only* through untrusted content (every current edge is untrusted),
	// else TrustTrusted. A node accrues from many sources, so trust is computed
	// from its live edges rather than stored — treat an untrusted entity's
	// attributes as claims to weigh, not established fact.
	Trust string     `json:"trust"`
	Edges []EdgeView `json:"edges"`
}

// deriveNodeTrust computes a node's trust from its current edges (#375): untrusted
// only when the node has at least one current edge and every current edge is
// untrusted. A node with no current edges carries no untrusted signal (trusted).
func deriveNodeTrust(edges []EdgeView) string {
	current := 0
	for _, e := range edges {
		if e.Edge.Status != model.StatusCurrent {
			continue
		}
		current++
		if e.Edge.Trust != model.TrustUntrusted {
			return model.TrustTrusted // one trusted current edge → trusted
		}
	}
	if current == 0 {
		return model.TrustTrusted
	}
	return model.TrustUntrusted // ≥1 current edge and all untrusted
}

// GetNode looks up one entity — by id when non-empty, otherwise by canonical name
// within the project's lens (the project's own scope first, then global) — and
// returns it with its edges (including supersedes history) resolved for citation.
// Returns (nil, nil) when nothing matches.
func (c *Core) GetNode(ctx context.Context, id, name, project string) (*NodeDetail, error) {
	node, wall, err := c.resolveNode(ctx, id, name, project)
	if err != nil || node == nil {
		return nil, err
	}
	edges, err := store.EdgesForNode(ctx, c.pool, node.ID, true, maxEdgesPerNode, wall)
	if err != nil {
		return nil, fmt.Errorf("get node edges: %w", err)
	}
	views := c.edgeViews(ctx, node, edges)
	return &NodeDetail{Node: *node, Trust: deriveNodeTrust(views), Edges: views}, nil
}

// GetNodeAsOf answers "what did we think about X on date Y": the entity and only
// the relationships that were live at asOf — created by then and not yet
// superseded (#200). Returns (nil, nil) if the entity did not yet exist at asOf.
func (c *Core) GetNodeAsOf(ctx context.Context, id, name, project string, asOf time.Time) (*NodeDetail, error) {
	node, wall, err := c.resolveNode(ctx, id, name, project)
	if err != nil || node == nil {
		return nil, err
	}
	if node.CreatedAt.After(asOf) {
		return nil, nil // the entity didn't exist yet
	}
	edges, err := store.EdgesForNodeAsOf(ctx, c.pool, node.ID, asOf, maxEdgesPerNode, wall)
	if err != nil {
		return nil, fmt.Errorf("get node edges as-of: %w", err)
	}
	views := c.edgeViews(ctx, node, edges)
	return &NodeDetail{Node: *node, Trust: deriveNodeTrust(views), Edges: views}, nil
}

// resolveNode looks up one entity by id or name (name honoring the read wall /
// project lens) and withholds a node outside the caller's wall. Returns the
// effective wall for the subsequent edge read.
func (c *Core) resolveNode(ctx context.Context, id, name, project string) (*model.Node, store.Wall, error) {
	var (
		node *model.Node
		err  error
	)
	_, wall := c.readScope(ctx, project)
	switch {
	case id != "":
		node, err = store.GetNodeByID(ctx, c.pool, id)
	case name != "":
		if PrincipalFrom(ctx) != nil {
			// Under a principal a bare name resolves within the caller's readable
			// namespace(s), not the global scope (#120).
			node, err = store.GetNodeByNameWalled(ctx, c.pool, name, wall)
		} else {
			scopeKey := model.ScopeKey(discFromProject(project))
			node, err = store.GetNodeByCanonicalNameScoped(ctx, c.pool, name, scopeKey)
			if err == nil && node == nil && scopeKey != "" {
				// A project name lookup falls back to the global entity of that name.
				node, err = store.GetNodeByCanonicalNameScoped(ctx, c.pool, name, "")
			}
		}
	default:
		return nil, wall, nil
	}
	if err != nil {
		return nil, wall, fmt.Errorf("get node: %w", err)
	}
	if node == nil {
		return nil, wall, nil
	}
	// Withhold a node outside the caller's read wall — an id/name guess across the
	// wall must read as "not found", never leak the row (#120).
	if !c.visibleToPrincipal(ctx, node.Discriminators) {
		return nil, wall, nil
	}
	return node, wall, nil
}

// edgeViews resolves each edge's endpoint names for citation.
func (c *Core) edgeViews(ctx context.Context, node *model.Node, edges []model.Edge) []EdgeView {
	names := map[string]string{node.ID: node.CanonicalName}
	views := make([]EdgeView, 0, len(edges))
	for _, e := range edges {
		views = append(views, EdgeView{
			Edge:     e,
			FromName: c.nodeName(ctx, names, e.FromID),
			ToName:   c.nodeName(ctx, names, e.ToID),
		})
	}
	return views
}
