package core

import (
	"context"
	"fmt"

	"github.com/programmism/brainiac/internal/model"
	"github.com/programmism/brainiac/internal/store"
)

// Disambiguate introduces identity axes onto an existing node — the reactive way
// to configure discriminators (§12). You work with few tags; when you notice one
// entity actually conflates two things (e.g. the prod vs staging Config), you say
// "this one differs by env=prod" and the node is re-scoped. The given axes are
// merged onto the node's current discriminators (given values win); the node's
// edges/facts move with it (they reference it by id). It is the mirror of a merge:
// merge collapses wrong duplicates, disambiguate separates a wrongly-conflated one.
//
// If a current node already occupies the resulting (name, scope) identity, it
// errors and points at merge instead of silently folding two entities together.
func (c *Core) Disambiguate(ctx context.Context, nodeID string, add map[string]string) (*model.Node, error) {
	if nodeID == "" {
		return nil, fmt.Errorf("disambiguate requires a node id")
	}
	if len(add) == 0 {
		return nil, fmt.Errorf("disambiguate requires at least one discriminator to add")
	}
	if err := model.ValidateDiscriminators(add); err != nil {
		return nil, err
	}
	// A principal may add facet axes (env, client, …) to its own entity but must
	// not re-scope the `project` axis — that moves the entity across the wall (#265).
	if _, ok := add["project"]; ok && PrincipalFrom(ctx) != nil {
		return nil, ErrForbiddenNamespace
	}

	var updated *model.Node
	err := store.WithTx(ctx, c.pool, func(db store.DBTX) error {
		node, err := c.assertNodeWritable(ctx, db, nodeID)
		if err != nil {
			return err
		}

		merged := mergeDiscriminators(node.Discriminators, add)
		newScope := model.ScopeKey(merged)
		if newScope == model.ScopeKey(node.Discriminators) {
			updated = node // no-op: already in that scope
			return nil
		}

		// Guard: don't collapse into an existing entity's identity.
		existing, err := store.GetNodeByCanonicalNameScoped(ctx, db, node.CanonicalName, newScope)
		if err != nil {
			return fmt.Errorf("collision check: %w", err)
		}
		if existing != nil && existing.ID != node.ID {
			return fmt.Errorf("an entity %q already exists in scope %q — use merge, not disambiguate", node.CanonicalName, newScope)
		}

		if err := store.UpdateNodeScope(ctx, db, node.ID, merged); err != nil {
			return fmt.Errorf("update scope: %w", err)
		}
		node.Discriminators = merged
		updated = node
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// mergeDiscriminators returns base with the add axes layered on top (add wins).
// A nil result means global.
func mergeDiscriminators(base, add map[string]string) map[string]string {
	m := map[string]string{}
	for k, v := range base {
		m[k] = v
	}
	for k, v := range add {
		m[k] = v
	}
	if len(m) == 0 {
		return nil
	}
	return m
}
