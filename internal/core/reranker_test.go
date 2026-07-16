package core

import (
	"context"
	"sort"
	"testing"

	"github.com/programmism/brainiac/internal/model"
	"github.com/programmism/brainiac/internal/plugins/density"
	"github.com/programmism/brainiac/internal/store"
)

// reverseReranker sorts hits by source_uri descending — a deterministic reorder
// that proves the reranker is actually applied in the retrieval path.
type reverseReranker struct{}

func (reverseReranker) Rerank(_ context.Context, _ string, hits []model.ChunkHit) ([]model.ChunkHit, error) {
	out := append([]model.ChunkHit(nil), hits...)
	sort.Slice(out, func(i, j int) bool { return out[i].SourceURI > out[j].SourceURI })
	return out, nil
}

func TestRerankerReordersResults(t *testing.T) {
	_, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	// Two chunks that both match the query lexically.
	for _, uri := range []string{"doc://a", "doc://b"} {
		emb, _ := hashEmbedder{}.Embed(ctx, "widget "+uri)
		if err := store.InsertChunk(ctx, pool, &model.Chunk{Text: "the widget report " + uri, Embedding: emb, SourceURI: uri}); err != nil {
			t.Fatalf("insert %s: %v", uri, err)
		}
	}

	withRerank := New(pool, hashEmbedder{}, density.New(), WithReranker(reverseReranker{}))
	hits, err := withRerank.Search(ctx, "widget report", 10, "")
	if err != nil || len(hits) < 2 {
		t.Fatalf("search: hits=%d err=%v", len(hits), err)
	}
	// The reranker forces descending source_uri, so doc://b comes first.
	if hits[0].SourceURI != "doc://b" {
		t.Fatalf("reranker not applied: first hit is %q, want doc://b", hits[0].SourceURI)
	}
}
