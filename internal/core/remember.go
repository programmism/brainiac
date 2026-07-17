package core

import (
	"context"
	"errors"
	"fmt"

	"github.com/programmism/brainiac/internal/model"
	"github.com/programmism/brainiac/internal/store"
)

// SemanticDupThreshold is the max cosine distance at which two nodes are
// flagged as likely duplicates. Flagged, never auto-merged — merges are
// human-approved in consolidation (§11.1).
const SemanticDupThreshold = 0.15

// RememberInput describes a node to upsert.
type RememberInput struct {
	CanonicalName string
	Type          string
	Aliases       []string
	// Discriminators are the identity-bearing axes (project, env, ...) that scope
	// this entity. Empty = global/shared. Two nodes are the same identity iff they
	// share CanonicalName AND discriminators (#117).
	Discriminators map[string]string
	// Summary is optional text embedded for semantic dedup and stored on the
	// node's summary_embedding.
	Summary string
}

// DuplicateCandidate is an existing node that may be the same entity.
type DuplicateCandidate struct {
	Node     model.Node
	Reason   string  // "normalized-name" or "semantic"
	Distance float64 // cosine distance for semantic matches
}

// RememberResult reports what happened to the node plus any duplicate flags.
type RememberResult struct {
	Node       *model.Node
	Created    bool // false if an existing exact-name node was returned
	Duplicates []DuplicateCandidate
}

// Remember upserts a node with a dedup check (§5, §9). An exact canonical-name
// match is idempotent (new aliases are merged in). Otherwise the node is
// inserted and likely duplicates — by normalized name or summary-embedding
// proximity — are returned for consolidation to review. Nothing is auto-merged.
func (c *Core) Remember(ctx context.Context, in RememberInput) (*RememberResult, error) {
	if err := model.ValidateDiscriminators(in.Discriminators); err != nil {
		return nil, err
	}
	disc, err := c.pinWrite(ctx, in.Discriminators)
	if err != nil {
		return nil, err
	}
	in.Discriminators = disc
	scope := model.ScopeKey(in.Discriminators)
	existing, err := store.GetNodeByCanonicalNameScoped(ctx, c.pool, in.CanonicalName, scope)
	if err != nil {
		return nil, fmt.Errorf("lookup node: %w", err)
	}
	if existing != nil {
		merged := mergeAliases(existing.Aliases, in.Aliases)
		if len(merged) != len(existing.Aliases) {
			if err := store.UpdateNodeAliases(ctx, c.pool, existing.ID, merged); err != nil {
				return nil, fmt.Errorf("merge aliases: %w", err)
			}
			existing.Aliases = merged
		}
		// Re-remembering with a description backfills/updates the node's summary
		// text and its derived embedding together — the path by which nodes created
		// before summaries were persisted acquire one (#181).
		if in.Summary != "" && in.Summary != existing.Summary {
			emb, err := c.embedSummary(ctx, in.Summary)
			if err != nil {
				return nil, err
			}
			if err := store.UpdateNodeSummary(ctx, c.pool, existing.ID, in.Summary, emb); err != nil {
				return nil, fmt.Errorf("update summary: %w", err)
			}
			existing.Summary = in.Summary
			existing.SummaryEmbedding = emb
		}
		c.audit(ctx, "remember", existing.CanonicalName, existing.Discriminators["project"])
		return &RememberResult{Node: existing, Created: false}, nil
	}

	// Embed outside the transaction — it's a network round-trip and must not hold a
	// DB tx open. The dedup snapshot, quota check, and insert then run in ONE
	// transaction so the quota can't be bypassed by a racing writer and the node
	// count Remember sees is the count it inserts against (#222) — matching Link,
	// which already counts inside its tx.
	emb, err := c.embedSummary(ctx, in.Summary)
	if err != nil {
		return nil, err
	}

	var result *RememberResult
	err = store.WithTx(ctx, c.pool, func(db store.DBTX) error {
		dups, err := c.findDuplicates(ctx, db, in.CanonicalName, scope, emb)
		if err != nil {
			return err
		}
		if err := checkNodeQuota(ctx, db); err != nil {
			return err
		}
		node := &model.Node{
			CanonicalName:    in.CanonicalName,
			Type:             normalizeType(in.Type), // canonicalize separator/case variants (#156)
			Aliases:          in.Aliases,
			Discriminators:   in.Discriminators,
			Summary:          in.Summary,
			SummaryEmbedding: emb,
		}
		if err := store.InsertNode(ctx, db, node); err != nil {
			if errors.Is(err, store.ErrNodeExists) {
				// Lost a create race with a concurrent writer — reuse the winner,
				// keeping remember idempotent (#220). InsertNode's ON CONFLICT DO
				// NOTHING doesn't abort the tx, so re-reading here is safe.
				existing, gerr := store.GetNodeByCanonicalNameScoped(ctx, db, in.CanonicalName, scope)
				if gerr != nil {
					return fmt.Errorf("lookup after conflict: %w", gerr)
				}
				if existing != nil {
					result = &RememberResult{Node: existing, Created: false}
					return nil
				}
			}
			return fmt.Errorf("insert node: %w", err)
		}
		result = &RememberResult{Node: node, Created: true, Duplicates: dups}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Audit only a real create, matching the pre-#222 behavior (the conflict-reuse
	// path stayed silent). Best-effort and outside the tx so a slow audit never
	// holds the write open.
	if result.Created {
		c.audit(ctx, "remember", result.Node.CanonicalName, result.Node.Discriminators["project"])
	}
	return result, nil
}

// embedSummary embeds a node summary for semantic dedup, or returns nil when
// there is no summary or no embedder configured.
func (c *Core) embedSummary(ctx context.Context, summary string) ([]float32, error) {
	if summary == "" || c.embedder == nil {
		return nil, nil
	}
	emb, err := c.embedder.Embed(ctx, summary)
	if err != nil {
		return nil, fmt.Errorf("embed summary: %w", err)
	}
	return emb, nil
}

func (c *Core) findDuplicates(ctx context.Context, db store.DBTX, name, scope string, emb []float32) ([]DuplicateCandidate, error) {
	var dups []DuplicateCandidate

	// Dedup only within the same identity scope: two same-named entities in
	// different projects are distinct, not duplicates (#117).
	byName, err := store.FindNodesByNormalizedName(ctx, db, name, store.ExactScope(scope))
	if err != nil {
		return nil, fmt.Errorf("normalized-name dedup: %w", err)
	}
	for _, n := range byName {
		dups = append(dups, DuplicateCandidate{Node: n, Reason: "normalized-name"})
	}

	if emb != nil {
		hits, err := store.FindSimilarNodes(ctx, db, emb, 5, store.ExactScope(scope), store.NoWall())
		if err != nil {
			return nil, fmt.Errorf("semantic dedup: %w", err)
		}
		for _, h := range hits {
			if h.Distance <= SemanticDupThreshold {
				dups = append(dups, DuplicateCandidate{Node: h.Node, Reason: "semantic", Distance: h.Distance})
			}
		}
	}
	return dups, nil
}

// mergeAliases returns the union of two alias lists, preserving order and
// dropping duplicates.
func mergeAliases(existing, incoming []string) []string {
	seen := make(map[string]bool, len(existing))
	out := make([]string, 0, len(existing)+len(incoming))
	for _, a := range existing {
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	for _, a := range incoming {
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	return out
}
