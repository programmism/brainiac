package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

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
	// Incremental skips a document whose source modification time has not advanced
	// since it was last synced (persisted per source_uri, #236) — so a periodic
	// auto-import doesn't re-chunk/re-hash unchanged files. Off by default: a
	// one-shot `import` re-reconciles fully (content-hash safety), trusting mtime
	// only when the caller opts in.
	Incremental bool
	// Trust is the provenance/trust level stamped on every chunk from this run
	// (#273): model.TrustTrusted or model.TrustUntrusted. Empty defaults to
	// untrusted (fail-closed) — the connector Ingest path is bulk external input;
	// IngestText (explicit client curation) sets it trusted. Untrusted chunks force
	// extraction through the review queue and are surfaced in retrieval results.
	Trust string
	// PruneMissing propagates source-side deletions (#247/#323): after a full
	// connector sweep, a document previously synced from this connector's scheme
	// but absent from this run is removed (its source's chunk memberships dropped,
	// then orphaned chunks pruned, and its source_sync row deleted). Off by default
	// — the #107 retention default keeps deleted content, so this is strictly
	// opt-in. Skipped entirely if the sweep had any fetch/document error, so a
	// transient failure can never be mistaken for a deletion. Scoped by URI scheme
	// so one connector's sweep never deletes another connector's documents.
	PruneMissing bool
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

// chunkLocator merges a document's source locator with this chunk's passage-level
// anchor (#243): the byte offset of its content-defined core and the nearest
// preceding Markdown heading (omitted when empty), so a citation can point at a
// section, not just the whole document. Copies base so per-chunk fields don't
// alias the shared doc locator.
func chunkLocator(base map[string]any, p chunk.Piece) map[string]any {
	loc := make(map[string]any, len(base)+2)
	for k, v := range base {
		loc[k] = v
	}
	loc["char_offset"] = p.Offset
	if p.Heading != "" {
		loc["heading"] = p.Heading
	}
	return loc
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
	Docs        int // documents fetched
	Chunks      int // chunks seen
	Kept        int // stored hot
	Queued      int // stored cold (borderline; excluded from default search)
	Dropped     int // rejected by the selector
	Skipped     int // unchanged (content hash already present for this source)
	Deduped     int // reused an existing chunk by content hash across sources (#389) — not re-embedded
	SkippedDocs int // whole documents skipped by incremental mtime check (#236)
	Deleted     int // stale chunks removed (source content edited away/removed)
	DeletedDocs int // whole documents removed by PruneMissing (source-side deletions, #247)
	Failed      int // documents that failed (e.g. embedder down) — skipped, run continues
	FetchErrors int // connector fetch/pagination errors — skipped, run continues (#241)
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
	// For opt-in deletion propagation (#247/#323): the set of documents the source
	// still has this run, and the URI schemes they belong to. A document present at
	// the source — even one that failed to ingest — is NOT a deletion, so it's
	// recorded as seen regardless of ingestDoc's outcome.
	seen := map[string]bool{}
	schemes := map[string]bool{}
	for doc, err := range conn.Fetch(ctx) {
		if err != nil {
			// A single fetch/pagination error must not abort the whole backfill
			// (#241): count it and keep consuming whatever the connector yields
			// next, so one bad page doesn't lose a large import. Only a cancelled
			// context is terminal. (Resuming an interrupted backfill rides on the
			// per-source mtime skip, #236; persisted connector cursors are #323.)
			if ctx.Err() != nil {
				return stats, ctx.Err()
			}
			stats.FetchErrors++
			continue
		}
		stats.Docs++
		if doc.SourceURI != "" {
			seen[doc.SourceURI] = true
			if s := sourceScheme(doc.SourceURI); s != "" {
				schemes[s] = true
			}
		}
		if err := c.ingestDoc(ctx, doc, opts, &stats); err != nil {
			stats.Failed++ // skip this doc, keep going
			continue
		}
	}

	// Propagate source-side deletions only on an explicit opt-in and a CLEAN sweep:
	// any fetch or document error means we can't tell "deleted" from "temporarily
	// unavailable", so we keep everything (fail-safe — never delete on doubt).
	if opts.PruneMissing && !opts.DryRun && stats.FetchErrors == 0 && stats.Failed == 0 {
		if err := c.pruneMissingDocs(ctx, seen, schemes, &stats); err != nil {
			return stats, err
		}
	}
	return stats, nil
}

