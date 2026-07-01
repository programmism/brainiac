package core

import (
	"context"

	"github.com/programmism/brainiac/internal/store"
)

// DefaultGraphLimit bounds how many nodes the graph view returns.
const DefaultGraphLimit = 200

// GraphNode is a node for visualization.
type GraphNode struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type,omitempty"`
}

// GraphEdge is an edge for visualization (endpoints are node ids).
type GraphEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Type string `json:"type"`
	Why  string `json:"why,omitempty"`
}

// GraphView is a bounded snapshot of the current graph.
type GraphView struct {
	Nodes []GraphNode `json:"nodes"`
	Edges []GraphEdge `json:"edges"`
}

// Graph returns a bounded snapshot of the current graph for the WebUI. Edges
// referencing nodes outside the returned set are dropped.
func (c *Core) Graph(ctx context.Context, limit int) (*GraphView, error) {
	if limit <= 0 {
		limit = DefaultGraphLimit
	}
	nodes, edges, err := store.GraphSnapshot(ctx, c.pool, limit)
	if err != nil {
		return nil, err
	}

	view := &GraphView{Nodes: make([]GraphNode, 0, len(nodes))}
	known := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		known[n.ID] = true
		view.Nodes = append(view.Nodes, GraphNode{ID: n.ID, Name: n.CanonicalName, Type: n.Type})
	}
	for _, e := range edges {
		if known[e.FromID] && known[e.ToID] {
			view.Edges = append(view.Edges, GraphEdge{From: e.FromID, To: e.ToID, Type: e.Type, Why: e.Why})
		}
	}
	return view, nil
}
