package core

import (
	"context"
	"fmt"

	"github.com/programmism/brainiac/internal/model"
	"github.com/programmism/brainiac/internal/store"
)

// ImportCounts reports what a namespace import loaded.
type ImportCounts struct {
	Nodes        int `json:"nodes"`
	Edges        int `json:"edges"`
	Chunks       int `json:"chunks"`
	EdgesSkipped int `json:"edges_skipped"` // an endpoint was not in the bundle
}

// ImportNamespace loads an export bundle (from ExportNamespace) into a target
// project namespace — restore, migrate, or seed a project (#196). Nodes are
// upserted via Remember, so a same-name entity already in the target is reused
// (no forked identity) and summaries are re-embedded; edges are reconnected by
// remapping their endpoint ids old→new; chunks are re-embedded from their retained
// text. An empty target defaults to the bundle's own namespace; a principal may
// only import into its own write namespace.
func (c *Core) ImportNamespace(ctx context.Context, exp *NamespaceExport, target string) (ImportCounts, error) {
	var counts ImportCounts
	if exp == nil {
		return counts, fmt.Errorf("import requires an export bundle")
	}
	if target == "" {
		target = exp.Namespace
	}
	if target == "" {
		return counts, fmt.Errorf("import requires a target namespace (bundle has none)")
	}
	if p := PrincipalFrom(ctx); p != nil && target != p.Write {
		return counts, ErrForbiddenNamespace
	}

	// Nodes first, recording old→new ids so edges can reconnect.
	idMap := make(map[string]string, len(exp.Nodes))
	for _, n := range exp.Nodes {
		r, err := c.Remember(ctx, RememberInput{
			CanonicalName:  n.CanonicalName,
			Type:           n.Type,
			Aliases:        n.Aliases,
			Summary:        n.Summary,
			Discriminators: reproject(n.Discriminators, target),
		})
		if err != nil {
			return counts, fmt.Errorf("import node %q: %w", n.CanonicalName, err)
		}
		idMap[n.ID] = r.Node.ID
		counts.Nodes++
	}

	// Edges: reconnect remapped endpoints; skip any whose endpoint left the bundle.
	for _, e := range exp.Edges {
		from, ok1 := idMap[e.FromID]
		to, ok2 := idMap[e.ToID]
		if !ok1 || !ok2 {
			counts.EdgesSkipped++
			continue
		}
		edge := &model.Edge{
			FromID: from, ToID: to, Type: e.Type, Why: e.Why, SourceURI: e.SourceURI,
			SourceLocator: e.SourceLocator, Author: e.Author, Status: e.Status,
		}
		if err := store.InsertEdge(ctx, c.pool, edge); err != nil {
			return counts, fmt.Errorf("import edge %s→%s: %w", from, to, err)
		}
		counts.Edges++
	}

	// Chunks: re-embed from retained text and land in the target namespace.
	disc := reproject(nil, target)
	for _, ch := range exp.Chunks {
		emb, err := c.embedSummary(ctx, ch.Text) // reuses the nil-embedder-safe embed helper
		if err != nil {
			return counts, fmt.Errorf("import chunk embed: %w", err)
		}
		nc := &model.Chunk{
			Text: ch.Text, Embedding: emb, SourceURI: ch.SourceURI, SourceLocator: ch.SourceLocator,
			QualityScore: ch.QualityScore, Tier: ch.Tier, ContentHash: ch.ContentHash,
			SourceModifiedAt: ch.SourceModifiedAt, Discriminators: disc,
		}
		if err := store.InsertChunk(ctx, c.pool, nc); err != nil {
			return counts, fmt.Errorf("import chunk: %w", err)
		}
		counts.Chunks++
	}
	return counts, nil
}
