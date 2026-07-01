// Package core is the single home of all Brainiac business logic.
//
// Every client (MCP, HTTP, CLI) is a thin adapter that forwards into this
// package; none of them may hold business logic of their own. Core orchestrates
// the storage repositories (internal/store) and the plugin seams
// (internal/plugins) into the operation set: search, remember, link, recall,
// supersede, consolidate, ingest, health.
package core

import (
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/programmism/brainiac/internal/plugins"
)

// Core holds the shared dependencies and exposes the operation set as methods.
// It depends only on the plugin interfaces, never on a concrete plugin.
type Core struct {
	pool     *pgxpool.Pool
	embedder plugins.Embedder
	selector plugins.Selector
}

// New constructs a Core over a database pool, an embedder, and a selector.
// selector may be nil for surfaces that never ingest (it is only used by Ingest).
func New(pool *pgxpool.Pool, embedder plugins.Embedder, selector plugins.Selector) *Core {
	return &Core{pool: pool, embedder: embedder, selector: selector}
}
