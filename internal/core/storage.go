package core

import (
	"context"
	"fmt"

	"github.com/programmism/brainiac/internal/model"
	"github.com/programmism/brainiac/internal/store"
)

// Reembed recomputes every chunk's embedding from its stored raw text using the
// current embedder — the migration path for an embedding-model upgrade, with no
// source re-read (SYSTEM.md §13.5). Returns the number of chunks re-embedded.
func (c *Core) Reembed(ctx context.Context) (int, error) {
	chunks, err := store.AllChunkTexts(ctx, c.pool)
	if err != nil {
		return 0, err
	}
	for _, ck := range chunks {
		emb, err := c.embedder.Embed(ctx, ck.Text)
		if err != nil {
			return 0, fmt.Errorf("embed chunk %s: %w", ck.ID, err)
		}
		if err := store.UpdateChunkEmbedding(ctx, c.pool, ck.ID, emb); err != nil {
			return 0, err
		}
	}
	return len(chunks), nil
}

// SetChunkTier moves a chunk between the hot index and the cold archive (§13.4).
func (c *Core) SetChunkTier(ctx context.Context, chunkID string, tier model.Tier) error {
	if tier != model.TierHot && tier != model.TierCold {
		return fmt.Errorf("invalid tier %q", tier)
	}
	return store.SetChunkTier(ctx, c.pool, chunkID, tier)
}
