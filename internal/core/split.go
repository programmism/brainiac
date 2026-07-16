package core

import (
	"context"
	"fmt"

	"github.com/programmism/brainiac/internal/model"
	"github.com/programmism/brainiac/internal/store"
)

// SplitChild is one entity produced by a split: the node for a given axis value
// and how many of the original's edges were routed to it.
type SplitChild struct {
	Value string     `json:"value"`
	Node  model.Node `json:"node"`
	Edges int        `json:"edges"`
}

// SplitResult reports the outcome of a split.
type SplitResult struct {
	Children      []SplitChild `json:"children"`
	ParentRetired bool         `json:"parent_retired"`
}

// Split separates a genuinely conflated node into scoped children along a new
// discriminator axis, routing each of its edges to the child it belongs to
// (#127). It is the heavier sibling of Disambiguate: Disambiguate moves a whole
// node to one value; Split partitions one node's facts across several values.
//
// routes maps an edge id to the axis value it belongs to (e.g. edge → "prod").
// For each distinct value a child node (name + parent discriminators + {axis:value})
// is created or reused, and the routed edges are repointed to it. Edges not listed
// stay on the parent; if none remain, the parent is retired (historical, reversible).
func (c *Core) Split(ctx context.Context, nodeID, axis string, routes map[string]string) (*SplitResult, error) {
	if nodeID == "" {
		return nil, fmt.Errorf("split requires a node id")
	}
	if axis == "" {
		return nil, fmt.Errorf("split requires an axis")
	}
	if len(routes) == 0 {
		return nil, fmt.Errorf("split requires at least one edge route")
	}
	for _, v := range routes {
		if err := model.ValidateDiscriminators(map[string]string{axis: v}); err != nil {
			return nil, err
		}
	}
	// A principal must not split on the project axis — that would route children
	// out of its namespace (#265). Facet axes (env, client, …) stay in-namespace.
	if axis == "project" && PrincipalFrom(ctx) != nil {
		return nil, ErrForbiddenNamespace
	}

	result := &SplitResult{}
	err := store.WithTx(ctx, c.pool, func(db store.DBTX) error {
		parent, err := c.assertNodeWritable(ctx, db, nodeID)
		if err != nil {
			return err
		}

		// One child node per distinct routed value (get-or-create by identity).
		childByValue := make(map[string]*model.Node)
		countByValue := make(map[string]int)
		for _, v := range routes {
			if _, ok := childByValue[v]; ok {
				continue
			}
			childDisc := mergeDiscriminators(parent.Discriminators, map[string]string{axis: v})
			existing, err := store.GetNodeByCanonicalNameScoped(ctx, db, parent.CanonicalName, model.ScopeKey(childDisc))
			if err != nil {
				return fmt.Errorf("resolve child %q: %w", v, err)
			}
			if existing != nil {
				childByValue[v] = existing
				continue
			}
			child := &model.Node{CanonicalName: parent.CanonicalName, Aliases: parent.Aliases, Discriminators: childDisc}
			if err := store.InsertNode(ctx, db, child); err != nil {
				return fmt.Errorf("create child %q: %w", v, err)
			}
			childByValue[v] = child
		}

		// Route each edge to its child.
		for edgeID, v := range routes {
			child := childByValue[v]
			if child.ID == parent.ID {
				continue // value equals the parent's own axis value — nothing to move
			}
			moved, err := store.RepointEdgeEndpoint(ctx, db, edgeID, parent.ID, child.ID)
			if err != nil {
				return fmt.Errorf("route edge %s: %w", edgeID, err)
			}
			if moved {
				countByValue[v]++
			}
		}

		// Retire the parent if it has no current edges left.
		remaining, err := store.EdgesForNode(ctx, db, parent.ID, false, 1, store.NoWall())
		if err != nil {
			return fmt.Errorf("check remaining edges: %w", err)
		}
		if len(remaining) == 0 {
			if err := store.UpdateNodeStatus(ctx, db, parent.ID, model.StatusHistorical); err != nil {
				return fmt.Errorf("retire parent: %w", err)
			}
			result.ParentRetired = true
		}

		for v, child := range childByValue {
			result.Children = append(result.Children, SplitChild{Value: v, Node: *child, Edges: countByValue[v]})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	c.audit(ctx, "split", nodeID+" by "+axis, "")
	return result, nil
}
