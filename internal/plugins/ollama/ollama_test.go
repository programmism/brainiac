package ollama

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEmbed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embeddings" || r.Method != http.MethodPost {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var req embedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if req.Model != "nomic-embed-text" || req.Prompt != "hello" {
			t.Errorf("bad request payload: %+v", req)
		}
		_ = json.NewEncoder(w).Encode(embedResponse{Embedding: []float64{0.1, 0.2, 0.3}})
	}))
	defer srv.Close()

	e := New(srv.URL, "nomic-embed-text", 3)
	if e.Dims() != 3 {
		t.Fatalf("dims = %d", e.Dims())
	}
	vec, err := e.Embed(context.Background(), "hello")
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	want := []float32{0.1, 0.2, 0.3}
	if len(vec) != len(want) {
		t.Fatalf("len = %d, want %d", len(vec), len(want))
	}
	for i := range want {
		if vec[i] != want[i] {
			t.Errorf("vec[%d] = %v, want %v", i, vec[i], want[i])
		}
	}
}

func TestEmbedErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "model not found", http.StatusNotFound)
	}))
	defer srv.Close()

	e := New(srv.URL, "missing", 3)
	if _, err := e.Embed(context.Background(), "x"); err == nil {
		t.Fatal("expected error on non-200 response")
	}
}

func TestEmbedEmptyVector(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(embedResponse{Embedding: []float64{}})
	}))
	defer srv.Close()

	e := New(srv.URL, "nomic-embed-text", 3)
	if _, err := e.Embed(context.Background(), "x"); err == nil {
		t.Fatal("expected error on empty embedding")
	}
}

func TestEmbedBatch(t *testing.T) {
	var reqCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" || r.Method != http.MethodPost {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		reqCount++
		var req embedBatchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		// Echo one distinct vector per input so we can check order + alignment.
		embs := make([][]float64, len(req.Input))
		for i := range req.Input {
			embs[i] = []float64{float64(i), 0.5}
		}
		_ = json.NewEncoder(w).Encode(embedBatchResponse{Embeddings: embs})
	}))
	defer srv.Close()

	// 5 inputs with batchSize 2 → 3 requests (2+2+1), all 5 vectors returned in order.
	e := New(srv.URL, "nomic-embed-text", 2, WithBatchSize(2))
	texts := []string{"a", "b", "c", "d", "e"}
	vecs, err := e.EmbedBatch(context.Background(), texts)
	if err != nil {
		t.Fatalf("embed batch: %v", err)
	}
	if len(vecs) != len(texts) {
		t.Fatalf("got %d vectors, want %d", len(vecs), len(texts))
	}
	if reqCount != 3 {
		t.Errorf("made %d requests, want 3 (batchSize 2 over 5 inputs)", reqCount)
	}
	// Within each sub-batch the mock returns [index,0.5]; alignment must hold.
	for i, v := range vecs {
		if len(v) != 2 || v[1] != 0.5 {
			t.Fatalf("vec[%d] malformed: %v", i, v)
		}
	}
}

func TestEmbedBatchEmpty(t *testing.T) {
	e := New("http://unused", "m", 3)
	vecs, err := e.EmbedBatch(context.Background(), nil)
	if err != nil || vecs != nil {
		t.Fatalf("empty batch = (%v, %v), want (nil, nil)", vecs, err)
	}
}

func TestEmbedBatchErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	e := New(srv.URL, "m", 3, WithRetries(1))
	if _, err := e.EmbedBatch(context.Background(), []string{"x"}); err == nil {
		t.Fatal("expected error on non-200 response")
	}
}