// sourceScheme returns the "scheme://" prefix of a source URI (e.g.
// "markdown://docs/a.md" -> "markdown://"), or "" if it has none. Deletion
// propagation is scoped by this prefix so one connector's sweep never removes
// another connector's documents (#323).
func sourceScheme(uri string) string {
	if i := strings.Index(uri, "://"); i > 0 {
		return uri[:i+len("://")]
	}
	return ""
}

// pruneMissingDocs removes documents that were previously synced under one of the
// swept schemes but are absent from this run — the opt-in deletion-propagation
// path (#247/#323). Deletion is membership-based (#387): a document's chunks are
// removed only when this source was their last claim, so content another source
// still vouches for survives; the document's source_sync row is deleted so a
// later run doesn't resurrect the skip. Each document is its own transaction.
func (c *Core) pruneMissingDocs(ctx context.Context, seen, schemes map[string]bool, stats *IngestStats) error {
	for scheme := range schemes {
		uris, err := store.SourceSyncURIsWithScheme(ctx, c.pool, scheme)
		if err != nil {
			return err
		}
		for _, uri := range uris {
			if seen[uri] {
				continue
			}
			deleted, err := c.propagateDelete(ctx, uri)
			if err != nil {
				return err
			}
			stats.Deleted += int(deleted)
			stats.DeletedDocs++
		}
	}
	return nil
}

