// Package ollama implements the plugins.Embedder seam against a local Ollama
// server (SYSTEM.md §3, §7.4). It is the v1 embedder; the platform is not bound
// to Ollama — any Embedder variant can replace it via config.
package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/programmism/brainiac/internal/plugins"
)

// DefaultRetries is the number of embed attempts before giving up.
const DefaultRetries = 3

// DefaultBatchSize is how many chunks EmbedBatch sends per /api/embed request.
// Kept modest so a small (e.g. 4 GB) prototype box running Ollama isn't swamped
// by a huge single request; still turns thousands of round-trips into dozens.
const DefaultBatchSize = 32

// Embedder produces embeddings via Ollama's embeddings endpoints: single via
// /api/embeddings, batched via /api/embed (#140).
type Embedder struct {
	baseURL   string
	model     string
	dims      int
	client    *http.Client
	retries   int
	batchSize int
}

// Option customizes an Embedder.
type Option func(*Embedder)

// WithHTTPClient overrides the default HTTP client (useful in tests).
func WithHTTPClient(c *http.Client) Option {
	return func(e *Embedder) { e.client = c }
}

// WithRetries sets how many attempts Embed makes on transient failures.
func WithRetries(n int) Option {
	return func(e *Embedder) { e.retries = n }
}

// WithBatchSize sets how many chunks EmbedBatch sends per request (<=0 keeps the
// default). Lets a deployment tune throughput against its Ollama box's memory.
func WithBatchSize(n int) Option {
	return func(e *Embedder) {
		if n > 0 {
			e.batchSize = n
		}
	}
}

// New builds an Ollama embedder for the given base URL, model, and dimension.
func New(baseURL, model string, dims int, opts ...Option) *Embedder {
	e := &Embedder{
		baseURL:   strings.TrimRight(baseURL, "/"),
		model:     model,
		dims:      dims,
		client:    &http.Client{Timeout: 30 * time.Second},
		retries:   DefaultRetries,
		batchSize: DefaultBatchSize,
	}
	for _, o := range opts {
		o(e)
	}
	return e
}

// Dims returns the embedding dimensionality.
func (e *Embedder) Dims() int { return e.dims }

type embedRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

type embedResponse struct {
	Embedding []float64 `json:"embedding"`
}

type embedBatchRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedBatchResponse struct {
	Embeddings [][]float64 `json:"embeddings"`
}

// withRetries runs fn, retrying transient failures with exponential backoff. A
// persistent failure is returned so callers may queue ingest (graceful
// degradation, §11).
func (e *Embedder) withRetries(ctx context.Context, fn func() error) error {
	attempts := e.retries
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(100<<uint(attempt-1)) * time.Millisecond // 100ms, 200ms, 400ms…
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}
		if err := fn(); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	return lastErr
}

// Embed returns the vector for a single text (query path).
func (e *Embedder) Embed(ctx context.Context, text string) ([]float32, error) {
	var vec []float32
	err := e.withRetries(ctx, func() error {
		v, err := e.embedOnce(ctx, text)
		if err != nil {
			return err
		}
		vec = v
		return nil
	})
	return vec, err
}

// EmbedBatch embeds many texts, sending batchSize per /api/embed request so a
// bulk import costs dozens of round-trips instead of one per chunk (#140). The
// result is aligned 1:1 with texts.
func (e *Embedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	size := e.batchSize
	if size < 1 {
		size = DefaultBatchSize
	}
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += size {
		end := start + size
		if end > len(texts) {
			end = len(texts)
		}
		batch := texts[start:end]
		var vecs [][]float32
		err := e.withRetries(ctx, func() error {
			v, err := e.embedBatchOnce(ctx, batch)
			if err != nil {
				return err
			}
			vecs = v
			return nil
		})
		if err != nil {
			return nil, err
		}
		if len(vecs) != len(batch) {
			return nil, fmt.Errorf("ollama embed: got %d vectors for %d inputs", len(vecs), len(batch))
		}
		out = append(out, vecs...)
	}
	return out, nil
}

func (e *Embedder) embedOnce(ctx context.Context, text string) ([]float32, error) {
	payload, err := json.Marshal(embedRequest{Model: e.model, Prompt: text})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+"/api/embeddings", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama embeddings request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		return nil, fmt.Errorf("ollama embeddings: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var out embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode ollama response: %w", err)
	}
	if len(out.Embedding) == 0 {
		return nil, fmt.Errorf("ollama returned an empty embedding for model %q", e.model)
	}

	vec := make([]float32, len(out.Embedding))
	for i, f := range out.Embedding {
		vec[i] = float32(f)
	}
	return vec, nil
}

func (e *Embedder) embedBatchOnce(ctx context.Context, texts []string) ([][]float32, error) {
	payload, err := json.Marshal(embedBatchRequest{Model: e.model, Input: texts})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+"/api/embed", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama embed request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		return nil, fmt.Errorf("ollama embed: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var out embedBatchResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode ollama response: %w", err)
	}
	if len(out.Embeddings) == 0 {
		return nil, fmt.Errorf("ollama returned no embeddings for model %q", e.model)
	}

	vecs := make([][]float32, len(out.Embeddings))
	for i, emb := range out.Embeddings {
		if len(emb) == 0 {
			return nil, fmt.Errorf("ollama returned an empty embedding for model %q", e.model)
		}
		vec := make([]float32, len(emb))
		for j, f := range emb {
			vec[j] = float32(f)
		}
		vecs[i] = vec
	}
	return vecs, nil
}

var (
	_ plugins.Embedder      = (*Embedder)(nil)
	_ plugins.BatchEmbedder = (*Embedder)(nil)
)
