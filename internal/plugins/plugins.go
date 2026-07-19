// Package plugins defines the four swappable seams of Brainiac — source
// connectors, extractors, selectors, and embedders — plus the value types they
// exchange and a registry that lets configuration select a variant by name.
//
// The interfaces are drawn from the start (SYSTEM.md §2.3, §7). The
// SourceConnector seam is now **stable**: it was validated against a real second
// implementation (the Markdown connector, #31) built after the Notion one, and
// the interface fit both with no changes. Implementations: Notion + Markdown
// connectors, the chat-driven extractor (client-supplied), the density-filter
// selector, and the Ollama embedder. The core depends only on these interfaces,
// never on a concrete plugin.
package plugins

import (
	"context"
	"iter"
	"time"

	"github.com/programmism/brainiac/internal/model"
)

// Reranker reorders retrieved chunks by relevance to the query — the precision
// lever a dense+lexical retriever lacks (#213). Optional: when unset, retrieval
// returns the RRF-fused order. A typical implementation is a local cross-encoder
// scoring each (query, chunk) pair. It receives the fused candidate pool and
// returns it reordered (it must not add or drop hits).
type Reranker interface {
	Rerank(ctx context.Context, query string, hits []model.ChunkHit) ([]model.ChunkHit, error)
}

// RawDoc is a document produced by a connector, before selection and chunking.
type RawDoc struct {
	Text          string
	SourceURI     string
	SourceLocator map[string]any
	Metadata      map[string]any
	// ModifiedAt is the source's last-edited time, if the connector knows it.
	// Stored as chunks.source_modified_at to power staleness signals.
	ModifiedAt *time.Time
}

// ChangeKind classifies a source-side change.
type ChangeKind string

// Change kinds reported by a connector's Watch.
const (
	ChangeUpserted ChangeKind = "upserted"
	ChangeDeleted  ChangeKind = "deleted"
)

// Change describes a source-side change, powering refresh/actualization.
type Change struct {
	SourceURI string
	Kind      ChangeKind
}

// SourceConnector imports documents from a source and reports when they change.
// The core is agnostic to origin (Notion, Git, Markdown, Slack, ...).
//
// Fetch and Watch return pull-based iterators; a non-nil error yielded by the
// iterator ends iteration.
type SourceConnector interface {
	Fetch(ctx context.Context) iter.Seq2[RawDoc, error]
	Watch(ctx context.Context) iter.Seq2[Change, error]
}

// BatchItem pairs a stable custom id with the chunk text to extract, for the async
// batch-extraction API (#383/#420).
type BatchItem struct {
	CustomID string
	Text     string
}

// Entity is a node candidate extracted from text.
type Entity struct {
	Name    string
	Type    string
	Aliases []string
}

// Relation is an edge candidate extracted from text; Why carries the rationale.
type Relation struct {
	From string
	Type string
	To   string
	Why  string
}

// Extraction is the structured result of turning a chunk into graph elements.
type Extraction struct {
	Entities  []Entity
	Relations []Relation
}

// Extractor turns text into nodes/edges. The v1 default `chat-driven` extractor
// is bypassed — Claude supplies the Extraction directly via the MCP client — so
// server-side Extractors exist only for automated bulk paths.
type Extractor interface {
	Extract(ctx context.Context, chunk string) (Extraction, error)
}

// Decision is the selection verdict for a chunk.
type Decision string

const (
	// Keep — index it.
	Keep Decision = "keep"
	// Queue — borderline; send to the LLM gatekeeper.
	Queue Decision = "queue"
	// Drop — water; do not index.
	Drop Decision = "drop"
)

// Score is a selector's verdict: a density/value metric plus a keep/queue/drop
// decision.
type Score struct {
	Quality  float64
	Decision Decision
}

// Selector decides what is allowed into the index (the "water filter"). Criteria
// differ by domain, so it is a plugin (SYSTEM.md §7.3, §8).
type Selector interface {
	Score(chunk string) Score
}

// Embedder turns text into a vector. Not bound to Ollama even though v1 uses it.
// Embed is the DOCUMENT/storage path: what gets indexed (chunks, node summaries).
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	Dims() int
}

// QueryEmbedder is an optional Embedder extension for asymmetric retrieval models
// (e.g. nomic-embed-text) that embed a *query* differently from a *document* — for
// nomic via `search_query:` vs `search_document:` task prefixes. Omitting the
// prefixes measurably degrades recall and makes query/doc distances non-comparable.
// Core uses EmbedQuery for search/recall when the embedder exposes it and falls
// back to Embed otherwise, so a symmetric embedder needs no change.
type QueryEmbedder interface {
	Embedder
	EmbedQuery(ctx context.Context, text string) ([]float32, error)
}

// BatchEmbedder is an optional Embedder extension: embed many texts in one shot.
// Bulk ingest embeds one chunk per HTTP round-trip otherwise, which dominates the
// cost of a large import (#140). Ingest uses this path when the embedder exposes
// it and falls back to Embed otherwise, so it stays transparent to the core. The
// returned slice must be aligned 1:1 with texts (same length and order).
type BatchEmbedder interface {
	Embedder
	EmbedBatch(ctx context.Context, texts []string) ([][]float32, error)
}
