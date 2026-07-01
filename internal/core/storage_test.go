package core

import (
	"context"
	"testing"

	"github.com/programmism/brainiac/internal/model"
	"github.com/programmism/brainiac/internal/store"
)

func TestReembedRebuildsFromText(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	// Store a chunk with a deliberately wrong embedding (from different text).
	wrong, _ := hashEmbedder{}.Embed(ctx, "totally-unrelated")
	if err := store.InsertChunk(ctx, pool, &model.Chunk{Text: "alpha", Embedding: wrong, SourceURI: "u/a"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	before, err := c.Search(ctx, "alpha", 1)
	if err != nil || len(before) == 0 {
		t.Fatalf("search before: %v", err)
	}
	if before[0].Distance < 0.5 {
		t.Fatalf("expected far distance before reembed, got %.3f", before[0].Distance)
	}

	n, err := c.Reembed(ctx)
	if err != nil || n != 1 {
		t.Fatalf("reembed: n=%d err=%v", n, err)
	}

	after, err := c.Search(ctx, "alpha", 1)
	if err != nil || len(after) == 0 {
		t.Fatalf("search after: %v", err)
	}
	if after[0].Distance > 0.01 {
		t.Fatalf("expected near-zero distance after reembed, got %.3f", after[0].Distance)
	}
}

func TestSetChunkTierExcludesFromSearch(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	emb, _ := hashEmbedder{}.Embed(ctx, "beta")
	ch := &model.Chunk{Text: "beta", Embedding: emb, SourceURI: "u/b"}
	if err := store.InsertChunk(ctx, pool, ch); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := c.SetChunkTier(ctx, ch.ID, model.TierCold); err != nil {
		t.Fatalf("set tier: %v", err)
	}
	// Cold chunks are excluded from default (hot) search.
	hits, err := c.Search(ctx, "beta", 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("cold chunk should not appear in search, got %d", len(hits))
	}
}
