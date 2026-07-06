package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/programmism/brainiac/internal/chunk"
	"github.com/programmism/brainiac/internal/model"
	"github.com/programmism/brainiac/internal/plugins"
	"github.com/programmism/brainiac/internal/store"
)

// IngestOptions tunes an ingest run. (Chunking is content-defined, so there is
// no size knob; see internal/chunk.)
type IngestOptions struct {
	// Project scopes every chunk from this run to the retrieval lens (#119).
	// Empty = global.
	Project string
}

// discFromProject builds the identity discriminator set for a project name;
// empty project = nil = global.
func discFromProject(project string) map[string]string {
	if project == "" {
		return nil
	}
	return map[string]string{"project": project}
}

// IngestStats reports what happened during an ingest run.
type IngestStats struct {
	Docs    int // documents fetched
	Chunks  int // chunks seen
	Kept    int // stored hot
	Queued  int // stored cold (borderline; excluded from default search)
	Dropped int // rejected by the selector
	Skipped int // unchanged (content hash already present for this source)
	Deleted int // stale chunks removed (source content edited away/removed)
	Failed  int // documents that failed (e.g. embedder down) — skipped, run continues
}

// Ingest runs the Layer-1 pipeline for a connector: fetch → chunk → select →
// embed → store, reconciling per document (SYSTEM.md §8, PRD §8). Each document
// is actualized: unchanged chunks are kept, chunks whose content was edited away
// or removed are deleted, and new chunks are inserted — all in one transaction
// per document. A document that fails (e.g. the embedder is down) is counted and
// skipped; the run continues.
func (c *Core) Ingest(ctx context.Context, conn plugins.SourceConnector, opts IngestOptions) (IngestStats, error) {
	if c.selector == nil {
		return IngestStats{}, fmt.Errorf("ingest requires a selector")
	}

	disc := discFromProject(opts.Project)
	var stats IngestStats
	for doc, err := range conn.Fetch(ctx) {
		if err != nil {
			return stats, fmt.Errorf("fetch: %w", err)
		}
		stats.Docs++
		if err := c.ingestDoc(ctx, doc, disc, &stats); err != nil {
			stats.Failed++ // skip this doc, keep going
			continue
		}
	}
	return stats, nil
}

// IngestText stores a single document's text into the memory (chunk → select →
// embed → store, with per-source reconcile), for content a client already has
// in hand — e.g. Claude reading Notion/web via its own integration and pushing
// it in (the chat-driven path; no server-side connector/token needed).
func (c *Core) IngestText(ctx context.Context, sourceURI, text, project string) (IngestStats, error) {
	if c.selector == nil {
		return IngestStats{}, fmt.Errorf("ingest requires a selector")
	}
	if sourceURI == "" {
		return IngestStats{}, fmt.Errorf("source_uri is required")
	}
	stats := IngestStats{Docs: 1}
	if err := c.ingestDoc(ctx, plugins.RawDoc{SourceURI: sourceURI, Text: text}, discFromProject(project), &stats); err != nil {
		stats.Failed++
		return stats, err
	}
	return stats, nil
}

// ingestDoc actualizes a single document. Embeddings are computed outside the
// transaction (no network held open); the reconcile (delete stale + insert new)
// runs in one short transaction.
func (c *Core) ingestDoc(ctx context.Context, doc plugins.RawDoc, disc map[string]string, stats *IngestStats) error {
	chunks := chunk.Split(doc.Text)
	hashes := make([]string, len(chunks))
	for i, ck := range chunks {
		hashes[i] = hashText(ck)
	}

	existing, err := store.ChunkHashesBySourceURI(ctx, c.pool, doc.SourceURI)
	if err != nil {
		return err
	}

	// Pass 1: decide skip/drop/keep per chunk and collect the ones that need
	// embedding, so they can be embedded in one batch instead of one round-trip
	// each (embedding dominates bulk-ingest cost — #140).
	type pending struct {
		text    string
		hash    string
		tier    model.Tier
		quality float64
	}
	var toEmbed []pending
	skipped, dropped, kept, queued := 0, 0, 0, 0
	for i, ck := range chunks {
		if existing[hashes[i]] {
			skipped++ // unchanged — already stored for this source
			continue
		}
		score := c.selector.Score(ck)
		if score.Decision == plugins.Drop {
			dropped++
			continue
		}
		tier := model.TierHot
		if score.Decision == plugins.Queue {
			tier = model.TierCold
			queued++
		} else {
			kept++
		}
		toEmbed = append(toEmbed, pending{text: ck, hash: hashes[i], tier: tier, quality: score.Quality})
	}

	// Pass 2: embed all pending chunks (batched when the embedder supports it),
	// before the tx so no network is held open across the reconcile.
	texts := make([]string, len(toEmbed))
	for i, p := range toEmbed {
		texts[i] = p.text
	}
	embs, err := c.embedTexts(ctx, texts)
	if err != nil {
		return fmt.Errorf("embed chunks: %w", err)
	}
	inserts := make([]*model.Chunk, 0, len(toEmbed))
	for i, p := range toEmbed {
		emb := embs[i]
		if len(emb) != model.SchemaEmbeddingDims {
			return fmt.Errorf("embedding has %d dims, schema expects %d (wrong embedding model?)", len(emb), model.SchemaEmbeddingDims)
		}
		inserts = append(inserts, &model.Chunk{
			Text: p.text, Embedding: emb, SourceURI: doc.SourceURI, SourceLocator: doc.SourceLocator,
			QualityScore: p.quality, Tier: p.tier, ContentHash: p.hash, SourceModifiedAt: doc.ModifiedAt,
			Discriminators: disc,
		})
	}

	var deleted int64
	err = store.WithTx(ctx, c.pool, func(db store.DBTX) error {
		d, err := store.DeleteChunksBySourceURINotIn(ctx, db, doc.SourceURI, hashes)
		if err != nil {
			return err
		}
		deleted = d
		for _, ch := range inserts {
			if err := store.InsertChunk(ctx, db, ch); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	stats.Chunks += len(chunks)
	stats.Skipped += skipped
	stats.Dropped += dropped
	stats.Kept += kept
	stats.Queued += queued
	stats.Deleted += int(deleted)
	return nil
}

// embedTexts embeds chunk texts, using the embedder's batch path when it exposes
// one (far fewer round-trips on bulk ingest — #140) and falling back to one call
// per text otherwise. The result is aligned 1:1 with texts.
func (c *Core) embedTexts(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if be, ok := c.embedder.(plugins.BatchEmbedder); ok {
		embs, err := be.EmbedBatch(ctx, texts)
		if err != nil {
			return nil, err
		}
		if len(embs) != len(texts) {
			return nil, fmt.Errorf("batch embed returned %d vectors for %d texts", len(embs), len(texts))
		}
		return embs, nil
	}
	embs := make([][]float32, len(texts))
	for i, t := range texts {
		v, err := c.embedder.Embed(ctx, t)
		if err != nil {
			return nil, err
		}
		embs[i] = v
	}
	return embs, nil
}

func hashText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
