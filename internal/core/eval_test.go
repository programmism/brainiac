package core

import (
	"context"
	"testing"

	"github.com/programmism/brainiac/internal/model"
	"github.com/programmism/brainiac/internal/store"
)

func TestEvalRecallAtK(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	// One chunk whose text is the query; source "doc://a".
	emb, _ := hashEmbedder{}.Embed(ctx, "orders use kafka")
	if err := store.InsertChunk(ctx, pool, &model.Chunk{Text: "orders use kafka", Embedding: emb, SourceURI: "doc://a"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	golden := []GoldenQuery{
		{Query: "orders use kafka", ExpectedSources: []string{"doc://a"}},       // hit
		{Query: "orders use kafka", ExpectedSources: []string{"doc://missing"}}, // miss
	}
	res, err := c.Eval(ctx, golden, 8)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if res.RecallAtK != 0.5 {
		t.Errorf("recall@8 = %.2f, want 0.5", res.RecallAtK)
	}
	if !res.PerQuery[0].Hit || res.PerQuery[1].Hit {
		t.Errorf("per-query hits = %+v", res.PerQuery)
	}
}
