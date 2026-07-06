package core

import (
	"context"
	"testing"
)

// batchSpy implements plugins.BatchEmbedder and records batch calls.
type batchSpy struct {
	batchCalls int
	singleCall int
}

func (b *batchSpy) Dims() int { return 3 }
func (b *batchSpy) Embed(_ context.Context, _ string) ([]float32, error) {
	b.singleCall++
	return []float32{1, 0, 0}, nil
}
func (b *batchSpy) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	b.batchCalls++
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{float32(i), 0, 0}
	}
	return out, nil
}

// singleOnly implements only plugins.Embedder (no batch path).
type singleOnly struct{ calls int }

func (s *singleOnly) Dims() int { return 3 }
func (s *singleOnly) Embed(_ context.Context, _ string) ([]float32, error) {
	s.calls++
	return []float32{2, 0, 0}, nil
}

func TestEmbedTextsUsesBatchWhenAvailable(t *testing.T) {
	spy := &batchSpy{}
	c := &Core{embedder: spy}
	texts := []string{"a", "b", "c"}
	embs, err := c.embedTexts(context.Background(), texts)
	if err != nil {
		t.Fatalf("embedTexts: %v", err)
	}
	if spy.batchCalls != 1 || spy.singleCall != 0 {
		t.Errorf("batch embedder should be used once, no single calls: batch=%d single=%d", spy.batchCalls, spy.singleCall)
	}
	if len(embs) != len(texts) || embs[2][0] != 2 {
		t.Errorf("result misaligned: %v", embs)
	}
}

func TestEmbedTextsFallsBackToSingle(t *testing.T) {
	s := &singleOnly{}
	c := &Core{embedder: s}
	embs, err := c.embedTexts(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatalf("embedTexts: %v", err)
	}
	if s.calls != 2 {
		t.Errorf("fallback should call Embed once per text: calls=%d", s.calls)
	}
	if len(embs) != 2 {
		t.Errorf("want 2 vectors, got %d", len(embs))
	}
}

func TestEmbedTextsEmpty(t *testing.T) {
	c := &Core{embedder: &batchSpy{}}
	embs, err := c.embedTexts(context.Background(), nil)
	if err != nil || embs != nil {
		t.Fatalf("empty = (%v, %v), want (nil, nil)", embs, err)
	}
}
