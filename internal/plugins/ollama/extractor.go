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

// Extractor turns a text chunk into graph nodes/edges by prompting a local
// Ollama chat model with a JSON schema (Ollama structured outputs). It is the
// optional server-side Extractor for bulk ingest (SYSTEM.md §7): the default
// path stays chat-driven (Claude supplies the Extraction), so a weak box pays
// nothing; a beefy self-hosted box can opt in via config. Quality is lower than
// Claude, which is why extracted nodes/edges default to the review queue.
type Extractor struct {
	baseURL string
	model   string
	client  *http.Client
	retries int
}

// ExtractorOption customizes an Extractor.
type ExtractorOption func(*Extractor)

// WithExtractorHTTPClient overrides the default HTTP client (useful in tests).
func WithExtractorHTTPClient(c *http.Client) ExtractorOption {
	return func(e *Extractor) { e.client = c }
}

// WithExtractorRetries sets how many attempts Extract makes on transient
// failures (<=0 keeps the default).
func WithExtractorRetries(n int) ExtractorOption {
	return func(e *Extractor) {
		if n > 0 {
			e.retries = n
		}
	}
}

// NewExtractor builds an Ollama-backed Extractor for the given base URL and chat
// model. Extraction can be slow on a small box, so the timeout is generous.
func NewExtractor(baseURL, model string, opts ...ExtractorOption) *Extractor {
	e := &Extractor{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		client:  &http.Client{Timeout: 5 * time.Minute},
		retries: DefaultRetries,
	}
	for _, o := range opts {
		o(e)
	}
	return e
}

// extractionSchema is the JSON schema Ollama is asked to conform its output to,
// mirroring plugins.Extraction. Structured outputs make parsing deterministic:
// the model must return exactly this shape.
var extractionSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"entities": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":    map[string]any{"type": "string"},
					"type":    map[string]any{"type": "string"},
					"aliases": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				},
				"required": []string{"name"},
			},
		},
		"relations": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"from": map[string]any{"type": "string"},
					"type": map[string]any{"type": "string"},
					"to":   map[string]any{"type": "string"},
					"why":  map[string]any{"type": "string"},
				},
				"required": []string{"from", "type", "to"},
			},
		},
	},
	"required": []string{"entities", "relations"},
}

const extractSystemPrompt = `You extract a knowledge graph from a text chunk for an engineering-team memory.
Return entities (people, services, systems, decisions, concepts) and the relations between them.
For every relation, put the rationale ("why") in the why field when the text gives one — this memory
records decisions, not just facts. Use names exactly as they appear. Do not invent facts not supported
by the text. If the chunk carries no durable knowledge, return empty arrays.`

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model    string         `json:"model"`
	Messages []chatMessage  `json:"messages"`
	Stream   bool           `json:"stream"`
	Format   any            `json:"format,omitempty"`
	Options  map[string]any `json:"options,omitempty"`
}

type chatResponse struct {
	Message chatMessage `json:"message"`
}

// rawExtraction matches extractionSchema; it is decoded from the model's JSON
// message content and mapped to plugins.Extraction.
type rawExtraction struct {
	Entities []struct {
		Name    string   `json:"name"`
		Type    string   `json:"type"`
		Aliases []string `json:"aliases"`
	} `json:"entities"`
	Relations []struct {
		From string `json:"from"`
		Type string `json:"type"`
		To   string `json:"to"`
		Why  string `json:"why"`
	} `json:"relations"`
}

// Extract prompts the chat model and returns the structured Extraction. A
// persistent failure is returned so the caller can degrade gracefully (skip
// extraction for the chunk, keep the chunk) rather than fail the ingest (§11).
func (e *Extractor) Extract(ctx context.Context, chunk string) (plugins.Extraction, error) {
	var ext plugins.Extraction
	err := e.withRetries(ctx, func() error {
		v, err := e.extractOnce(ctx, chunk)
		if err != nil {
			return err
		}
		ext = v
		return nil
	})
	return ext, err
}

// withRetries mirrors the embedder's backoff, kept separate so the two Ollama
// clients tune independently.
func (e *Extractor) withRetries(ctx context.Context, fn func() error) error {
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
		if err := fn(); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return lastErr
}

func (e *Extractor) extractOnce(ctx context.Context, chunk string) (plugins.Extraction, error) {
	payload, err := json.Marshal(chatRequest{
		Model: e.model,
		Messages: []chatMessage{
			{Role: "system", Content: extractSystemPrompt},
			{Role: "user", Content: chunk},
		},
		Stream:  false,
		Format:  extractionSchema,
		Options: map[string]any{"temperature": 0},
	})
	if err != nil {
		return plugins.Extraction{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+"/api/chat", bytes.NewReader(payload))
	if err != nil {
		return plugins.Extraction{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return plugins.Extraction{}, fmt.Errorf("ollama chat request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		return plugins.Extraction{}, fmt.Errorf("ollama chat: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var out chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return plugins.Extraction{}, fmt.Errorf("decode ollama chat response: %w", err)
	}

	var raw rawExtraction
	if err := json.Unmarshal([]byte(out.Message.Content), &raw); err != nil {
		return plugins.Extraction{}, fmt.Errorf("parse extraction JSON from model %q: %w", e.model, err)
	}
	return toExtraction(raw), nil
}

// toExtraction maps the schema-shaped result to plugins types, dropping entities
// and relations that lack the fields the graph needs (an entity with no name,
// a relation missing an endpoint or type) rather than storing garbage.
func toExtraction(raw rawExtraction) plugins.Extraction {
	var ext plugins.Extraction
	for _, en := range raw.Entities {
		name := strings.TrimSpace(en.Name)
		if name == "" {
			continue
		}
		ext.Entities = append(ext.Entities, plugins.Entity{
			Name:    name,
			Type:    strings.TrimSpace(en.Type),
			Aliases: en.Aliases,
		})
	}
	for _, r := range raw.Relations {
		from, to, typ := strings.TrimSpace(r.From), strings.TrimSpace(r.To), strings.TrimSpace(r.Type)
		if from == "" || to == "" || typ == "" {
			continue
		}
		ext.Relations = append(ext.Relations, plugins.Relation{
			From: from,
			Type: typ,
			To:   to,
			Why:  strings.TrimSpace(r.Why),
		})
	}
	return ext
}

var _ plugins.Extractor = (*Extractor)(nil)
