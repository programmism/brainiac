package core

import (
	"context"
	"strings"

	"github.com/programmism/brainiac/internal/model"
	"github.com/programmism/brainiac/internal/store"
)

// normIdent mirrors the SQL normExpr (store/consolidation.go): lowercase, then drop
// every non-alphanumeric rune, so "Order Service", "order-service", and
// "OrderService" collapse to one identity key. Kept in sync with normExpr so the
// batch resolver and the review-time merge proposer agree on identity.
func normIdent(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// resolveBatchDuplicates collapses proposed nodes in the given identity scopes that
// refer to the same entity — same normalized canonical name, or one node's name
// matching another's alias — into a single proposal, folding aliases and repointing
// edges onto the survivor (#431). It runs after a batch of extractions is applied,
// when a whole document set's mentions are visible at once, catching cross-document
// duplicates that per-chunk resolution (exact canonical name only) leaves as
// separate proposals.
//
// Conservative by construction:
//   - it merges only on exact *normalized* string identity (name/alias) — never
//     fuzzy or embedding similarity, which stays a review-time proposal (Consolidate);
//   - it only touches PROPOSED nodes, so nothing live is altered and a human still
//     reviews the merged proposal;
//   - scope is part of the identity key, so same-named entities in different projects
//     never merge (the identity model, #117/#118).
//
// Returns how many duplicate proposals were retired into a survivor.
func (c *Core) resolveBatchDuplicates(ctx context.Context, scopeKeys []string) (merged int, err error) {
	nodes, err := store.ListProposedNodesInScopes(ctx, c.pool, scopeKeys)
	if err != nil {
		return 0, err
	}
	if len(nodes) < 2 {
		return 0, nil
	}

	// Union-find over nodes that share any identity key. nodes is ordered
	// (created_at, id), so the lowest index in a set is the oldest — the survivor.
	parent := make([]int, len(nodes))
	for i := range parent {
		parent[i] = i
	}
	find := func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra == rb {
			return
		}
		if ra < rb { // keep the smaller (older) index as the root
			parent[rb] = ra
		} else {
			parent[ra] = rb
		}
	}

	keyOwner := make(map[string]int) // scope\x00normIdent -> first node index seen
	for i, n := range nodes {
		scope := model.ScopeKey(n.Discriminators)
		idents := make([]string, 0, 1+len(n.Aliases))
		idents = append(idents, n.CanonicalName)
		idents = append(idents, n.Aliases...)
		for _, id := range idents {
			ni := normIdent(id)
			if ni == "" {
				continue
			}
			k := scope + "\x00" + ni
			if j, ok := keyOwner[k]; ok {
				union(j, i)
			} else {
				keyOwner[k] = i
			}
		}
	}

	// Group members by their set root (== survivor index).
	groups := make(map[int][]int)
	for i := range nodes {
		r := find(i)
		if r != i {
			groups[r] = append(groups[r], i)
		}
	}
	if len(groups) == 0 {
		return 0, nil
	}

	err = store.WithTx(ctx, c.pool, func(db store.DBTX) error {
		for rep, drops := range groups {
			survivor := nodes[rep]
			aliases := survivor.Aliases
			for _, m := range drops {
				drop := nodes[m]
				aliases = mergeAliases(aliases, append([]string{drop.CanonicalName}, drop.Aliases...))
			}
			if err := store.UpdateNodeAliases(ctx, db, survivor.ID, aliases); err != nil {
				return err
			}
			for _, m := range drops {
				drop := nodes[m]
				if err := store.RepointProposedEdges(ctx, db, drop.ID, survivor.ID); err != nil {
					return err
				}
				if err := store.UpdateNodeStatus(ctx, db, drop.ID, model.StatusHistorical); err != nil {
					return err
				}
				merged++
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return merged, nil
}