// propagateDelete removes one document that no longer exists at its source: drop
// this source's chunk memberships, prune chunks whose last source is now gone
// (#387), and delete its source_sync row so a later run doesn't resurrect the skip.
// One transaction. Returns how many chunks were actually deleted (0 if every chunk
// is still vouched for by another source). Shared by the full-sweep prune
// (#247) and the Watch()-driven delete stream (#323).
func (c *Core) propagateDelete(ctx context.Context, uri string) (int64, error) {
	var deleted int64
	err := store.WithTx(ctx, c.pool, func(db store.DBTX) error {
		if _, err := store.DropChunkSourceMembershipNotIn(ctx, db, uri, nil); err != nil {
			return err
		}
		d, err := store.PruneOrphanChunks(ctx, db)
		if err != nil {
			return err
		}
		deleted = d
		return store.DeleteSourceSync(ctx, db, uri)
	})
	return deleted, err
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
	// Ingested content is untrusted by default (#273): it's external text (a page
	// Claude read, a connector's docs), the indirect-injection surface. Trusted is
	// opt-in via IngestOptions.Trust (wired per-source in the follow-up), so recall
	// treats content as untrusted until an operator vouches for its source.
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
	// Incremental sync (#236): skip a document whose source modification time has
	// not advanced since it was last synced — no chunking, hashing, or embedding.
	// Never skips on a DryRun (which must report the true reconcile) or when the
	// source's mtime is unknown.
	if opts.Incremental && doc.ModifiedAt != nil && !opts.DryRun {
		if prev, ok, serr := store.SourceSyncModifiedAt(ctx, c.pool, doc.SourceURI); serr == nil && ok && !doc.ModifiedAt.After(prev) {
			stats.SkippedDocs++
			return nil
		}
	}

	disc, err := c.pinWrite(ctx, discFromProject(opts.Project))
	if err != nil {
		return err
	}
	// Trust posture for this document (#273): empty defaults to untrusted, so any
	// path that forgets to set it fails closed. Untrusted forces extraction review.
	trust := opts.Trust
	if trust == "" {
		trust = model.TrustUntrusted
	}
	pieces := chunk.SplitWithProvenance(normalizeText(doc.Text))
	chunks := make([]string, len(pieces))
	locators := make([]map[string]any, len(pieces))
	hashes := make([]string, len(pieces))
	for i, p := range pieces {
		chunks[i] = p.Text
		locators[i] = chunkLocator(doc.SourceLocator, p) // per-chunk passage anchor (#243)
		hashes[i] = hashText(p.Text)
	}

	// Membership-based "already have it" set (#389): hashes this source vouches
	// for, including chunks it shares (deduped) with other sources — so a re-ingest
	// skips them. For a single-source chunk this equals the old per-source hash set.
	existing, err := store.SourceMemberHashes(ctx, c.pool, doc.SourceURI)
	if err != nil {
		return err
	}

	// Pass 1: decide skip/link/drop/keep per chunk and collect the ones that need
	// embedding, so they can be embedded in one batch instead of one round-trip
	// each (embedding dominates bulk-ingest cost — #140).
	type pending struct {
		text    string
		hash    string
		tier    model.Tier
		quality float64
		locator map[string]any
	}
	var toEmbed []pending
	var toLink []string // ids of existing chunks this source should also vouch for (#389)
	skipped, dropped, kept, queued, deduped := 0, 0, 0, 0, 0
	for i, ck := range chunks {
		if existing[hashes[i]] {
			skipped++ // unchanged — this source already vouches for this content
			continue
		}
		// Global content dedup (#389): identical content already stored in the SAME
		// scope and trust level is reused — record this source's membership and skip
		// re-embedding and re-storing it. Scoping by scope_key keeps content inside
		// the per-project isolation wall (#120); scoping by trust keeps untrusted
		// content from inheriting a trusted chunk (#273). The chunk survives until
		// its LAST source drops it.
		if id, ok, err := store.ChunkIDByHashScoped(ctx, c.pool, hashes[i], model.ScopeKey(disc), trust); err != nil {
			return err
		} else if ok {
			toLink = append(toLink, id)
			deduped++
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
		toEmbed = append(toEmbed, pending{text: ck, hash: hashes[i], tier: tier, quality: score.Quality, locator: locators[i]})
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
		stats.Deduped += deduped
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
			Text: p.text, Embedding: emb, SourceURI: doc.SourceURI, SourceLocator: p.locator,
			QualityScore: p.quality, Tier: p.tier, ContentHash: p.hash, SourceModifiedAt: doc.ModifiedAt,
			Discriminators: disc, Trust: trust,
		})
	}

	var deleted int64
	err = store.WithTx(ctx, c.pool, func(db store.DBTX) error {
		// Membership-based reconcile (#244/#387): drop this source's claim on the
		// content it no longer carries, then delete only the chunks whose LAST
		// source is now gone. For a single-source chunk this is identical to the old
		// per-source delete (its sole membership drops → it's orphaned → pruned); for
		// content another source still vouches for, the chunk survives.
		if _, err := store.DropChunkSourceMembershipNotIn(ctx, db, doc.SourceURI, hashes); err != nil {
			return err
		}
		d, err := store.PruneOrphanChunks(ctx, db)
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
		// Global dedup (#389): this source also vouches for content already stored
		// under another source — record membership without re-storing the chunk.
		for _, id := range toLink {
			if err := store.RecordChunkSource(ctx, db, id, doc.SourceURI); err != nil {
				return err
			}
		}
		// Record the sync point so a later incremental run can skip this document
		// if its source hasn't advanced (#236). Atomic with the reconcile above.
		return store.UpsertSourceSync(ctx, db, doc.SourceURI, doc.ModifiedAt)
	})
	if err != nil {
		return err
	}

	stats.Chunks += len(chunks)
	stats.Skipped += skipped
	stats.Deduped += deduped
	stats.Dropped += dropped
	stats.Kept += kept
	stats.Queued += queued
	stats.Deleted += int(deleted)
	c.ingestedChunks.Add(uint64(len(inserts))) // process-lifetime throughput counter (#319)

	// Optional Layer-2 extraction: derive nodes/edges from the freshly-stored
	// hot chunks. Runs after the chunk reconcile (chunks are provenance and must
	// persist even if extraction fails), best-effort per chunk so one bad chunk
	// never fails the whole document.
	if c.extractor != nil {
		for _, p := range toEmbed {
			if p.tier != model.TierHot {
				continue // extract from kept knowledge only, not the cold queue
			}
			// Untrusted content never auto-writes live graph facts — force it
			// through the review queue regardless of extraction.review (#273).
			forceReview := trust == model.TrustUntrusted
			n, e, err := c.extractChunk(ctx, p.text, doc.SourceURI, disc, forceReview)
			if err != nil {
				stats.ExtractFailed++
				c.extractFailures.Add(1) // process-lifetime extraction-failure counter (#319)
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
