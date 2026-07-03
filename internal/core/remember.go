package core

import (
	"context"
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
		return &RememberResult{Node: existing, Created: false}, nil
	}

	var emb []float32
	if in.Summary != "" && c.embedder != nil {
		emb, err = c.embedder.Embed(ctx, in.Summary)
		if err != nil {
			return nil, fmt.Errorf("embed summary: %w", err)
		}
	}

	dups, err := c.findDuplicates(ctx, in.CanonicalName, scope, emb)
	if err != nil {
		return nil, err
	}

	node := &model.Node{
		CanonicalName:    in.CanonicalName,
		Type:             in.Type,
		Aliases:          in.Aliases,
		Discriminators:   in.Discriminators,
		SummaryEmbedding: emb,
	}
	if err := store.InsertNode(ctx, c.pool, node); err != nil {
		return nil, fmt.Errorf("insert node: %w", err)
	}
	return &RememberResult{Node: node, Created: true, Duplicates: dups}, nil
}

func (c *Core) findDuplicates(ctx context.Context, name, scope string, emb []float32) ([]DuplicateCandidate, error) {
	var dups []DuplicateCandidate

	// Dedup only within the same identity scope: two same-named entities in
	// different projects are distinct, not duplicates (#117).
	byName, err := store.FindNodesByNormalizedName(ctx, c.pool, name, store.ExactScope(scope))
	if err != nil {
		return nil, fmt.Errorf("normalized-name dedup: %w", err)
	}
	for _, n := range byName {
		dups = append(dups, DuplicateCandidate{Node: n, Reason: "normalized-name"})
	}

	if emb != nil {
		hits, err := store.FindSimilarNodes(ctx, c.pool, emb, 5, store.ExactScope(scope))
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
