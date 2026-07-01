package core

import (
	"context"
	"fmt"

	"github.com/programmism/brainiac/internal/model"
	"github.com/programmism/brainiac/internal/store"
)

// LinkInput describes an edge to create between two nodes named by canonical
// name. Missing endpoints are created. Why + provenance + author are the value
// triple (§4.3).
type LinkInput struct {
	From          string
	Type          string
	To            string
	Why           string
	SourceURI     string
	SourceLocator map[string]any
	Author        string
}

// Link creates an edge, creating either endpoint node if it does not yet exist.
// Endpoints and edge land in one transaction (the capture flow, §9).
func (c *Core) Link(ctx context.Context, in LinkInput) (*model.Edge, error) {
	if in.From == "" || in.To == "" || in.Type == "" {
		return nil, fmt.Errorf("link requires from, to, and type")
	}
	edge := &model.Edge{
		Type:          in.Type,
		Why:           in.Why,
		SourceURI:     in.SourceURI,
		SourceLocator: in.SourceLocator,
		Author:        in.Author,
	}
	err := store.WithTx(ctx, c.pool, func(db store.DBTX) error {
		from, err := ensureNode(ctx, db, in.From)
		if err != nil {
			return fmt.Errorf("resolve from-node: %w", err)
		}
		to, err := ensureNode(ctx, db, in.To)
		if err != nil {
			return fmt.Errorf("resolve to-node: %w", err)
		}
		edge.FromID = from.ID
		edge.ToID = to.ID
		return store.InsertEdge(ctx, db, edge)
	})
	if err != nil {
		return nil, err
	}
	return edge, nil
}

// ensureNode returns the current node with the given canonical name, creating a
// bare one if none exists.
func ensureNode(ctx context.Context, db store.DBTX, name string) (*model.Node, error) {
	n, err := store.GetNodeByCanonicalName(ctx, db, name)
	if err != nil {
		return nil, err
	}
	if n != nil {
		return n, nil
	}
	n = &model.Node{CanonicalName: name}
	if err := store.InsertNode(ctx, db, n); err != nil {
		return nil, err
	}
	return n, nil
}
