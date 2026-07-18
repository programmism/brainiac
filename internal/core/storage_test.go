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

	// Probe the DENSE arm directly (c.Search is now hybrid, so its lexical half
	// would find "alpha" by text regardless of the vector). The wrong embedding is
	// orthogonal to the query, so the dense distance is far (~1) before reembed.
	qemb, _ := hashEmbedder{}.Embed(ctx, "alpha")
	before, err := store.SearchChunks(ctx, pool, qemb, 1, store.AllScopes(), store.NoWall(), false)
	if err != nil {
		t.Fatalf("search before: %v", err)
	}
	if len(before) == 0 || before[0].Distance < 0.5 {
		t.Fatalf("expected a far dense hit before reembed, got %+v", before)
	}

	n, err := c.Reembed(ctx)
	if err != nil || n != 1 {
		t.Fatalf("reembed: n=%d err=%v", n, err)
	}

	// After rebuilding the vector from the stored text, the dense distance is ~0.
	after, err := store.SearchChunks(ctx, pool, qemb, 1, store.AllScopes(), store.NoWall(), false)
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
	hits, err := c.Search(ctx, "beta", 5, "", false)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("cold chunk should not appear in search, got %d", len(hits))
	}
}
