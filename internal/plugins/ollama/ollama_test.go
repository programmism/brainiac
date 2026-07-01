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
