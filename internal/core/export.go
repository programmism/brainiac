package core

import (
	"context"
	"fmt"

	"github.com/programmism/brainiac/internal/model"
	"github.com/programmism/brainiac/internal/store"
)

// NamespaceExport is a portable, self-contained backup of one namespace: its
// nodes, edges, and chunks. Embeddings are omitted (recomputable from the
// retained text on import), so the JSON is small and model-agnostic.
type NamespaceExport struct {
	Namespace string        `json:"namespace"`
	Nodes     []model.Node  `json:"nodes"`
	Edges     []model.Edge  `json:"edges"`
	Chunks    []model.Chunk `json:"chunks"`
}

// ExportNamespace dumps everything in the given project namespace for backup or
// hand-off (#187). It reuses the Layer 2 wall predicate as the WHERE, so an
// operator (Layer 1) can export any namespace by name, while a principal can only
// export a namespace it is allowed to read — a foreign name is rejected, never
// silently emptied.
func (c *Core) ExportNamespace(ctx context.Context, namespace string) (*NamespaceExport, error) {
	if namespace == "" {
		return nil, fmt.Errorf("export requires a project namespace")
	}
	if p := PrincipalFrom(ctx); p != nil && !contains(p.Read, namespace) {
		return nil, ErrForbiddenNamespace
	}
	wall := store.Namespaces([]string{namespace})

	nodes, err := store.ExportNodes(ctx, c.pool, wall)
	if err != nil {
		return nil, fmt.Errorf("export nodes: %w", err)
	}
	edges, err := store.ExportEdges(ctx, c.pool, wall)
	if err != nil {
		return nil, fmt.Errorf("export edges: %w", err)
	}
	chunks, err := store.ExportChunks(ctx, c.pool, wall)
	if err != nil {
		return nil, fmt.Errorf("export chunks: %w", err)
	}
	return &NamespaceExport{Namespace: namespace, Nodes: nodes, Edges: edges, Chunks: chunks}, nil
}
