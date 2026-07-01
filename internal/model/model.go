// Package model holds Brainiac's domain-neutral data types (chunks, nodes,
// edges) shared between the storage layer and the core. It depends on nothing
// but the standard library, so both layers can import it without cycles.
package model

import "time"

// Tier marks whether a chunk is in the active index or the archive.
type Tier string

// Chunk tiers.
const (
	TierHot  Tier = "hot"
	TierCold Tier = "cold"
)

// Status marks whether a node/edge is current or a preserved historical record.
type Status string

// Node/edge statuses.
const (
	StatusCurrent    Status = "current"
	StatusHistorical Status = "historical"
)

// Chunk is a unit of the semantic-search layer (Layer 1). Raw Text is always
// stored so vectors can be rebuilt on a model change without re-reading sources.
type Chunk struct {
	ID               string
	Text             string
	Embedding        []float32
	SourceURI        string
	SourceLocator    map[string]any
	QualityScore     float64
	Tier             Tier
	ContentHash      string
	CreatedAt        time.Time
	SourceModifiedAt *time.Time
}

// Node is an entity in the curated graph (Layer 2).
type Node struct {
	ID               string
	CanonicalName    string
	Aliases          []string
	Type             string
	SummaryEmbedding []float32
	Status           Status
	CreatedAt        time.Time
	LastConfirmedAt  *time.Time
}

// Edge is a relationship in the curated graph. Why + provenance + author are
// what make this a memory of decisions, not a fact dump.
type Edge struct {
	ID              string
	FromID          string
	ToID            string
	Type            string
	Why             string
	SourceURI       string
	SourceLocator   map[string]any
	Author          string
	Status          Status
	CreatedAt       time.Time
	LastConfirmedAt *time.Time
}

// ChunkHit is a search result: a chunk plus its cosine distance to the query.
type ChunkHit struct {
	Chunk
	Distance float64
}
