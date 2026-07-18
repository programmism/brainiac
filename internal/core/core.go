// Package core is the single home of all Brainiac business logic.
//
// Every client (MCP, HTTP, CLI) is a thin adapter that forwards into this
// package; none of them may hold business logic of their own. Core orchestrates
// the storage repositories (internal/store) and the plugin seams
// (internal/plugins) into the operation set: search, remember, link, recall,
// supersede, consolidate, ingest, health.
package core

import (
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/programmism/brainiac/internal/plugins"
)

// Core holds the shared dependencies and exposes the operation set as methods.
// It depends only on the plugin interfaces, never on a concrete plugin.
type Core struct {
	pool      *pgxpool.Pool
	embedder  plugins.Embedder
	selector  plugins.Selector
	startedAt time.Time // process start, for the uptime metric (§9)

	// extractor is the optional server-side Extractor (SYSTEM.md §7). Nil = the
	// default chat-driven path (Claude supplies structure via remember/link); when
	// set, ingest also derives nodes/edges from kept chunks.
	extractor plugins.Extractor
	// extractReview routes extracted nodes/edges to the review queue (status
	// 'proposed') instead of writing them live ('current'). Default true.
	extractReview bool
	// reranker is the optional cross-encoder that reorders retrieved chunks by
	// relevance (SYSTEM.md §7, #213). Nil = the RRF-fused order is returned as is.
	reranker plugins.Reranker

	// retrieval holds the tunable distance thresholds (#332). Defaults are the
	// package consts (MaxRelevantDistance/ChunkDistanceGap/MaxNodeDistance/
	// NodeDistanceGap); a deployment can override them per model/domain without a
	// rebuild via WithRetrievalThresholds (config-wired in the adapters).
	retrieval RetrievalThresholds

	// Process-lifetime ingest/extraction counters exposed as Prometheus counters
	// (#319), so throughput and extraction-failure rate are observable via rate().
	// Monotonic within a process; they reset on restart, which counters tolerate.
	ingestedChunks  atomic.Uint64
	extractFailures atomic.Uint64
}

// IngestedChunksTotal is the cumulative count of chunks stored by ingest this
// process lifetime (#319).
func (c *Core) IngestedChunksTotal() uint64 { return c.ingestedChunks.Load() }

// ExtractFailuresTotal is the cumulative count of chunks whose optional extraction
// errored and was skipped this process lifetime (#319).
func (c *Core) ExtractFailuresTotal() uint64 { return c.extractFailures.Load() }

// Option customizes a Core at construction.
type Option func(*Core)

// WithExtractor enables the optional local-LLM extraction path during ingest.
// review=true (the default posture) writes extracted nodes/edges as 'proposed'
// for human approval; review=false writes them live as 'current'.
func WithExtractor(ext plugins.Extractor, review bool) Option {
	return func(c *Core) {
		c.extractor = ext
		c.extractReview = review
	}
}

// WithReranker enables an optional reranker over retrieved chunks (#213).
func WithReranker(r plugins.Reranker) Option {
	return func(c *Core) { c.reranker = r }
}

// RetrievalThresholds are the tunable cosine-distance gates for retrieval (#332):
// the absolute cutoff and relative gap for chunk hits (Search) and for node hits
// (Recall). Cosine distance runs 0..2; a gap is >= 0.
type RetrievalThresholds struct {
	MaxChunkDistance float64
	ChunkDistanceGap float64
	MaxNodeDistance  float64
	NodeDistanceGap  float64
}

// DefaultRetrievalThresholds returns the measured defaults (the package consts),
// used when a caller passes no override.
func DefaultRetrievalThresholds() RetrievalThresholds {
	return RetrievalThresholds{
		MaxChunkDistance: MaxRelevantDistance,
		ChunkDistanceGap: ChunkDistanceGap,
		MaxNodeDistance:  MaxNodeDistance,
		NodeDistanceGap:  NodeDistanceGap,
	}
}

// WithRetrievalThresholds overrides the retrieval distance gates (#332). Any
// zero-valued field is left at its default so a partial override is safe.
func WithRetrievalThresholds(t RetrievalThresholds) Option {
	return func(c *Core) {
		if t.MaxChunkDistance > 0 {
			c.retrieval.MaxChunkDistance = t.MaxChunkDistance
		}
		if t.ChunkDistanceGap > 0 {
			c.retrieval.ChunkDistanceGap = t.ChunkDistanceGap
		}
		if t.MaxNodeDistance > 0 {
			c.retrieval.MaxNodeDistance = t.MaxNodeDistance
		}
		if t.NodeDistanceGap > 0 {
			c.retrieval.NodeDistanceGap = t.NodeDistanceGap
		}
	}
}

// New constructs a Core over a database pool, an embedder, and a selector.
// selector may be nil for surfaces that never ingest (it is only used by Ingest).
// Options enable opt-in features such as the local-LLM extractor.
func New(pool *pgxpool.Pool, embedder plugins.Embedder, selector plugins.Selector, opts ...Option) *Core {
	c := &Core{pool: pool, embedder: embedder, selector: selector, startedAt: time.Now(), extractReview: true,
		retrieval: DefaultRetrievalThresholds()}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Discriminators merges a project name (sugar for the `project` axis) with any
// explicit axes into one identity set, for clients that expose both. An explicit
// non-empty project wins over an "project" key in extra. Returns nil (global)
// when the result is empty.
func Discriminators(project string, extra map[string]string) map[string]string {
	m := map[string]string{}
	for k, v := range extra {
		m[k] = v
	}
	if project != "" {
		m["project"] = project
	}
	if len(m) == 0 {
		return nil
	}
	return m
}
