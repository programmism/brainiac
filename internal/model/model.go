// Package model holds Brainiac's domain-neutral data types (chunks, nodes,
// edges) shared between the storage layer and the core. It depends on nothing
// but the standard library, so both layers can import it without cycles.
package model

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// SchemaEmbeddingDims is the vector dimension the schema is built for
// (halfvec(768) = nomic-embed-text). Embeddings must match it.
const SchemaEmbeddingDims = 768

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
	// StatusProposed marks a node/edge extracted by the optional local-LLM
	// Extractor that awaits human review (SYSTEM.md §7, §8). Proposed rows are
	// invisible to recall/search/graph — every read filters status='current' — so
	// a weaker local model can suggest structure without polluting the memory
	// until someone approves it (flip to current) or rejects it (flip to
	// historical).
	StatusProposed Status = "proposed"
)

// Chunk is a unit of the semantic-search layer (Layer 1). Raw Text is always
// stored so vectors can be rebuilt on a model change without re-reading sources.
// The Embedding is never serialized to clients (json:"-").
type Chunk struct {
	ID            string         `json:"id"`
	Text          string         `json:"text"`
	Embedding     []float32      `json:"-"`
	SourceURI     string         `json:"source_uri"`
	SourceLocator map[string]any `json:"source_locator,omitempty"`
	QualityScore  float64        `json:"quality_score"`
	Tier          Tier           `json:"tier"`
	ContentHash   string         `json:"content_hash,omitempty"`
	// Discriminators scope the chunk's identity for the retrieval lens (#119),
	// mirroring Node. Empty = global. Facet tags are not identity.
	Discriminators   map[string]string `json:"discriminators,omitempty"`
	CreatedAt        time.Time         `json:"created_at"`
	SourceModifiedAt *time.Time        `json:"source_modified_at,omitempty"`
}

// Node is an entity in the curated graph (Layer 2).
type Node struct {
	ID            string   `json:"id"`
	CanonicalName string   `json:"canonical_name"`
	Aliases       []string `json:"aliases,omitempty"`
	Type          string   `json:"type,omitempty"`
	// Discriminators are the identity-bearing axes (project, env, ...) that make
	// same-named entities distinct. Empty = global/shared. Facet/descriptive tags
	// are NOT identity and are not stored here (#117).
	Discriminators   map[string]string `json:"discriminators,omitempty"`
	SummaryEmbedding []float32         `json:"-"`
	Status           Status            `json:"status"`
	CreatedAt        time.Time         `json:"created_at"`
	LastConfirmedAt  *time.Time        `json:"last_confirmed_at,omitempty"`
}

// ValidateDiscriminators rejects discriminator sets that would corrupt the
// scope_key serialization. Keys/values must be non-empty and free of the ';' and
// '=' delimiters, otherwise a single crafted pair could collide with a
// multi-pair set (e.g. {"a":"b;c=d"} vs {"a":"b","c":"d"}).
func ValidateDiscriminators(disc map[string]string) error {
	for k, v := range disc {
		if k == "" {
			return fmt.Errorf("discriminator key must not be empty")
		}
		if v == "" {
			return fmt.Errorf("discriminator %q has an empty value", k)
		}
		if strings.ContainsAny(k, ";=") || strings.ContainsAny(v, ";=") {
			return fmt.Errorf("discriminator %q=%q must not contain ';' or '='", k, v)
		}
	}
	return nil
}

// ScopeKey is the canonical identity serialization of a discriminator set:
// sorted "k=v" pairs joined by ";". Empty (global) when there are none. Two nodes
// are the same identity iff they share canonical_name AND ScopeKey.
func ScopeKey(disc map[string]string) string {
	if len(disc) == 0 {
		return ""
	}
	keys := make([]string, 0, len(disc))
	for k := range disc {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(';')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(disc[k])
	}
	return b.String()
}

// ScopeLabel renders a discriminator set as a short, human-readable provenance
// tag for result badges (#143): "global" when empty, "project:NAME" for the
// common single-project case, otherwise the canonical scope_key. It is
// display-only — identity comparisons must use ScopeKey.
func ScopeLabel(disc map[string]string) string {
	if len(disc) == 0 {
		return "global"
	}
	if len(disc) == 1 {
		if v, ok := disc["project"]; ok {
			return "project:" + v
		}
	}
	return ScopeKey(disc)
}

// Edge is a relationship in the curated graph. Why + provenance + author are
// what make this a memory of decisions, not a fact dump.
type Edge struct {
	ID              string         `json:"id"`
	FromID          string         `json:"from_id"`
	ToID            string         `json:"to_id"`
	Type            string         `json:"type"`
	Why             string         `json:"why,omitempty"`
	SourceURI       string         `json:"source_uri,omitempty"`
	SourceLocator   map[string]any `json:"source_locator,omitempty"`
	Author          string         `json:"author,omitempty"`
	Status          Status         `json:"status"`
	FlaggedStale    bool           `json:"flagged_stale"`
	CreatedAt       time.Time      `json:"created_at"`
	LastConfirmedAt *time.Time     `json:"last_confirmed_at,omitempty"`
}

// ChunkHit is a search result: a chunk plus its cosine distance to the query.
type ChunkHit struct {
	Chunk
	Distance float64 `json:"distance"`
	// Scope is the human-readable provenance of this hit ("global" or
	// "project:NAME"), so clients can mark project vs global results (#143).
	Scope string `json:"scope"`
}
