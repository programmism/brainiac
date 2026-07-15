package core

import (
	"context"
	"errors"
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
	// Discriminators scope both endpoints' identity (project, env, …). Empty =
	// global. Both endpoints of a link share the same scope (#116/#117).
	Discriminators map[string]string
}

// Link creates an edge, creating either endpoint node if it does not yet exist.
// Endpoints and edge land in one transaction (the capture flow, §9).
func (c *Core) Link(ctx context.Context, in LinkInput) (*model.Edge, error) {
	in.Type = normalizeType(in.Type) // canonicalize separator/case variants (#156)
	if in.From == "" || in.To == "" || in.Type == "" {
		return nil, fmt.Errorf("link requires from, to, and type")
	}
	if err := model.ValidateDiscriminators(in.Discriminators); err != nil {
		return nil, err
	}
	disc, err := c.pinWrite(ctx, in.Discriminators)
	if err != nil {
		return nil, err
	}
	in.Discriminators = disc
	edge := &model.Edge{
		Type:          in.Type,
		Why:           in.Why,
		SourceURI:     in.SourceURI,
		SourceLocator: in.SourceLocator,
		Author:        in.Author,
	}
	err = store.WithTx(ctx, c.pool, func(db store.DBTX) error {
		from, err := ensureNode(ctx, db, in.From, in.Discriminators)
		if err != nil {
			return fmt.Errorf("resolve from-node: %w", err)
		}
		to, err := ensureNode(ctx, db, in.To, in.Discriminators)
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

// ensureNode returns the current node with the given canonical name within the
// given identity scope, creating a bare one (in that scope) if none exists.
func ensureNode(ctx context.Context, db store.DBTX, name string, disc map[string]string) (*model.Node, error) {
	n, err := store.GetNodeByCanonicalNameScoped(ctx, db, name, model.ScopeKey(disc))
	if err != nil {
		return nil, err
	}
	if n != nil {
		return n, nil
	}
	// A new endpoint counts against the namespace node quota (#186); count within
	// the tx so the two endpoints of one link see each other.
	if err := checkNodeQuota(ctx, db); err != nil {
		return nil, err
	}
	n = &model.Node{CanonicalName: name, Discriminators: disc}
	if err := store.InsertNode(ctx, db, n); err != nil {
		if errors.Is(err, store.ErrNodeExists) {
			// Concurrent create won the race — reuse the existing node (#220).
			return store.GetNodeByCanonicalNameScoped(ctx, db, name, model.ScopeKey(disc))
		}
		return nil, err
	}
	return n, nil
}
