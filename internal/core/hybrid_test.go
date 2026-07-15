package core

import (
	"context"
	"testing"

	"github.com/programmism/brainiac/internal/model"
	"github.com/programmism/brainiac/internal/store"
)

// The lexical arm must surface an exact token the dense arm misses (#211). Under
// the test hashEmbedder, a query embeds orthogonally to any non-identical chunk,
// so dense alone returns nothing for a keyword query — only FTS can find it.
func TestHybridSearchFindsExactTokenDenseMisses(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	const text = "the widget assembler emits error ORD500 on overflow"
	emb, _ := hashEmbedder{}.Embed(ctx, text)
	if err := store.InsertChunk(ctx, pool, &model.Chunk{Text: text, Embedding: emb, SourceURI: "doc://ord"}); err != nil {
		t.Fatalf("insert chunk: %v", err)
	}

	// A keyword query that shares no whole-text identity with the chunk: dense
	// distance is ~1 (filtered), but FTS matches the token.
	hits, err := c.Search(ctx, "ORD500", 10, "")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	var found bool
	for _, h := range hits {
		if h.SourceURI == "doc://ord" {
			found = true
		}
	}
	if !found {
		t.Fatalf("hybrid search missed the exact-token chunk (lexical arm not working): %+v", hits)
	}
}
