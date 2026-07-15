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
	// DryRun runs chunking + density selection but writes nothing and embeds
	// nothing: IngestStats reports what *would* happen (chunk count, kept/queued/
	// dropped/skipped, and how many stale chunks would be deleted), so a large or
	// wrongly-scoped import can be previewed before committing (#142).
	DryRun bool
	// OnProgress, if set, is called periodically during a document's embedding
	// with a running snapshot, so clients can show progress on a long import
	// instead of a black box (#139). It fires from the ingest goroutine — keep it
	// cheap and non-blocking. Setting it makes embedding step through the chunks
	// in batches (to report between them) rather than one shot.
	OnProgress func(IngestProgress)
}

// IngestProgress is a running snapshot emitted during ingest (#139).
type IngestProgress struct {
	Doc      string      // SourceURI of the document being embedded
	Embedded int         // chunks embedded so far in this document
	ToEmbed  int         // chunks that need embedding in this document (denominator)
	Stats    IngestStats // running totals across the run so far (prior documents)
}

// ingestProgressStep is how many chunks embed between progress callbacks.
const ingestProgressStep = 64

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
	// Extraction totals — non-zero only when the optional local-LLM extractor is
	// configured (SYSTEM.md §7). Nodes/Edges count what was created (proposed or
	// live per config); ExtractFailed counts chunks whose extraction errored and
	// was skipped (the chunk itself is still stored).
	ExtractedNodes int
	ExtractedEdges int
	ExtractFailed  int
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

	var stats IngestStats
	for doc, err := range conn.Fetch(ctx) {
		if err != nil {
			return stats, fmt.Errorf("fetch: %w", err)
		}
		stats.Docs++
		if err := c.ingestDoc(ctx, doc, opts, &stats); err != nil {
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
	if err := c.ingestDoc(ctx, plugins.RawDoc{SourceURI: sourceURI, Text: text}, IngestOptions{Project: project}, &stats); err != nil {
		stats.Failed++
		return stats, err
	}
	return stats, nil
}

// ingestDoc actualizes a single document. Embeddings are computed outside the
// transaction (no network held open); the reconcile (delete stale + insert new)
// runs in one short transaction.
func (c *Core) ingestDoc(ctx context.Context, doc plugins.RawDoc, opts IngestOptions, stats *IngestStats) error {
	disc, err := c.pinWrite(ctx, discFromProject(opts.Project))
	if err != nil {
		return err
	}
	chunks := chunk.Split(normalizeText(doc.Text))
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

	// Dry run: report what would happen — including how many stale chunks would be
	// deleted (stored hashes no longer present) — without embedding or writing (#142).
	if opts.DryRun {
		have := make(map[string]bool, len(hashes))
		for _, h := range hashes {
			have[h] = true
		}
		wouldDelete := 0
		for h := range existing {
			if !have[h] {
				wouldDelete++
			}
		}
		stats.Chunks += len(chunks)
		stats.Skipped += skipped
		stats.Dropped += dropped
		stats.Kept += kept
		stats.Queued += queued
		stats.Deleted += wouldDelete
		return nil
	}

	// Pass 2: embed all pending chunks (batched when the embedder supports it),
	// before the tx so no network is held open across the reconcile.
	texts := make([]string, len(toEmbed))
	for i, p := range toEmbed {
		texts[i] = p.text
	}
	embs, err := c.embedWithProgress(ctx, doc.SourceURI, texts, opts, stats)
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
		// Enforce the namespace chunk quota after the stale-chunk delete, so a
		// re-ingest that replaces content isn't wrongly rejected (#186).
		if err := checkChunkQuota(ctx, db, len(inserts)); err != nil {
			return err
		}
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

	// Optional Layer-2 extraction: derive nodes/edges from the freshly-stored
	// hot chunks. Runs after the chunk reconcile (chunks are provenance and must
	// persist even if extraction fails), best-effort per chunk so one bad chunk
	// never fails the whole document.
	if c.extractor != nil {
		for _, p := range toEmbed {
			if p.tier != model.TierHot {
				continue // extract from kept knowledge only, not the cold queue
			}
			n, e, err := c.extractChunk(ctx, p.text, doc.SourceURI, disc)
			if err != nil {
				stats.ExtractFailed++
				continue
			}
			stats.ExtractedNodes += n
			stats.ExtractedEdges += e
		}
	}
	return nil
}

// embedWithProgress embeds texts, emitting a running snapshot between batches
// when opts.OnProgress is set (#139). Without a callback it defers to embedTexts
// (the single fast path — the batch embedder still batches internally), so the
// common case is unchanged.
func (c *Core) embedWithProgress(ctx context.Context, sourceURI string, texts []string, opts IngestOptions, stats *IngestStats) ([][]float32, error) {
	if opts.OnProgress == nil || len(texts) == 0 {
		return c.embedTexts(ctx, texts)
	}
	opts.OnProgress(IngestProgress{Doc: sourceURI, ToEmbed: len(texts), Stats: *stats})
	embs := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += ingestProgressStep {
		end := min(start+ingestProgressStep, len(texts))
		part, err := c.embedTexts(ctx, texts[start:end])
		if err != nil {
			return nil, err
		}
		embs = append(embs, part...)
		opts.OnProgress(IngestProgress{Doc: sourceURI, Embedded: len(embs), ToEmbed: len(texts), Stats: *stats})
	}
	return embs, nil
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
