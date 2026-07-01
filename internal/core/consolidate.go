package core

import (
	"context"
	"fmt"

	"github.com/programmism/brainiac/internal/model"
	"github.com/programmism/brainiac/internal/store"
)

// RollupMinEdges is the edge count at which a node becomes a rollup candidate.
const RollupMinEdges = 5

// Conflict is a contradiction with endpoint names resolved for review.
type Conflict struct {
	From string `json:"from"`
	Type string `json:"type"`
	ToA  string `json:"to_a"`
	ToB  string `json:"to_b"`
}

// ConsolidationReport is the librarian pass output — all human-reviewable, none
// applied automatically (§11).
type ConsolidationReport struct {
	MergeGroups [][]model.Node          `json:"merge_groups"`
	Conflicts   []Conflict              `json:"conflicts"`
	Stale       []model.Edge            `json:"stale"`
	Rollups     []store.RollupCandidate `json:"rollups"`
}

// Consolidate runs the librarian pass over the graph (small, not the corpus):
// merge candidates, conflicts, staleness flags, and rollup candidates. It only
// proposes; ApplyMerge/Supersede/Confirm apply the human decisions (§11).
func (c *Core) Consolidate(ctx context.Context) (*ConsolidationReport, error) {
	merges, err := store.ProposeNodeMerges(ctx, c.pool)
	if err != nil {
		return nil, fmt.Errorf("propose merges: %w", err)
	}
	conflictRows, err := store.FindConflicts(ctx, c.pool)
	if err != nil {
		return nil, fmt.Errorf("find conflicts: %w", err)
	}
	stale, err := store.FindStaleEdges(ctx, c.pool)
	if err != nil {
		return nil, fmt.Errorf("find stale: %w", err)
	}
	rollups, err := store.FindRollupCandidates(ctx, c.pool, RollupMinEdges)
	if err != nil {
		return nil, fmt.Errorf("find rollups: %w", err)
	}

	names := make(map[string]string)
	conflicts := make([]Conflict, 0, len(conflictRows))
	for _, r := range conflictRows {
		conflicts = append(conflicts, Conflict{
			From: c.nodeName(ctx, names, r.FromID),
			Type: r.Type,
			ToA:  c.nodeName(ctx, names, r.ToA),
			ToB:  c.nodeName(ctx, names, r.ToB),
		})
	}

	return &ConsolidationReport{MergeGroups: merges, Conflicts: conflicts, Stale: stale, Rollups: rollups}, nil
}

// ApplyMerge merges the drop node into the keep node: its edges are repointed,
// its name/aliases folded into keep, and it is marked historical (reversible,
// alias history kept — §11.1). Atomic.
func (c *Core) ApplyMerge(ctx context.Context, keepID, dropID string) error {
	if keepID == "" || dropID == "" {
		return fmt.Errorf("merge requires both keep and drop ids")
	}
	if keepID == dropID {
		return fmt.Errorf("cannot merge a node into itself")
	}
	return store.WithTx(ctx, c.pool, func(db store.DBTX) error {
		keep, err := store.GetNodeByID(ctx, db, keepID)
		if err != nil {
			return err
		}
		drop, err := store.GetNodeByID(ctx, db, dropID)
		if err != nil {
			return err
		}
		if keep == nil || drop == nil {
			return fmt.Errorf("keep or drop node not found")
		}
		merged := mergeAliases(keep.Aliases, append([]string{drop.CanonicalName}, drop.Aliases...))
		if err := store.UpdateNodeAliases(ctx, db, keep.ID, merged); err != nil {
			return err
		}
		if err := store.RepointEdges(ctx, db, drop.ID, keep.ID); err != nil {
			return err
		}
		return store.UpdateNodeStatus(ctx, db, drop.ID, model.StatusHistorical)
	})
}
