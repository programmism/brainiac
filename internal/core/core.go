// Package core is the single home of all Brainiac business logic.
//
// Every client (MCP, HTTP, CLI) is a thin adapter that forwards into this
// package; none of them may hold business logic of their own. The operation
// set (search, remember, link, recall, supersede, consolidate, ingest,
// health) is added in its own issues — this file is the scaffold that wires
// shared dependencies together.
package core

// Core holds the shared dependencies (storage, embedder, config) and exposes
// the operation set. Operations are attached as methods as they land.
type Core struct {
	// Dependencies (DB pool, embedder, config, plugin registry) are added as
	// the corresponding issues are implemented.
}

// New constructs a Core. Dependency wiring grows as operations are added.
func New() *Core {
	return &Core{}
}
