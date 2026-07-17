package ollama

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
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
		// Documents get the nomic document task prefix (#210).
		if req.Model != "nomic-embed-text" || req.Prompt != "search_document: hello" {
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

func TestEmbedQueryUsesQueryPrefix(t *testing.T) {
	var gotPrompt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req embedRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotPrompt = req.Prompt
		_ = json.NewEncoder(w).Encode(embedResponse{Embedding: []float64{0.1}})
	}))
	defer srv.Close()

	e := New(srv.URL, "nomic-embed-text", 1)
	if _, err := e.EmbedQuery(context.Background(), "hello"); err != nil {
		t.Fatalf("embed query: %v", err)
	}
	if gotPrompt != "search_query: hello" {
		t.Errorf("query prompt = %q, want %q", gotPrompt, "search_query: hello")
	}
	// A non-nomic model stays symmetric (no prefix).
	e2 := New(srv.URL, "some-symmetric-model", 1)
	if _, err := e2.EmbedQuery(context.Background(), "hi"); err != nil {
		t.Fatalf("embed: %v", err)
	}
	if gotPrompt != "hi" {
		t.Errorf("symmetric model prompt = %q, want %q", gotPrompt, "hi")
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

func TestMaxConcurrencyCapsInFlight(t *testing.T) {
	const limit = 2
	var (
		mu       sync.Mutex
		inFlight int
		peak     int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()
		time.Sleep(20 * time.Millisecond) // hold the slot so overlap is observable
		mu.Lock()
		inFlight--
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(embedResponse{Embedding: []float64{1, 2, 3}})
	}))
	defer srv.Close()

	e := New(srv.URL, "nomic-embed-text", 3, WithMaxConcurrency(limit))
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.Embed(context.Background(), "x"); err != nil {
				t.Errorf("embed: %v", err)
			}
		}()
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if peak > limit {
		t.Fatalf("peak in-flight embeds = %d, want <= %d", peak, limit)
	}
	if peak == 0 {
		t.Fatal("no embeds observed")
	}
}

func TestMaxConcurrencyUnsetIsUnlimited(t *testing.T) {
	e := New("http://x", "m", 3)
	release, err := e.acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	release() // must be a no-op, not panic
}
