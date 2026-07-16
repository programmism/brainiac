package core

import (
	"context"
	"fmt"

	"github.com/programmism/brainiac/internal/model"
	"github.com/programmism/brainiac/internal/plugins"
	"github.com/programmism/brainiac/internal/store"
)

// reembedBatch is how many chunks are embedded per round when the embedder
// supports batching — dozens of round-trips instead of one per chunk (#219).
const reembedBatch = 128

// Reembed recomputes every chunk's embedding from its stored raw text using the
// current embedder — the migration path for an embedding-model upgrade, with no
// source re-read (SYSTEM.md §13.5). Embeds in batches via the batch embedder when
// available, and commits each batch, so a failure mid-run keeps the batches
// already done (re-running is idempotent — #219). Returns the count re-embedded.
func (c *Core) Reembed(ctx context.Context) (int, error) {
	chunks, err := store.AllChunkTexts(ctx, c.pool)
	if err != nil {
		return 0, err
	}
	batcher, canBatch := c.embedder.(plugins.BatchEmbedder)
	updated := 0
	for start := 0; start < len(chunks); start += reembedBatch {
		end := start + reembedBatch
		if end > len(chunks) {
			end = len(chunks)
		}
		batch := chunks[start:end]

		var embs [][]float32
		if canBatch {
			texts := make([]string, len(batch))
			for i, ck := range batch {
				texts[i] = ck.Text
			}
			embs, err = batcher.EmbedBatch(ctx, texts)
			if err != nil {
				return updated, fmt.Errorf("embed batch: %w", err)
			}
		} else {
			embs = make([][]float32, len(batch))
			for i, ck := range batch {
				if embs[i], err = c.embedder.Embed(ctx, ck.Text); err != nil {
					return updated, fmt.Errorf("embed chunk %s: %w", ck.ID, err)
				}
			}
		}
		for i, ck := range batch {
			if err := store.UpdateChunkEmbedding(ctx, c.pool, ck.ID, embs[i]); err != nil {
				return updated, err
			}
			updated++
		}
	}
	return updated, nil
}

// SetChunkTier moves a chunk between the hot index and the cold archive (§13.4).
func (c *Core) SetChunkTier(ctx context.Context, chunkID string, tier model.Tier) error {
	if tier != model.TierHot && tier != model.TierCold {
		return fmt.Errorf("invalid tier %q", tier)
	}
	return store.SetChunkTier(ctx, c.pool, chunkID, tier)
}
