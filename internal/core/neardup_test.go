package core

import (
	"context"
	"testing"

	"github.com/programmism/brainiac/internal/model"
	"github.com/programmism/brainiac/internal/store"
)

// The same content mirrored across two sources should collapse to one result, so
// redundancy doesn't crowd out diverse evidence (#217). A genuinely different
// chunk still survives alongside it.
func TestSearchCollapsesNearDuplicates(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	const dup = "the payment retry uses exponential backoff capped at three attempts"
	for _, uri := range []string{"notion://pay", "repo://pay"} { // same text, two sources
		emb, _ := hashEmbedder{}.Embed(ctx, dup)
		if err := store.InsertChunk(ctx, pool, &model.Chunk{Text: dup, Embedding: emb, SourceURI: uri}); err != nil {
			t.Fatalf("insert %s: %v", uri, err)
		}
	}
	// A different chunk that also matches the query token "backoff".
	other := "backoff jitter avoids synchronized retries across many clients"
	oemb, _ := hashEmbedder{}.Embed(ctx, other)
	if err := store.InsertChunk(ctx, pool, &model.Chunk{Text: other, Embedding: oemb, SourceURI: "doc://jitter"}); err != nil {
		t.Fatalf("insert other: %v", err)
	}

	hits, err := c.Search(ctx, "backoff retry", 10, "")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	var dupCount int
	for _, h := range hits {
		if h.Text == dup {
			dupCount++
		}
	}
	if dupCount != 1 {
		t.Fatalf("near-duplicate not collapsed: %d copies of the mirrored chunk in %+v", dupCount, hits)
	}
}
