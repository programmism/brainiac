package core

import (
	"context"
	"testing"
	"time"

	"github.com/programmism/brainiac/internal/model"
	"github.com/programmism/brainiac/internal/store"
)

// Two equally-relevant chunks (same text) differ only in source freshness; the
// recency prior must rank the fresher one first (#218).
func TestFreshnessBreaksTies(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	const text = "the widget calibration spec"
	fresh := time.Now()
	stale := time.Now().AddDate(-2, 0, 0)
	emb, _ := hashEmbedder{}.Embed(ctx, text)

	if err := store.InsertChunk(ctx, pool, &model.Chunk{Text: text, Embedding: emb, SourceURI: "doc://stale", SourceModifiedAt: &stale}); err != nil {
		t.Fatalf("insert stale: %v", err)
	}
	if err := store.InsertChunk(ctx, pool, &model.Chunk{Text: text, Embedding: emb, SourceURI: "doc://fresh", SourceModifiedAt: &fresh}); err != nil {
		t.Fatalf("insert fresh: %v", err)
	}

	hits, err := c.Search(ctx, text, 10, "")
	if err != nil || len(hits) < 2 {
		t.Fatalf("search: hits=%d err=%v", len(hits), err)
	}
	if hits[0].SourceURI != "doc://fresh" {
		t.Fatalf("fresher chunk should rank first, got %q", hits[0].SourceURI)
	}
}
