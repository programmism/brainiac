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

// Embedder produces embeddings via Ollama's /api/embeddings endpoint.
type Embedder struct {
	baseURL string
	model   string
	dims    int
	client  *http.Client
	retries int
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

// New builds an Ollama embedder for the given base URL, model, and dimension.
func New(baseURL, model string, dims int, opts ...Option) *Embedder {
	e := &Embedder{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		dims:    dims,
		client:  &http.Client{Timeout: 30 * time.Second},
		retries: DefaultRetries,
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

// Embed returns the vector for text, retrying transient failures with
// exponential backoff. A persistent failure is returned so callers may queue
// ingest (graceful degradation, §11).
func (e *Embedder) Embed(ctx context.Context, text string) ([]float32, error) {
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
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
		}
		vec, err := e.embedOnce(ctx, text)
		if err == nil {
			return vec, nil
		}
		lastErr = err
	}
	return nil, lastErr
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

var _ plugins.Embedder = (*Embedder)(nil)
